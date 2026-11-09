package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent"
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
	spec := testInstance(protocol.VideoModel, "http://127.0.0.1:8001", 2)
	registerReady(t, a, spec)
	id := uuid.NewString()
	cfg := testConfig()
	cfg.UnitsByModelKey = map[string]int{spec.ModelKey: 2}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(b, runtime))
	// 从生成入口受理，确认按模型键配置的两单位确实进入账本并用于预留。
	submit := `{"input":{"images":[{"mime_type":"image/png","data":"eA=="}]},"metadata":{"trace_id":"trace-for-ops"}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/gateway/requests/"+spec.ModelKey, strings.NewReader(submit))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Idempotency-Key", id)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil || res.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status=%d response=%s error=%v", res.StatusCode, body, readErr)
	}
	if task, err := a.Claim(t.Context(), cfg, id); err != nil || task == nil || task.Units != 2 {
		t.Fatalf("model key capacity not reserved: task=%v err=%v", task, err)
	}
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
	list := get("/v1/admin/requests?trace_id=trace-for-ops&state=processing&model="+spec.ModelKey, adminToken, 200)
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
		page.Items[0].RequestID != id || page.Items[0].ModelKey != spec.ModelKey || !page.Items[0].SlotReserved ||
		strings.Contains(string(list), "request_payload") || strings.Contains(string(list), `"capability"`) {
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
	if err := json.Unmarshal(detail, &task); err != nil || task.TraceID != "trace-for-ops" ||
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

func TestClearInstanceKeepsRequestHistory(t *testing.T) {
	a, b, db := stores(t)
	ctx := t.Context()
	spec := testInstance(protocol.VideoModel, "http://127.0.0.1:8001", 1)
	if err := a.Register(ctx, spec); err != nil {
		t.Fatal(err)
	}
	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(b, runtime))
	call := func(method, path, token string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
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
			t.Fatalf("%s %s: status=%d body=%s err=%v", method, path, res.StatusCode, raw, err)
		}
		return raw
	}
	path := "/v1/instances/" + spec.ID
	call(http.MethodDelete, path, apiToken, 401)
	call(http.MethodDelete, path, adminToken, 409) // 未暂停
	if err := a.Drain(ctx, spec.ID, true); err != nil {
		t.Fatal(err)
	}
	call(http.MethodDelete, path, adminToken, 409) // 尚未探测
	if err := a.Drain(ctx, spec.ID, false); err != nil {
		t.Fatal(err)
	}
	registerReady(t, a, spec)
	id := uuid.NewString()
	if _, err := a.Admit(ctx, testAdmission(spec, id, 1)); err != nil {
		t.Fatal(err)
	}
	if row, err := a.Claim(ctx, testConfig(), id); err != nil || row == nil {
		t.Fatalf("claim: %v %v", row, err)
	}
	if err := a.Drain(ctx, spec.ID, true); err != nil {
		t.Fatal(err)
	}

	// 暂停后停止探活，模拟成功样本过期和请求已报失败但执行尚未确认的场景。
	if err := db.ModelInstance.UpdateOneID(spec.ID).SetHealthCheckedAt(time.Now().Add(-time.Minute)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.DispatchFailure(ctx, id, 504, "upstream_timeout", "test pending confirmation", true); err != nil {
		t.Fatal(err)
	}
	call(http.MethodDelete, path, adminToken, 409)
	if rows := call(http.MethodGet, "/v1/instances", adminToken, 200); strings.Contains(string(rows), `"clearable":true`) {
		t.Fatalf("instance with a reserved slot was clearable: %s", rows)
	}
	if err := a.RecordResponse(ctx, id, "", succeeded(id), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordProbe(ctx, spec.ID, time.Now().UTC(), true); err != nil {
		t.Fatal(err)
	}
	call(http.MethodDelete, path, adminToken, 409) // 已暂停且无占槽，探活仍健康
	if err := db.ModelInstance.UpdateOneID(spec.ID).SetHealthCheckedAt(time.Now().Add(-time.Minute)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := call(http.MethodGet, "/v1/instances", adminToken, 200); !strings.Contains(string(rows), `"clearable":true`) {
		t.Fatalf("paused instance with an expired sample was not clearable: %s", rows)
	}

	call(http.MethodDelete, path, adminToken, 200)
	call(http.MethodDelete, path, adminToken, 200)
	if rows := call(http.MethodGet, "/v1/instances", adminToken, 200); strings.TrimSpace(string(rows)) != "[]" {
		t.Fatalf("soft-deleted registration still listed: %s", rows)
	}
	row, err := db.ModelInstance.Get(ctx, spec.ID)
	if err != nil || row.DeletedAt == nil {
		t.Fatalf("missing soft-deleted row: %v %v", row, err)
	}
	if err := a.Drain(ctx, spec.ID, false); !ent.IsNotFound(err) {
		t.Fatalf("clear did not prevent reopening: %v", err)
	}
	if err := a.Register(ctx, spec); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cleared ID accepted for registration: %v", err)
	}
	// 新部署可以更换组容量，已清除的登记不会约束它。
	replacement := spec
	replacement.ID, replacement.GroupCapacity = uuid.NewString(), 2
	if err := a.Register(ctx, replacement); err != nil {
		t.Fatalf("tombstone blocked a new deployment: %v", err)
	}
	history := call(http.MethodGet, "/v1/admin/requests?instance_id="+spec.ID, adminToken, 200)
	if !strings.Contains(string(history), id) || !strings.Contains(string(history), `"state":"failed"`) {
		t.Fatalf("historical request changed or disappeared: %s", history)
	}
}
