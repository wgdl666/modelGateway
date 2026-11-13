package worker

import (
	"context"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSubjectPoolHealthIsIndependentAndAuthenticated(t *testing.T) {
	status := 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			if r.Header.Get("X-Segment-Pool-Key") != "" {
				t.Error("pool secret leaked to legacy health")
			}
			w.WriteHeader(503)
			return
		}
		if r.URL.Path != "/pool/ready" || r.Header.Get("X-Segment-Pool-Key") != "fixture-secret" {
			t.Errorf("invalid pool probe %s", r.URL)
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	// Production requires private addresses; transport maps a private test identity to the local fixture.
	endpoint := "http://10.0.0.2:" + u.Port() + "/pool"
	cfg := config.Config{APIToken: "test-client-token-at-least-24-chars", AdminToken: "test-admin-token-at-least-24-chars", AllowedNetworks: []string{"10.0.0.0/8"}, AllowedPorts: []string{u.Port()}, SubjectPoolAPIKey: "fixture-secret", SubjectPools: map[string]config.SubjectPool{protocol.SegmentSubjectModel: {Protocol: protocol.SegmentSubjectProtocol, Endpoint: endpoint, HealthURL: endpoint + "/ready"}}}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, u.Host)
	}
	client.Transport = transport
	defer transport.CloseIdleConnections()
	w := &Reconciler{client: client}
	p := cfg.SubjectPools[protocol.SegmentSubjectModel]
	if !w.readSubjectPoolHealth(t.Context(), runtime.Current(), p) {
		t.Fatal("available pool rejected")
	}
	target := protocol.InstanceTarget{ModelKey: protocol.SegmentSubjectModel, Protocol: protocol.SegmentSubjectProtocol, Endpoint: p.Endpoint, HealthURL: p.HealthURL}
	if result, err := w.ProbeInstance(t.Context(), runtime.Current(), target); err != nil || !result.Ready {
		t.Fatalf("manual pool health: %+v %v", result, err)
	}
	status = 503
	if w.readSubjectPoolHealth(t.Context(), runtime.Current(), p) {
		t.Fatal("unavailable pool accepted")
	}
	sample := w.readSynchronousHealth(t.Context(), protocol.SyncVision, server.URL+"/health")
	if sample.ready(protocol.SegmentSubjectProtocol, "http://127.0.0.1:6106", 1) {
		t.Fatal("pool readiness used as legacy replica proof")
	}
}
