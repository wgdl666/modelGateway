package store

import (
	"context"
	stdsql "database/sql"
	"errors"
	"log/slog"
	"runtime"
	"time"

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
	return ent.NewClient(ent.Driver(&observedDriver{Driver: sql.OpenDB(dialect.Postgres, db)})), nil
}

// observedDriver 在 SQL 发出前记录取连接和 BEGIN 的总耗时，不把整个阶段误称为连接池等待。
type observedDriver struct{ *sql.Driver }

func (d *observedDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	return d.BeginTx(ctx, nil)
}

func (d *observedDriver) BeginTx(ctx context.Context, opts *stdsql.TxOptions) (dialect.Tx, error) {
	before := d.DB().Stats()
	started := time.Now()
	tx, err := d.Driver.BeginTx(ctx, opts)
	elapsed := time.Since(started)
	if err != nil || elapsed >= 100*time.Millisecond {
		after := d.DB().Stats()
		// 池计数是此时间段内所有调用的增量，不能当成该事务独占的等待时间。
		attrs := []any{"db.phase", "begin_tx", "db.begin_tx_ms", float64(elapsed) / float64(time.Millisecond),
			"db.pool.wait_count_delta", after.WaitCount - before.WaitCount,
			"db.pool.wait_duration_delta_ms", float64(after.WaitDuration-before.WaitDuration) / float64(time.Millisecond),
			"db.pool.in_use_before", before.InUse, "db.pool.in_use_after", after.InUse, "db.pool.max_open", after.MaxOpenConnections}
		if err != nil {
			telemetry.LogError(ctx, "gateway database transaction start failed", err, attrs...)
		} else {
			slog.InfoContext(ctx, "gateway database transaction start delayed", attrs...)
		}
	}
	return tx, err
}
