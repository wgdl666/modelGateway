package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// 使用真实监听器覆盖 Hertz 的路由、流式读取和关闭，避免只测处理函数而漏掉协议层差异。
func startHTTPServer(t *testing.T, app *Server) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := app.HTTPServer(listener)
	done := make(chan error, 1)
	go func() { done <- h.Run() }()
	t.Cleanup(func() {
		http.DefaultClient.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := h.Shutdown(ctx); err != nil {
			_ = h.Close()
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Hertz server did not stop")
		}
	})
	base := "http://" + listener.Addr().String()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(base + "/_gateway/live")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return base
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Hertz server did not become live")
	return ""
}

func TestAdminViewsReadSharedRequestWithoutInput(t *testing.T) {
	a, b := stores(t)
	spec := testInstance(videoModel, "video", "http://127.0.0.1:8001", 2)
	registerReady(t, a, spec)
	id := uuid.NewString()
	submission := testAdmission(spec, id, 2)
	submission.traceID = "trace-for-ops"
	if _, err := a.Admit(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	app, err := NewServer(b, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.cancel)
	base := startHTTPServer(t, app)
	get := func(path, token string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s: status=%d response=%s err=%v", path, res.StatusCode, raw, err)
		}
		return raw
	}
	get("/v1/admin/requests", apiToken, 401)
	list := get("/v1/admin/requests?trace_id=trace-for-ops&state=processing", adminToken, 200)
	var page struct {
		Items []requestView
		Total int
	}
	if err := json.Unmarshal(list, &page); err != nil || page.Total != 1 || len(page.Items) != 1 ||
		page.Items[0].RequestID != id || !page.Items[0].SlotReserved || strings.Contains(string(list), "request_payload") {
		t.Fatalf("invalid list %s, error %v", list, err)
	}
	instances := get("/v1/instances", adminToken, 200)
	if !strings.Contains(string(instances), `"reserved_units":2`) || !strings.Contains(string(instances), `"reserved_requests":1`) {
		t.Fatalf("invalid instance capacities %s", instances)
	}
	if err := a.RecordResponse(t.Context(), id, "", succeeded(id), time.Second); err != nil {
		t.Fatal(err)
	}
	detail := get("/v1/admin/requests/"+id, adminToken, 200)
	var task requestView
	if err := json.Unmarshal(detail, &task); err != nil || task.TraceID != submission.traceID ||
		task.State != "succeeded" || task.Result == nil || len(task.Result.Files) != 1 || task.SlotReserved {
		t.Fatalf("invalid detail %s, error %v", detail, err)
	}
}

func TestHertzRoutingAndValidation(t *testing.T) {
	cfg := testConfig()
	app, err := NewServer(&Store{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.cancel)
	base := startHTTPServer(t, app)
	for _, test := range []struct {
		path, token string
		code        int
	}{
		{"/_gateway/live", "", 200},
		{"/v1/gateway/requests", "", 401},
		{"/v1/gateway/requests", workerToken, 401},
		{"/v1/gateway/requests", apiToken, 400},
		{"/v1/requests/no-legacy-route", apiToken, 404},
	} {
		req, err := http.NewRequest(http.MethodGet, base+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+test.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != test.code {
			t.Errorf("%s: got %d want %d", test.path, resp.StatusCode, test.code)
		}
	}
}
