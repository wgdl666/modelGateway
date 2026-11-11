package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

const experimentalToken = "test-experimental-token-24-chars"

func TestExperimentalTokenAdmitsGenerationOverrides(t *testing.T) {
	ledger, _, db := stores(t)
	spec := testInstance(protocol.VideoModel, "http://127.0.0.1:8001", 1)
	ensureTestGroup(t, ledger, spec)
	if err := ledger.Register(t.Context(), spec, ""); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.ExperimentalToken = experimentalToken
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(ledger, runtime, nil))
	input := json.RawMessage(`{"prompt":"walk","duration":"4","resolution":"720p","images":[{"mime_type":"image/png","data":"eA=="}]}`)
	submit := func(token string) (string, int, string) {
		id := uuid.NewString()
		raw, _ := json.Marshal(protocol.Submission{Input: input})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/gateway/requests/"+spec.ModelKey, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", id)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return id, resp.StatusCode, string(body)
	}

	id, status, body := submit(apiToken)
	if status != http.StatusBadRequest || !strings.Contains(body, "invalid_input") {
		t.Fatalf("production token with overrides: %d %s", status, body)
	}
	if row, err := db.Request.Get(t.Context(), id); err == nil || row != nil {
		t.Fatalf("rejected overrides entered the queue: %+v", row)
	}

	id, status, body = submit(experimentalToken)
	if status != http.StatusAccepted {
		t.Fatalf("experimental token with overrides: %d %s", status, body)
	}
	row, err := db.Request.Get(t.Context(), id)
	if err != nil || row.State != "queued" || !strings.Contains(string(row.RequestPayload), `"duration":"4"`) {
		t.Fatalf("experimental request was not queued with its overrides: %+v %v", row, err)
	}
}
