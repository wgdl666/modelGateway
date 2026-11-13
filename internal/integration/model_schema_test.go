package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestUnifiedChatUsesStoredSchemaAndPreservesResponse(t *testing.T) {
	ledger, _, _ := stores(t)
	cfg := testConfig()
	cfg.VLMAddr = "172.31.43.7:18280"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input["model"] != "schema-chat" || input["reasoning_effort"] != "high" || input["temperature"] != float64(0) {
			t.Errorf("request changed: %+v", input)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	base := synchronousServer(t, ledger, cfg, upstream)
	spec := testInstance("schema-chat", "http://"+cfg.VLMAddr, 1)
	spec.Protocol = protocol.ChatProtocol
	spec.HealthURL = cfg.SynchronousHealthURL(spec.Protocol)
	registerReady(t, ledger, spec)
	schema := json.RawMessage(`{"type":"object","x-category":"chat","required":["messages","reasoning_effort"],"properties":{"messages":{"type":"array"},"reasoning_effort":{"enum":["high"]},"temperature":{"type":"number"},"stream":{"type":"boolean"}},"additionalProperties":false}`)
	if err := ledger.SetModelSchema(t.Context(), spec.ModelKey, schema); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		effort string
		status int
	}{{"low", 400}, {"high", 200}} {
		resp := sendSynchronous(t, base, "/v1/gateway/requests/"+spec.ModelKey, "application/json", `{"input":{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"`+tc.effort+`","temperature":0,"stream":true}}`, uuid.NewString())
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
		}
		if tc.status == 200 && !strings.Contains(string(raw), "[DONE]") {
			t.Fatalf("stream changed: %s", raw)
		}
	}
	req, _ := http.NewRequest("GET", base+"/v1/models?category=chat", nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "schema-chat") {
		t.Fatalf("catalog %d %s", resp.StatusCode, raw)
	}
	definition, err := ledger.ModelDefinition(t.Context(), spec.ModelKey)
	if err != nil || !strings.Contains(string(definition.InputSchema), "reasoning_effort") {
		t.Fatalf("stored schema: %+v %v", definition, err)
	}
}

func TestSchemaMigrationAndReplicaInheritance(t *testing.T) {
	ledger, _, db := stores(t)
	spec := testInstance(protocol.OutfitModel, "http://127.0.0.1:8001", 1)
	registerReady(t, ledger, spec)
	if err := db.ModelInstance.UpdateOneID(spec.ID).ClearInputSchema().Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	raw, err := os.ReadFile("../../deploy/migrations/model-input-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), string(raw)); err != nil {
		t.Fatal(err)
	}
	original, err := ledger.ModelDefinition(t.Context(), spec.ModelKey)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(original.InputSchema), "images") {
		t.Fatal("migration did not seed schema")
	}
	custom := json.RawMessage(`{"type":"object","description":"custom contract","properties":{"images":{"type":"array","minItems":2,"maxItems":2}},"required":["images"]}`)
	if err := ledger.SetModelSchema(t.Context(), spec.ModelKey, custom); err != nil {
		t.Fatal(err)
	}
	replica := spec
	replica.ID = uuid.NewString()
	replica.Endpoint = "http://127.0.0.1:8002"
	if err := ledger.Register(t.Context(), replica, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(), string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{spec.ID, replica.ID} {
		row, err := db.ModelInstance.Get(t.Context(), id)
		if err != nil || !strings.Contains(string(row.InputSchema), "custom contract") {
			t.Fatalf("replica contract: %s %v", id, err)
		}
	}
	definitions, err := ledger.ListModelDefinitions(t.Context())
	if err != nil || len(definitions) != 1 {
		t.Fatalf("directory did not dedup replicas: %+v %v", definitions, err)
	}
}
