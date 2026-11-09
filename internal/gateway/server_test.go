package gateway

import (
	"context"
	"net"
	"net/http"
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
