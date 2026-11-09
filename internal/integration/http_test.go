package integration

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
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

// 使用真实监听器覆盖 Hertz 的路由、流式读取和关闭，避免只测处理函数而漏掉协议层差异。
func startHTTPServer(t *testing.T, app *httpapi.Server) string {
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
	a, b, _ := stores(t)
	spec := testInstance(protocol.VideoModel, "video", "http://127.0.0.1:8001", 2)
	registerReady(t, a, spec)
	id := uuid.NewString()
	submission := testAdmission(spec, id, 2)
	submission.TraceID = "trace-for-ops"
	if _, err := a.Admit(t.Context(), submission); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Claim(t.Context(), testConfig(), id); err != nil {
		t.Fatal(err)
	}
	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(b, runtime))
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
	type requestView struct {
		protocol.Response
		TraceID      string `json:"trace_id"`
		SlotReserved bool   `json:"slot_reserved"`
	}
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
	if err := json.Unmarshal(detail, &task); err != nil || task.TraceID != submission.TraceID ||
		task.State != "succeeded" || task.Result == nil || len(task.Result.Files) != 1 || task.SlotReserved {
		t.Fatalf("invalid detail %s, error %v", detail, err)
	}
}

func TestHertzRoutingAndValidation(t *testing.T) {
	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(&store.Store{}, runtime))
	for _, test := range []struct {
		method, path, token string
		code                int
	}{
		{http.MethodGet, "/_gateway/live", "", 200},
		{http.MethodGet, "/v1/gateway/requests", "", 401},
		{http.MethodGet, "/v1/gateway/requests", adminToken, 401},
		{http.MethodGet, "/v1/gateway/requests", apiToken, 400},
		{http.MethodGet, "/v1/requests/no-legacy-route", apiToken, 404},
		{http.MethodPost, "/v1/instances/register", "", 401},
		{http.MethodPost, "/v1/instances/register", apiToken, 401},
		// 缺少 Content-Type 时返回 415，表明管理凭据已通过鉴权。
		{http.MethodPost, "/v1/instances/register", adminToken, 415},
	} {
		req, err := http.NewRequest(test.method, base+test.path, nil)
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
