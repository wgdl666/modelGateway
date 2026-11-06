package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

func TestHotReloadPreservesInFlightRequest(t *testing.T) {
	a, observer := stores(t)
	started, finish := make(chan struct{}), make(chan struct{})
	var release sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer original-backend-token" {
			close(started)
			select {
			case <-finish:
			case <-r.Context().Done():
				return
			}
		} else if r.Header.Get("Authorization") != "Bearer updated-backend-token" {
			t.Error("backend token did not reload")
		}
		if r.Method == "POST" && r.Header.Get("Authorization") == "Bearer updated-backend-token" {
			time.Sleep(100 * time.Millisecond)
		}
		writeBackendJSON(w, 200, BackendResponse{Status: "succeeded", Result: files})
	}))
	t.Cleanup(backend.Close)
	u, _ := url.Parse(backend.URL)
	cfg := Config{DatabaseURL: os.Getenv("TEST_DATABASE_URL"), APIToken: apiToken, WorkerToken: workerToken, AdminToken: adminToken, BackendToken: "original-backend-token", AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{u.Port()}, BackendTimeout: 5 * time.Second}
	app, err := NewServer(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { release.Do(func() { close(finish) }); app.Close() })
	server := startHTTPServer(t, app)
	i := instance(backend.URL, 4)
	body, _ := json.Marshal(i)
	if status, _ := post(t, server+"/v1/instances/register", workerToken, body); status != 200 {
		t.Fatal(status)
	}
	first := submission(2)
	if status, _ := post(t, server+"/v1/requests", apiToken, payload(first)); status != 202 {
		t.Fatal(status)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach backend")
	}
	// 修改连接参数创建新池，第一单仍在原池上等待模型结果。
	dsn, _ := url.Parse(cfg.DatabaseURL)
	query := dsn.Query()
	query.Set("application_name", "gateway-hot-reload-test")
	dsn.RawQuery = query.Encode()
	cfg.DatabaseURL = dsn.String()
	cfg.APIToken, cfg.WorkerToken, cfg.AdminToken = apiToken+"-new", workerToken+"-new", adminToken+"-new"
	cfg.BackendToken, cfg.BackendTimeout = "updated-backend-token", 20*time.Millisecond
	cfg.InstanceLease, cfg.PollInterval = 45*time.Second, 6*time.Second
	cfg.MaxBodyBytes = 1
	cfg.AllowedNetworks = []string{"10.0.0.0/8"}
	if err := app.Reload(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if status, _ := post(t, server+"/v1/requests", apiToken, payload(first)); status != 401 {
		t.Fatal("old API token remains valid")
	}
	if status, _ := post(t, server+"/v1/requests", cfg.APIToken, payload(first)); status != 400 {
		t.Fatal("request body limit did not reload")
	}
	cfg.MaxBodyBytes = 1 << 20
	if err := app.Reload(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if status, _ := post(t, server+"/v1/instances/register", workerToken, body); status != 401 {
		t.Fatal("old worker token remains valid")
	}
	if status, data := post(t, server+"/v1/instances/register", cfg.WorkerToken, body); status != 400 || string(data) != "{\"error_code\":\"endpoint_not_allowed\"}\n" {
		t.Fatal("backend network permit did not reload", status, string(data))
	}
	cfg.AllowedNetworks = []string{"127.0.0.0/8"}
	if err := app.Reload(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if status, _ := post(t, server+"/v1/instances/"+i.ID+"/heartbeat", cfg.WorkerToken, []byte(`{"ready":true}`)); status != 200 {
		t.Fatal("updated worker token rejected")
	}
	current, err := observer.DB.ModelInstance.Get(context.Background(), i.ID)
	if err != nil || time.Until(current.LeaseExpiresAt) < 40*time.Second {
		t.Fatal("instance lease did not reload", current, err)
	}
	if status, _ := post(t, server+"/v1/instances/"+i.ID+"/drain", cfg.AdminToken, []byte(`{"draining":false}`)); status != 200 {
		t.Fatal("updated admin token rejected")
	}
	second := submission(2)
	if status, _ := post(t, server+"/v1/requests", cfg.APIToken, payload(second)); status != 202 {
		t.Fatal(status)
	}
	waitState(t, observer, second.ID, "unknown")
	release.Do(func() { close(finish) })
	waitState(t, observer, first.ID, "succeeded")
	if used(t, observer, i.ID) != 2 {
		t.Fatal("in-flight completion or timeout lost capacity accounting")
	}
	err = observer.DB.Request.UpdateOneID(second.ID).SetUpdatedAt(time.Now().Add(-10 * time.Second)).Exec(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	app.ReconcileOnce(context.Background())
	waitState(t, observer, second.ID, "succeeded")
	if used(t, observer, i.ID) != 0 {
		t.Fatal("reloaded backend query did not release capacity")
	}
	bad := cfg
	bad.APIToken = ""
	if err := app.Reload(context.Background(), bad); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if status, _ := post(t, server+"/v1/requests", cfg.APIToken, payload(second)); status != 200 {
		t.Fatal("rejected replacement changed current configuration")
	}
}
