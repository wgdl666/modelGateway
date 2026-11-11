package store

import (
	"context"
	"errors"
	"runtime"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/telemetry"
)

// OpenDatabase 连接 PostgreSQL，库表由数据库发布流程提前准备。
func OpenDatabase(ctx context.Context, dsn string) (*ent.Client, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL configuration")
	}
	if cfg.RuntimeParams["search_path"] == "" {
		// 国内 ACK 共用 wghub 数据库，通过独立 schema 定位网关业务表。
		cfg.RuntimeParams["search_path"] = "model_gateway"
	}
	db := stdlib.OpenDB(*cfg)
	// 延续原 pgxpool 的连接上限，避免换为 Ent 后由标准连接池无限扩张连接数。
	db.SetMaxOpenConns(max(4, runtime.NumCPU()))
	// 旧表仍可能有同键不同业务的登记。迁移前禁止启动消费者，避免误派。
	var legacySchema bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_attribute
		WHERE attrelid IN (to_regclass('model_instances'), to_regclass('requests'))
			AND attname = 'capability' AND NOT attisdropped
	)`).Scan(&legacySchema); err != nil {
		telemetry.LogError(ctx, "gateway database connection failed", err)
		_ = db.Close()
		return nil, errors.New("cannot connect to configured PostgreSQL")
	}
	if legacySchema {
		_ = db.Close()
		return nil, errors.New("apply the model-key-only database migration before starting the gateway")
	}
	return ent.NewClient(ent.Driver(sql.OpenDB(dialect.Postgres, db))), nil
}
