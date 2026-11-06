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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/internal/gateway"
)

func TestConfigCenterReloadsRunningServer(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for PostgreSQL integration test")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(u.Path, "_test") {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URI for a _test database")
	}
	// 使用独立命名空间，避免与并行运行的容量和任务测试互相清理数据。
	namespace := fmt.Sprintf("gateway_reload_%d", time.Now().UnixNano())
	query := u.Query()
	query.Set("search_path", namespace)
	u.RawQuery = query.Encode()
	db, err := gateway.OpenDatabase(context.Background(), u.String(), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	defer func() {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Error(err)
			return
		}
		admin := stdlib.OpenDB(*cfg)
		defer admin.Close()
		driver, err := postgres.Open(admin)
		if err == nil {
			err = driver.ApplyChanges(context.Background(), []atlasschema.Change{&atlasschema.DropSchema{S: atlasschema.New(namespace), Extra: []atlasschema.Clause{&postgres.Cascade{}}}})
		}
		if err != nil {
			t.Error(err)
		}
	}()
	address := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().String()
	}
	first, second := address(), address()
	values := map[string]any{"database_url": u.String(), "listen": first, "api_token": "test-api-token-at-least-24-characters", "worker_token": "test-worker-token-at-least-24-characters", "admin_token": "test-admin-token-at-least-24-characters", "backend_cidrs": []string{"127.0.0.0/8"}, "backend_ports": []string{"8001"}, "backend_timeout": "120s", "poll_interval": "5s", "instance_lease": "30s"}
	var content atomic.Value
	setContent := func() { raw, _ := json.Marshal(values); content.Store(raw) }
	setContent()
	center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(content.Load().([]byte)) }))
	defer center.Close()
	bootstrap, _ := json.Marshal(map[string]string{"source": "appconfig", "url": center.URL})
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(path, bootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, false) }()
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
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("running server did not apply configuration")
	}
	wait(func() bool { return status(first, "/health/ready", "") == 200 })
	oldToken := values["api_token"].(string)
	values["listen"], values["api_token"] = second, oldToken+"-new"
	setContent()
	wait(func() bool {
		return status(second, "/v1/requests/not-a-uuid", oldToken+"-new") == 400 && status(first, "/health/ready", "") == 0
	})
	if status(second, "/v1/requests/not-a-uuid", oldToken) != 401 {
		t.Fatal("old token remains active after center update")
	}
}
