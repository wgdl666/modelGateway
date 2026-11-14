package worker

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/internal/config"
)

func TestModelHTTPPreservesValidationWithoutChangingStatus(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, want string
		status                        int
	}{
		{"prompt", "application/json; charset=utf-8", `{"error":{"code":"invalid_request","field":"prompt","message":"Must be at most 32000 characters (received 32001)"}}`, "prompt: Must be at most 32000 characters (received 32001)", 400},
		{"image", "application/json", `{"error":{"field":"image","message":"Image exceeds 20 MiB"}}`, "image: Image exceeds 20 MiB", 413},
		{"content-type", "application/json", `{"error":{"field":"body","message":"Use multipart/form-data"}}`, "body: Use multipart/form-data", 415},
		{"legacy", "application/json", `{"detail":"提示词超过编码器限制"}`, "提示词超过编码器限制", 422},
		{"controls", "application/json", `{"error":{"field":"prompt\n","message":"Invalid\u0000text"}}`, "prompt: Invalid text", 400},
		{"html", "text/html", `<html>proxy error</html>`, "fallback", 400},
		{"malformed", "application/json", `{"error":`, "fallback", 400},
		{"empty", "application/json", `{"error":{"message":""}}`, "fallback", 400},
		{"large", "application/json", `{"detail":"` + strings.Repeat("x", 16<<10) + `"}`, "fallback", 400},
		{"auth", "application/json", `{"error":{"message":"credential must not be exposed"}}`, "fallback", 401},
		{"server", "application/json", `{"error":{"message":"internal diagnostic"}}`, "fallback", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			origin, _ := url.Parse(server.URL)
			runtime, err := config.NewRuntime(config.Config{DatabaseURL: "test", APIToken: strings.Repeat("a", 24),
				AdminToken: strings.Repeat("b", 24), AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{origin.Port()}})
			if err != nil {
				t.Fatal(err)
			}
			w := &Reconciler{runtime: runtime, client: server.Client()}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, nil)
			_, err = w.modelHTTP(req, true)
			var status modelStatusError
			if !errors.As(err, &status) || int(status) != tc.status {
				t.Fatalf("lost HTTP status: %v", err)
			}
			if got := modelValidationMessage(err, "fallback"); got != tc.want {
				t.Fatalf("validation message = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelHTTPStillRecognizesBusyBeforeExecution(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"error":{"code":"worker_busy","execution_started":false}}`))}
	if readModelValidationError(resp) != nil || !workerBusyBeforeExecution(resp.Body) {
		t.Fatal("validation parser consumed or changed the busy response")
	}
}
