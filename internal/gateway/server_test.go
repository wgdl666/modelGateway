package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
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
		response, err := client.Get(base + "/health/live")
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

func TestHertzRoutingAndValidation(t *testing.T) {
	cfg := Config{APIToken: apiToken, WorkerToken: workerToken, AdminToken: adminToken,
		AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{"8001"}, MaxBodyBytes: 256}
	app, err := NewServer(&Store{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.cancel)
	base := startHTTPServer(t, app)
	for _, tc := range []struct {
		name, method, path, token, body string
		status                          int
		chunked                         bool
	}{
		{name: "health", method: "GET", path: "/health/live", status: 200},
		{name: "head", method: "HEAD", path: "/health/live", status: 200},
		{name: "method", method: "POST", path: "/health/live", status: 405},
		{name: "missing", method: "GET", path: "/unknown", status: 404},
		{name: "auth", method: "GET", path: "/v1/requests/invalid", status: 401},
		{name: "role", method: "POST", path: "/v1/requests", token: workerToken, status: 401},
		{name: "id", method: "GET", path: "/v1/requests/invalid", token: apiToken, status: 400},
		{name: "unknown field", method: "POST", path: "/v1/requests", token: apiToken, body: `{"extra":true}`, status: 400},
		{name: "trailing JSON", method: "POST", path: "/v1/requests", token: apiToken, body: `{} {}`, status: 400},
		{name: "body limit", method: "POST", path: "/v1/requests", token: apiToken, body: strings.Repeat("x", 257), status: 400},
		{name: "chunked limit", method: "POST", path: "/v1/requests", token: apiToken, body: strings.Repeat("x", 257), status: 400, chunked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader = strings.NewReader(tc.body)
			if tc.chunked {
				body = struct{ io.Reader }{body}
			}
			req, err := http.NewRequest(tc.method, base+tc.path, body)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tc.token)
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("expected %d got %d", tc.status, response.StatusCode)
			}
		})
	}
}
