package integration

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func TestAdmissionRejectsUnknownAndMismatchedModelsBeforeQueue(t *testing.T) {
	ledger, _, db := stores(t)
	spec := testInstance(protocol.VideoModel, "http://127.0.0.1:8001", 1)
	spec.ModelKey = "registered-video-v2"
	input := testAdmission(spec, uuid.NewString(), 1)
	if _, err := ledger.Admit(t.Context(), input); !errors.Is(err, store.ErrModelUnregistered) {
		t.Fatalf("unknown model admission: %v", err)
	}
	ensureTestGroup(t, ledger, spec)
	if err := ledger.Register(t.Context(), spec, ""); err != nil {
		t.Fatal(err)
	}
	input.Protocol = protocol.BodyProtocol
	if _, err := ledger.Admit(t.Context(), input); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatched model protocol admission: %v", err)
	}
	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(ledger, runtime, nil))
	for _, tc := range []struct {
		name, model, output string
		status              int
	}{
		{"unregistered", "not-registered", "video", 404},
		{"wrong capability", spec.ModelKey, "image", 400},
		{"unsupported capability", spec.ModelKey, "text", 400},
		{"registered but not ready", spec.ModelKey, "video", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.NewString()
			raw, _ := json.Marshal(protocol.Submission{Input: input.Input, Output: tc.output})
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/gateway/requests/"+tc.model, strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer "+apiToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", id)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			row, err := db.Request.Get(t.Context(), id)
			if tc.status != http.StatusAccepted {
				if row != nil || err == nil {
					t.Fatalf("rejected input entered queue: %+v", row)
				}
				return
			}
			if err != nil || row.Protocol != spec.Protocol || row.ModelKey != spec.ModelKey || row.State != "queued" {
				t.Fatalf("known unready model should wait in queue: %+v %v", row, err)
			}
		})
	}
	if count, err := db.Request.Query().Count(t.Context()); err != nil || count != 1 {
		t.Fatalf("only accepted request may persist: count=%d err=%v", count, err)
	}
}
