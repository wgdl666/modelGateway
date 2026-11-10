package worker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestSynchronousHealthUsesExecutionReplica(t *testing.T) {
	vision := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"services":{
			"facesim":{"ready":true,"health":"legacy readiness"},
			"human-parser":{"targeted_dispatch":true,"endpoint":"http://127.0.0.1:6105","ready":true,"health":{"model_loaded":true}},
			"segment-person":{"targeted_dispatch":true,"ready":true,"health":{"replicas":[{"port":6106,"ready":true},{"port":6111,"ready":false}]}}
		}}`)
	}))
	defer vision.Close()
	reconciler := &Reconciler{client: vision.Client()}
	sample := reconciler.readSynchronousHealth(t.Context(), protocol.SyncVision, vision.URL)
	if !sample.ready(protocol.HumanParseModel, "http://127.0.0.1:6105", 1) ||
		!sample.ready(protocol.SegmentPersonModel, "http://127.0.0.1:6106", 4) ||
		!sample.ready(protocol.SegmentSubjectModel, "http://127.0.0.1:6106", 4) ||
		sample.ready(protocol.SegmentSubjectModel, "http://127.0.0.1:6111", 4) ||
		sample.ready(protocol.SegmentPersonModel, "http://127.0.0.1:6112", 4) {
		t.Fatalf("unexpected G6 probe: %+v", sample)
	}

	decision := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ready":true,"ready_count":1,"workers":2}`)
	}))
	defer decision.Close()
	sample = reconciler.readSynchronousHealth(t.Context(), protocol.SyncDecision, decision.URL)
	if sample.ready(protocol.DecisionModel, "http://127.0.0.1:19101", 2) {
		t.Fatal("a partial aggregate must not identify a healthy Decision worker")
	}
}
