package integration

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAPITokenMigration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatal("use a _test database")
	}
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"tokens_" + strings.ReplaceAll(uuid.NewString(), "-", "")}.Sanitize()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(ctx, "ROLLBACK")
		_, _ = conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		_ = conn.Close(ctx)
	})
	exec := func(q string) {
		t.Helper()
		if _, err := conn.Exec(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE SCHEMA " + schema)
	exec("CREATE TABLE " + schema + ".requests (request_id text, state text); INSERT INTO " + schema + ".requests VALUES ('existing','processing')")
	raw, err := os.ReadFile("../../deploy/migrations/api-tokens.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.ReplaceAll(string(raw), "model_gateway.", schema+".")
	exec(migration)
	exec(migration)
	var state, id string
	if err := conn.QueryRow(t.Context(), "SELECT state,api_token_id FROM "+schema+".requests WHERE request_id='existing'").Scan(&state, &id); err != nil || state != "processing" || id != "" {
		t.Fatal("migration changed existing task")
	}
	exec("INSERT INTO " + schema + ".api_tokens (id,name,encrypted_token,created_by,created_at,updated_at) VALUES ('test','caller','ciphertext','tester','2020-01-01','2020-01-01')")
	exec("UPDATE " + schema + ".api_tokens SET deleted_at=now() WHERE id='test'")
	var valid bool
	if err := conn.QueryRow(t.Context(), "SELECT created_at='2020-01-01' AND updated_at>created_at AND deleted_at IS NOT NULL FROM "+schema+".api_tokens WHERE id='test'").Scan(&valid); err != nil || !valid {
		t.Fatal("timestamps or revoke not preserved")
	}
}
