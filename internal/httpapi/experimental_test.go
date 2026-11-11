package httpapi

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/wgdl666/modelGateway/internal/config"
)

func TestExperimentalTokenHasAPIAccessAndIsMarked(t *testing.T) {
	cfg := config.Config{
		APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24), ExperimentalToken: strings.Repeat("e", 24),
		AllowedNetworks: []string{"172.31.0.0/16"}, AllowedPorts: []string{"8001"},
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, runtime, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	h := s.HTTPServer(listener)
	h.GET("/test/credential", s.auth("api", func(_ context.Context, c *app.RequestContext) {
		if c.GetBool(experimentalCredential) {
			c.String(http.StatusOK, "experimental")
			return
		}
		c.String(http.StatusOK, "production")
	}))
	call := func(path, token string) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, http.MethodGet, path, nil, ut.Header{Key: "Authorization", Value: "Bearer " + token})
	}
	for token, want := range map[string]string{cfg.APIToken: "production", cfg.ExperimentalToken: "experimental"} {
		if got := call("/test/credential", token); got.Code != http.StatusOK || got.Body.String() != want {
			t.Fatalf("token %q: %d %q, want %q", token[:1], got.Code, got.Body.String(), want)
		}
	}
	for _, token := range []string{cfg.AdminToken, "wrong"} {
		if got := call("/test/credential", token); got.Code != http.StatusUnauthorized {
			t.Fatalf("token %q reached an API route: %d", token, got.Code)
		}
	}
	// 实验凭据不能访问管理接口。
	if got := call("/v1/instances", cfg.ExperimentalToken); got.Code != http.StatusUnauthorized {
		t.Fatalf("experimental token reached admin route: %d", got.Code)
	}
}
