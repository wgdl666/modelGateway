package config

import (
	"encoding/json"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"strings"
	"testing"
)

func TestWorkerGrantIsSecretScopedAndFrozen(t *testing.T) {
	cfg := Config{DatabaseURL: "test", APIToken: strings.Repeat("a", 32), AdminToken: strings.Repeat("b", 32), AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{"8001"}}
	binding := protocol.Instance{ModelKey: protocol.BodyModel, Protocol: protocol.BodyProtocol, Endpoint: "http://127.0.0.1:8001", HealthURL: "http://127.0.0.1:8001/health", ResourceGroup: "host/gpu0", Capacity: 1, MachineID: "host", GPUDevices: []int{0}}
	grant := WorkerRegistration{Token: strings.Repeat("w", 40), Region: "cn/dev", Bindings: []protocol.Instance{binding}}
	cfg.WorkerRegistrations = map[string]WorkerRegistration{"worker": grant}
	runtime, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	grant.Bindings[0].GPUDevices[0] = 7
	if runtime.Current().WorkerRegistrations["worker"].Bindings[0].GPUDevices[0] != 0 {
		t.Fatal("mutable grant escaped snapshot")
	}
	wire, _ := json.Marshal(cfg)
	if strings.Contains(string(wire), grant.Token) {
		t.Fatal("worker credential leaked into config JSON")
	}
	parsed, err := Parse([]byte(`{"database_url":"test","WorkerRegistrations":{"attacker":{"token":"injected"}}}`))
	if err != nil || len(parsed.WorkerRegistrations) != 0 {
		t.Fatal("worker grant accepted from config document", err)
	}
	secret, _ := json.Marshal(map[string]WorkerRegistration{"worker": grant})
	t.Setenv("MODEL_GATEWAY_WORKER_REGISTRATIONS", string(secret))
	parsed, err = Parse([]byte(`{"database_url":"test"}`))
	if err != nil || parsed.WorkerRegistrations["worker"].Token != grant.Token {
		t.Fatal("Secret not loaded", err)
	}
	grant.Token = cfg.AdminToken
	cfg.WorkerRegistrations["worker"] = grant
	if _, err = NewRuntime(cfg); err == nil {
		t.Fatal("worker reused admin credential")
	}
}
