package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ariga.io/atlas/sql/postgres"
	atlasschema "ariga.io/atlas/sql/schema"
	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/internal/store"
	"github.com/wgdl666/modelGateway/internal/testsupport"
)

func TestConfigCenterReloadsRunningServer(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for PostgreSQL integration test")
	}
	mqURL, queue, _ := testsupport.RabbitQueue(t)
	t.Setenv("MODEL_GATEWAY_RABBITMQ_URL", mqURL)
	t.Setenv("XX_WG_REGION", "US")
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(u.Path, "_test") {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URI for a _test database")
	}
	// 使用独立命名空间，避免与并行运行的容量和任务测试互相清理数据。
	namespace := fmt.Sprintf("gateway_reload_%d", time.Now().UnixNano())
	query := u.Query()
	query.Set("search_path", namespace)
	u.RawQuery = query.Encode()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*cfg)
	defer admin.Close()
	driver, err := postgres.Open(admin)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟数据库发布先准备库表，再启动网关验证就绪和热加载。
	schema := atlasschema.New(namespace)
	if err := driver.ApplyChanges(t.Context(), []atlasschema.Change{&atlasschema.AddSchema{S: schema}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := driver.ApplyChanges(context.Background(), []atlasschema.Change{&atlasschema.DropSchema{S: schema, Extra: []atlasschema.Clause{&postgres.Cascade{}}}}); err != nil {
			t.Error(err)
		}
	}()
	db, err := store.OpenDatabase(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Schema.Create(t.Context(), entschema.WithSchemaName(namespace), entschema.WithForeignKeys(false)); err != nil {
		t.Fatal(err)
	}
	address := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().String()
	}
	first, second := address(), address()
	values := map[string]any{"database_url": u.String(), "listen": first, "api_token": "test-api-token-at-least-24-characters", "admin_token": "test-admin-token-at-least-24-characters", "backend_cidrs": []string{"127.0.0.0/8"}, "backend_ports": []string{"8001"}, "backend_timeout": "120s", "poll_interval": "5s"}
	values["rabbitmq_queue"] = queue
	var content atomic.Value
	setContent := func() { raw, _ := json.Marshal(values); content.Store(raw) }
	setContent()
	center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(content.Load().([]byte)) }))
	defer center.Close()
	bootstrap, _ := json.Marshal(map[string]string{"appconfig_url": center.URL})
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(path, bootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, path) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	defer client.CloseIdleConnections()
	status := func(address, path, token string) int {
		req, _ := http.NewRequest("GET", "http://"+address+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			return 0
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	wait := func(timeout time.Duration, check func() bool) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("running server did not apply configuration")
	}
	wait(8*time.Second, func() bool { return status(first, "/_gateway/ready", "") == 200 })
	oldToken := values["api_token"].(string)
	values["listen"], values["api_token"] = second, oldToken+"-new"
	setContent()
	// 通过真实启动入口验证 AppConfig 的 45 秒刷新周期和监听切换。
	wait(60*time.Second, func() bool {
		return status(second, "/v1/gateway/requests", oldToken+"-new") == 400 && status(first, "/_gateway/ready", "") == 0
	})
	if status(second, "/v1/gateway/requests", oldToken) != 401 {
		t.Fatal("old token remains active after center update")
	}
}
