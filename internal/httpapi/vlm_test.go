package httpapi

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/wgdl666/modelGateway/internal/config"
)

type vlmRoundTrip func(*http.Request) (*http.Response, error)

func (f vlmRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestChatCompletions(t *testing.T) {
	const response = `{"model":"vmind","choices":[{"message":{"content":"red"}}]}`
	request := `{"model":"vmind","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("A", 4096) + `"}},{"type":"text","text":"What color?"}]}],"max_tokens":32}`
	cfg := config.Config{
		APIToken:        strings.Repeat("a", 24),
		AdminToken:      strings.Repeat("b", 24),
		AllowedNetworks: []string{"172.31.0.0/16"},
		AllowedPorts:    []string{"8001"},
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, runtime)
	calls := 0
	s.vlm.Transport = vlmRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil || string(raw) != request {
			t.Fatal("upstream did not receive the original multimodal request")
		}
		if r.Method != http.MethodPost || r.URL.String() != "http://172.31.3.219:30332/v1/chat/completions" ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "" {
			t.Fatal("invalid VLM upstream request")
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(strings.NewReader(response)),
			ContentLength: int64(len(response)),
		}, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	h := s.HTTPServer(listener)
	call := func(token, payload string) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, http.MethodPost, chatCompletionsPath,
			&ut.Body{Body: strings.NewReader(payload), Len: len(payload)},
			ut.Header{Key: "Content-Type", Value: "application/json"},
			ut.Header{Key: "Authorization", Value: "Bearer " + token})
	}
	if got := call(cfg.APIToken, request); got.Code != http.StatusNotFound {
		t.Fatalf("disabled VLM returned %d", got.Code)
	}
	cfg.VLMAddr = "172.31.3.219:30332"
	if err := runtime.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if got := call(cfg.AdminToken, request); got.Code != http.StatusUnauthorized {
		t.Fatalf("wrong credential returned %d", got.Code)
	}
	if got := call(cfg.APIToken, strings.Replace(request, `"vmind"`, `"other"`, 1)); got.Code != http.StatusNotFound {
		t.Fatalf("unknown model returned %d", got.Code)
	}
	got := call(cfg.APIToken, request)
	if got.Code != http.StatusOK || got.Body.String() != response || calls != 1 {
		t.Fatalf("VLM route returned %d and made %d upstream calls", got.Code, calls)
	}
	cfg.VLMAddr = "8.8.8.8:30332"
	if err := runtime.Reload(cfg); err == nil {
		t.Fatal("a public VLM address must be rejected")
	}
}
