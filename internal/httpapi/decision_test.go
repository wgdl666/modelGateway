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

func TestDecisionPredict(t *testing.T) {
	const contentType = "multipart/form-data; boundary=decision-test"
	const payload = "--decision-test\r\nContent-Disposition: form-data; name=\"image\"; filename=\"photo.png\"\r\n\r\nimage-data\r\n--decision-test--\r\n"
	const response = `{"prediction":{"label":"标准商品图","probability":0.8}}`
	cfg := config.Config{
		APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24),
		AllowedNetworks: []string{"172.31.0.0/16"}, AllowedPorts: []string{"8001"},
		MaxBodyBytes: int64(len(payload)),
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, runtime)
	calls := 0
	s.direct.Transport = directRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != payload ||
			r.Method != http.MethodPost || r.URL.String() != "http://172.31.3.219:8874/v1/predict" ||
			r.Header.Get("Content-Type") != contentType || r.Header.Get("Authorization") != "" {
			t.Fatal("Decision must receive the original multipart upload without the gateway token")
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
	call := func(token, body string) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, http.MethodPost, decisionPredictPath,
			&ut.Body{Body: strings.NewReader(body), Len: len(body)},
			ut.Header{Key: "Content-Type", Value: contentType},
			ut.Header{Key: "Authorization", Value: "Bearer " + token})
	}
	if got := call(cfg.APIToken, payload); got.Code != http.StatusNotFound {
		t.Fatalf("disabled Decision returned %d", got.Code)
	}
	cfg.DecisionAddr = "172.31.3.219:8874"
	if err := runtime.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if got := call(cfg.AdminToken, payload); got.Code != http.StatusUnauthorized {
		t.Fatalf("wrong credential returned %d", got.Code)
	}
	if got := call(cfg.APIToken, payload+"x"); got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request returned %d", got.Code)
	}
	if got := call(cfg.APIToken, payload); got.Code != http.StatusOK || got.Body.String() != response || calls != 1 {
		t.Fatalf("Decision route returned %d and made %d upstream calls", got.Code, calls)
	}
	cfg.DecisionAddr = "8.8.8.8:8874"
	if err := runtime.Reload(cfg); err == nil {
		t.Fatal("a public Decision address must be rejected")
	}
}
