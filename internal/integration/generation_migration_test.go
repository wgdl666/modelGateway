package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestGenerationRetryMigrationPreservesHistory(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatal("use a _test database")
	}
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"generation_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(ctx, "ROLLBACK")
		_, _ = conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(ctx)
	})
	exec := func(query string) {
		t.Helper()
		if _, err := conn.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA " + schema)
	exec("CREATE TABLE " + schema + `.requests (request_id text, state text, slot_reserved boolean, result jsonb);
        INSERT INTO ` + schema + `.requests VALUES ('old', 'succeeded', false, '{"files":[]}'), ('running','processing',true,null)`)
	raw, err := os.ReadFile("../../deploy/migrations/generation-retries.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.ReplaceAll(string(raw), "model_gateway.", schema+".")
	exec(migration)
	exec("UPDATE " + schema + ".requests SET dispatch_attempts=3, dispatch_retry_reason='worker_busy_before_execution' WHERE request_id='running'")
	exec(migration)
	var count int
	err = conn.QueryRow(t.Context(), "SELECT count(*) FROM "+schema+`.requests WHERE
        (request_id='old' AND dispatch_attempts=0 AND result='{"files":[]}'::jsonb) OR
        (request_id='running' AND state='processing' AND slot_reserved AND dispatch_attempts=3 AND dispatch_retry_reason='worker_busy_before_execution')`).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("migration changed history or execution ownership: %d %v", count, err)
	}
}
