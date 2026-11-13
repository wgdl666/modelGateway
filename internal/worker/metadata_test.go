package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/wgdl666/kangaroo/callmeta"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"go.opentelemetry.io/otel/trace"
)

func TestRestoredTaskPropagatesToNativeAndAdapterHTTP(t *testing.T) {
	span := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	original := callmeta.WithIdentity(trace.ContextWithSpanContext(context.Background(), span), "device-a", "user-a")
	raw, _ := json.Marshal(protocol.Submission{Input: json.RawMessage(`{}`), Metadata: protocol.CaptureMetadata(original)})
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		incoming := callmeta.Propagator{}.Extract(context.Background(), callmeta.HTTPHeaders(r.Header))
		if device, user := callmeta.Identity(incoming); device != "device-a" || user != "user-a" {
			t.Error("worker lost source identity")
		}
		if trace.SpanContextFromContext(incoming).TraceID() != span.TraceID() {
			t.Error("worker lost trace")
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	runtime, err := config.NewRuntime(config.Config{DatabaseURL: "test", APIToken: "test-api-token-at-least-24-chars", AdminToken: "test-admin-token-at-least-24-chars", AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{endpoint.Port()}})
	if err != nil {
		t.Fatal(err)
	}
	w := &Reconciler{runtime: runtime, client: server.Client()}
	ctx := w.taskContext(callmeta.WithIdentity(context.Background(), "query-device", "query-user"), &ent.Request{ID: "id", RequestPayload: raw})
	if _, err := w.modelCall(ctx, runtime.Current(), http.MethodGet, server.URL, "/requests/id", nil); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := w.nativeJSON(ctx, nativeAuth{}, server.URL+"/jobs/id", &result); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}
