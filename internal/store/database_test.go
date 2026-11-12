package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/ent"
)

func testDriver(t *testing.T) *observedDriver {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatal("test database required")
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { db.Close() })
	return &observedDriver{Driver: sql.OpenDB(dialect.Postgres, db)}
}

func TestSerializeLimitsTransactionsBeforeTakingConnections(t *testing.T) {
	d := testDriver(t)
	s := New(ent.NewClient(ent.Driver(d)))
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			err := s.serialize(t.Context(), func(*ent.Tx) error {
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				time.Sleep(10 * time.Millisecond)
				active.Add(-1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("concurrent serializable transactions: %d", peak.Load())
	}
	if d.DB().Stats().WaitCount != 0 {
		t.Fatal("admission gate waited inside SQL connection pool")
	}
	// 已排队的取消请求不能取得连接或执行事务体。
	s.serializeGate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := s.serialize(ctx, func(*ent.Tx) error { t.Error("canceled waiter executed"); return nil })
	<-s.serializeGate
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error: %v", err)
	}
}

func TestBeginTxReportsActualPoolWaiting(t *testing.T) {
	d := testDriver(t)
	d.DB().SetMaxOpenConns(1)
	held, err := d.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	output, err := os.CreateTemp(t.TempDir(), "pool-log")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(output, nil)))
	defer slog.SetDefault(previous)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err = d.Tx(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool wait: %v", err)
	}
	raw, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err = json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event["db.phase"] != "begin_tx" || event["db.pool.wait_count_delta"] != float64(1) || event["db.pool.wait_duration_delta_ms"].(float64) < 20 {
		t.Fatalf("missing pool diagnostic: %s", raw)
	}
	if strings.Contains(string(raw), os.Getenv("TEST_DATABASE_URL")) {
		t.Fatal("database URL logged")
	}
}
