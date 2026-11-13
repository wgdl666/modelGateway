package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/worker"
)

func TestInstanceHealthProbeAndAutomaticArtifact(t *testing.T) {
	ledger, _, _ := stores(t)
	digest := strings.Repeat("a", 64)
	var report atomic.Value
	report.Store(fmt.Sprintf(`{"ready":true,"artifact_digest":"sha256:%s"}`, digest))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/health" || r.Header.Get("Authorization") != "Bearer worker-test" {
			t.Errorf("wrong health request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, report.Load().(string))
	}))
	t.Cleanup(backend.Close)
	_, port, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.AllowedPorts = []string{port}
	cfg.BackendToken = "worker-test"
	cfg.RabbitMQURL = "amqp://127.0.0.1"
	cfg.RabbitMQQueue = "health-probe-test"
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(ledger, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reconciler.Close)
	base := startHTTPServer(t, httpapi.New(ledger, runtime, reconciler.ProbeInstance))
	post := func(path, token string, body any, want int) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s returned %d %s (%v), want %d", path, resp.StatusCode, data, err, want)
		}
		return data
	}

	const probe = "/v1/instances/probe"
	const register = "/v1/instances/register"
	instance := testInstance(protocol.OutfitModel, backend.URL, 1)
	instance.ModelKey = "registered-health-v2"
	instance.Artifact = ""
	target := protocol.InstanceTarget{ModelKey: instance.ModelKey, Protocol: instance.Protocol, Endpoint: instance.Endpoint, HealthURL: instance.HealthURL}
	post(probe, apiToken, target, 401)
	blocked := target
	blocked.HealthURL = "http://192.0.2.1:" + port + "/health"
	post(probe, adminToken, blocked, 400)
	blocked = target
	blocked.Protocol = "unsupported"
	post(probe, adminToken, blocked, 400)
	var health protocol.InstanceHealth
	if raw := post(probe, adminToken, target, 200); json.Unmarshal(raw, &health) != nil ||
		!health.Ready || health.ArtifactDigest != "sha256-"+digest {
		t.Fatalf("unexpected health preview: %s", raw)
	}
	if rows, err := ledger.ListInstances(t.Context()); err != nil || len(rows) != 0 {
		t.Fatalf("probe changed registry: %v %v", rows, err)
	}

	ensureTestGroup(t, ledger, instance)
	digest = strings.Repeat("b", 64)
	report.Store(fmt.Sprintf(`{"ready":true,"artifact_digest":"sha256:%s"}`, digest))
	post(register, adminToken, instance, 200)
	invalidSchema := instance
	invalidSchema.InputSchema = json.RawMessage(`{"type":"not-a-json-schema-type"}`)
	post(register, adminToken, invalidSchema, 400)
	// ModelHub 查询的类型直接来自这条登记，不要求探针先把实例标记为可派。
	metadataRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/models/"+instance.ModelKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	metadataRequest.Header.Set("Authorization", "Bearer "+apiToken)
	metadataResponse, err := http.DefaultClient.Do(metadataRequest)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	err = json.NewDecoder(metadataResponse.Body).Decode(&metadata)
	metadataResponse.Body.Close()
	if err != nil || metadataResponse.StatusCode != http.StatusOK || metadata["model_key"] != instance.ModelKey || metadata["protocol"] != instance.Protocol {
		t.Fatalf("wrong registered protocol: %v %v", metadata, err)
	}
	// 同一个 key 的其他副本不能改用另一协议，即使实例 ID 不同。
	conflict := instance
	conflict.ID = "different-protocol-replica"
	conflict.Protocol = protocol.BodyProtocol
	post(register, adminToken, conflict, 409)
	report.Store(`{"ready":false}`)
	health = protocol.InstanceHealth{}
	if raw := post(probe, adminToken, target, 200); json.Unmarshal(raw, &health) != nil ||
		health.Ready || health.ArtifactDigest != "" {
		t.Fatalf("unexpected unready response: %s", raw)
	}
	post(register, adminToken, instance, 200)
	unknown := testInstance(protocol.ClothGenModel, backend.URL, 1)
	unknown.Artifact = ""
	post(register, adminToken, unknown, 200)
	rows, err := ledger.ListInstances(t.Context())
	if err != nil || len(rows) != 2 {
		t.Fatalf("instances = %v, err=%v", rows, err)
	}
	for _, row := range rows {
		if row.ID == instance.ID && row.ArtifactDigest != "sha256-"+digest {
			t.Fatalf("registration did not keep the artifact read at save: %q", row.ArtifactDigest)
		}
		if row.ID == unknown.ID && row.ArtifactDigest != "" {
			t.Fatalf("unknown artifact was replaced with %q", row.ArtifactDigest)
		}
	}
}
