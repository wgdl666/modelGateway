package integration

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
)

func TestReadinessLogsMissingSchemaWithoutBypassingHealth(t *testing.T) {
	tasks, _, _ := stores(t)
	conn, err := pgx.Connect(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if conn.Config().RuntimeParams["search_path"] == "" {
		if _, err := conn.Exec(t.Context(), "SET search_path TO model_gateway"); err != nil {
			t.Fatal(err)
		}
	}
	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(tasks, runtime, nil))
	output, err := os.CreateTemp(t.TempDir(), "readiness-log-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(output, nil)))
	defer slog.SetDefault(previous)
	// 每个依赖字段都必须阻止接流量；真实 PG 错误应保留 SQLSTATE 与检查项，响应保持脱敏。
	for _, field := range []struct{ table, column string }{
		{"resource_groups", "capacity_units"}, {"model_instances", "protocol"}, {"model_instances", "deleted_at"},
		{"requests", "protocol"}, {"requests", "queue_publish_at"}, {"requests", "resource_groups"},
		{"requests", "dispatch_attempts"}, {"requests", "dispatch_retry_reason"}, {"requests", "input_media"},
	} {
		t.Run(field.table+"."+field.column, func(t *testing.T) {
			if err := output.Truncate(0); err != nil {
				t.Fatal(err)
			}
			if _, err := output.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			prefix := "ALTER TABLE " + pgx.Identifier{field.table}.Sanitize() + " RENAME COLUMN "
			column := pgx.Identifier{field.column}.Sanitize()
			if _, err := conn.Exec(t.Context(), prefix+column+" TO logfire_missing"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := conn.Exec(t.Context(), prefix+"logfire_missing TO "+column); err != nil {
					t.Error(err)
				}
			}()
			res, err := http.Get(base + "/_gateway/ready")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != 503 || !strings.Contains(string(body), "database_unavailable") || strings.Contains(string(body), "logfire_missing") {
				t.Fatalf("readiness must reject without exposing the query: %d %s", res.StatusCode, body)
			}
			raw, err := os.ReadFile(output.Name())
			var record map[string]any
			parseErr := json.Unmarshal(raw, &record)
			check, _ := record["check"].(string)
			if err != nil || parseErr != nil || record["msg"] != "gateway readiness failed" || record["db.sqlstate"] != "42703" || !strings.Contains(check, field.column) {
				t.Fatalf("missing readiness diagnostic: %s (%v)", raw, err)
			}
		})
	}
	res, err := http.Get(base + "/_gateway/ready")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("restored schema must become ready: %d", res.StatusCode)
	}
}
