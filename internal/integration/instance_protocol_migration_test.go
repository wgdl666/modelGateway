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

func TestInstanceProtocolMigrationPreservesRunningRequests(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatal("TEST_DATABASE_URL must point to a _test database")
	}
	ctx := t.Context()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	namespace := "protocol_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	schema := pgx.Identifier{namespace}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanup, "ROLLBACK")
		_, _ = conn.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(cleanup)
	})
	exec := func(query string) {
		t.Helper()
		if _, err := conn.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA " + schema)
	exec("SET search_path TO " + schema)
	// 复现迁移前没有协议列、但仍有执行中占用的历史表，避免新建 Ent 表掩盖回填问题。
	exec(`CREATE TABLE model_instances (model_key text, endpoint text);
		CREATE TABLE requests (model_key text, state text, slot_reserved boolean, endpoint text, backend_task_id text);
		INSERT INTO model_instances VALUES ('Vmind_dream_v1', 'http://registered'), ('unmapped', 'http://unknown');
		INSERT INTO requests VALUES ('Vmind_outfit_video_v1.0', 'processing', true, 'http://original/gpu1', 'original-job');`)
	raw, err := os.ReadFile("../../deploy/migrations/instance-protocol.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.ReplaceAll(string(raw), "model_gateway.", schema+".")
	if _, err := conn.Exec(ctx, migration); err == nil || !strings.Contains(err.Error(), "unmapped model_key") {
		t.Fatalf("unknown registration must block the migration: %v", err)
	}
	exec("ROLLBACK")
	var columns int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND column_name='protocol'", namespace).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("failed migration left partial schema: %d %v", columns, err)
	}
	exec("DELETE FROM model_instances WHERE model_key='unmapped'")
	exec(migration)
	exec(migration)
	var kind, state, endpoint, job string
	var reserved bool
	if err := conn.QueryRow(ctx, "SELECT protocol FROM model_instances").Scan(&kind); err != nil || kind != "chat_completions" {
		t.Fatalf("synchronous registration was not backfilled: %s %v", kind, err)
	}
	// 回填只能增加协议快照，不能释放 GPU、改终态或让回查丢失原执行端。
	if err := conn.QueryRow(ctx, "SELECT protocol, state, slot_reserved, endpoint, backend_task_id FROM requests").Scan(&kind, &state, &reserved, &endpoint, &job); err != nil ||
		kind != "outfit_video" || state != "processing" || !reserved || endpoint != "http://original/gpu1" || job != "original-job" {
		t.Fatalf("migration changed running request ownership: %s %s %v %s %s %v", kind, state, reserved, endpoint, job, err)
	}
}
