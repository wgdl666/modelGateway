package gateway

import (
	"context"
	"errors"
	"runtime"

	"ariga.io/atlas/sql/postgres"
	atlasschema "ariga.io/atlas/sql/schema"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/ent"
)

// OpenDatabase 统一创建 Ent 客户端；仅显式初始化时建表，普通启动和热加载不执行 DDL。
func OpenDatabase(ctx context.Context, dsn string, initialize bool) (*ent.Client, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL configuration")
	}
	namespace := cfg.RuntimeParams["search_path"]
	if namespace == "" {
		namespace = "model_gateway"
		cfg.RuntimeParams["search_path"] = namespace
	}
	db := stdlib.OpenDB(*cfg)
	// 延续原 pgxpool 的连接上限，避免换为 Ent 后由标准连接池无限扩张连接数。
	db.SetMaxOpenConns(max(4, runtime.NumCPU()))
	client := ent.NewClient(ent.Driver(sql.OpenDB(dialect.Postgres, db)))
	if err := db.PingContext(ctx); err != nil {
		_ = client.Close()
		return nil, errors.New("cannot connect to configured PostgreSQL")
	}
	if initialize {
		// 同一 schema 的两个发布 Job 可能重叠，事务锁覆盖 schema 创建和 Ent 建表。
		lock, err := db.BeginTx(ctx, nil)
		if err != nil {
			_ = client.Close()
			return nil, err
		}
		defer lock.Rollback()
		if _, err := lock.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1)::bigint)", "model-gateway:"+namespace); err != nil {
			_ = lock.Rollback()
			_ = client.Close()
			return nil, err
		}
		// 命名空间和表结构均交给 Atlas/Ent 生成，业务启动不读取或执行手写 SQL 文件。
		driver, err := postgres.Open(db)
		if err == nil {
			err = driver.ApplyChanges(ctx, []atlasschema.Change{&atlasschema.AddSchema{
				S: atlasschema.New(namespace), Extra: []atlasschema.Clause{&atlasschema.IfNotExists{}},
			}})
		}
		if err == nil {
			err = client.Schema.Create(ctx, entschema.WithSchemaName(namespace), entschema.WithForeignKeys(false))
		}
		if err != nil {
			_ = lock.Rollback()
			_ = client.Close()
			return nil, err
		}
		if err := lock.Commit(); err != nil {
			_ = client.Close()
			return nil, err
		}
	}
	return client, nil
}
