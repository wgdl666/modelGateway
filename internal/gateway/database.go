package gateway

import (
	"context"
	"errors"
	"runtime"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/ent"
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
	client := ent.NewClient(ent.Driver(sql.OpenDB(dialect.Postgres, db)))
	if err := db.PingContext(ctx); err != nil {
		_ = client.Close()
		return nil, errors.New("cannot connect to configured PostgreSQL")
	}
	return client, nil
}
