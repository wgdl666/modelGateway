package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/testsupport"
	"github.com/wgdl666/modelGateway/internal/worker"
)

// Real Python controller + PostgreSQL journal + fenced host agent + alternating
// Go API replicas. Only the physical model activation is a CPU test hook.
func TestDeploymentControllerAcrossGatewayReplicas(t *testing.T) {
	python := os.Getenv("TEST_LIFECYCLE_PYTHON")
	if python == "" {
		t.Skip("TEST_LIFECYCLE_PYTHON required")
	}
	a, b, db := stores(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(t.Context(), os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	migration, err := os.ReadFile(filepath.Join(root, "deploy/migrations/deployment-lifecycle.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(t.Context(), "TRUNCATE model_gateway.deployment_operations,model_gateway.deployment_leases,model_gateway.deployment_domain; INSERT INTO model_gateway.deployment_domain(id,region) VALUES(1,'test/region')"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.DeploymentLease.Delete().Exec(context.Background())
		db.DeploymentDomain.Delete().Exec(context.Background())
	})
	digest := "sha256-" + strings.Repeat("a", 64)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ready": true, "models": []string{"lifecycle-image"}, "deployment_id": "new-deployment", "artifact_digest": digest, "boot_id": "boot", "region": "test/region"})
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	cfg := testConfig()
	cfg.AllowedPorts = []string{u.Port()}
	cfg.RabbitMQURL, cfg.RabbitMQQueue, _ = testsupport.RabbitQueue(t)
	cfg.PollInterval = 20 * time.Millisecond
	cfg.NativeResults = config.NativeResults{Provider: "s3", Region: "us-east-2", Bucket: "test-results"}
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(a, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	reconciler.Start()
	one := startHTTPServer(t, httpapi.New(a, runtime, reconciler.ProbeInstance))
	two := startHTTPServer(t, httpapi.New(b, runtime, reconciler.ProbeInstance))
	first, _ := url.Parse(one)
	second, _ := url.Parse(two)
	var n atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := first
		if n.Add(1)%2 == 0 {
			target = second
		}
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	old := testInstance("lifecycle-image", backend.URL, 1)
	old.Protocol = protocol.ImageGenerationProtocol
	registerReady(t, a, old)
	item := map[string]any{"instance_id": "new-instance", "model_key": old.ModelKey, "protocol": old.Protocol, "deployment_id": "new-deployment", "artifact_digest": digest, "endpoint": old.Endpoint, "health_url": old.HealthURL, "resource_group": old.ResourceGroup, "machine_id": old.MachineID, "gpu_devices": old.GPUDevices, "capacity_units": 1}
	plan := map[string]any{"schema_version": 1, "region": "test/region", "worker_id": "worker", "action": "upgrade", "resource_groups": []string{old.ResourceGroup}, "instances": []any{item}}
	tmp := t.TempDir()
	write := func(name string, value any) string {
		raw, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(tmp, name)
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	planPath := write("plan.json", plan)
	raw, _ := json.Marshal(plan)
	hash := sha256.Sum256(raw)
	planHash := hex.EncodeToString(hash[:])
	hooks := map[string]any{}
	count := filepath.Join(tmp, "activated")
	for _, action := range []string{"prepare", "idle", "activate", "verify", "stop"} {
		receipt := map[string]any{"ok": true, "idle": true, "active_requests": 0, "uncertain_executions": 0, "smoke_passed": true, "boot_id": "boot", "region": "test/region", "bindings": []any{item}}
		raw, _ := json.Marshal(receipt)
		script := "import json;print(" + strconvQuote(string(raw)) + ")"
		if action == "activate" {
			script = "from pathlib import Path;p=Path(" + strconvQuote(count) + ");p.write_text(p.read_text()+'x' if p.exists() else 'x');" + script
		}
		hooks[action] = map[string]any{"argv": []string{python, "-c", script}}
	}
	agent := write("agent.json", map[string]any{"region": "test/region", "worker_id": "worker", "plans": map[string]any{planHash: hooks}})
	conf := write("regions.json", map[string]any{"regions": map[string]any{"test/region": map[string]any{"gateway_url": proxy.URL, "database_url_env": "TEST_DATABASE_URL", "admin_token_env": "TEST_DEPLOY_ADMIN", "workers": map[string]any{"worker": map[string]any{"agent_argv": []string{python, "-m", "deployment_controller.agent", "--config", agent, "--state", filepath.Join(tmp, "state")}}}}}})
	for range 2 {
		cmd := exec.CommandContext(t.Context(), python, "-m", "deployment_controller", "--config", conf, "apply", planPath, "--operation", "test-operation")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "TEST_DEPLOY_ADMIN="+adminToken)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("controller failed: %v %s", err, output)
		}
		if !strings.Contains(string(output), `"phase": "complete"`) {
			t.Fatal(string(output))
		}
	}
	content, _ := os.ReadFile(count)
	if string(content) != "x" {
		t.Fatalf("activation replayed: %q", content)
	}
	rows, err := a.ListInstances(t.Context())
	if err != nil || len(rows) != 1 || rows[0].ID != "new-instance" || !rows[0].Eligible {
		t.Fatalf("wrong final registration %+v %v", rows, err)
	}
	retired, err := db.ModelInstance.Get(t.Context(), old.ID)
	if err != nil || retired.DeletedAt == nil {
		t.Fatal("old history not retained", err)
	}
	if n.Load() < 4 {
		t.Fatal("did not exercise both replicas")
	}
}
func strconvQuote(value string) string { raw, _ := json.Marshal(value); return string(raw) }
