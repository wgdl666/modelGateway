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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	// 独立数据库避免与另一个包的容量和任务测试互相影响。
	name := pgx.Identifier{fmt.Sprintf("gateway_reload_%d_test", time.Now().UnixNano())}.Sanitize()
	if _, err = admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u.Path = "/" + strings.Trim(name, `"`)
	db, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), db, "../../migrations/001_init.sql"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
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
	go func() { done <- run(ctx, path, "") }()
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
