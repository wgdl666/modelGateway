package config

import (
	"github.com/wgdl666/modelGateway/internal/protocol"
	"testing"
)

func TestEmbeddingTargetPolicy(t *testing.T) {
	cfg, err := NewRuntime(Config{DatabaseURL: "test", APIToken: "test-api-token-at-least-24-chars", AdminToken: "test-admin-token-at-least-24-chars", AllowedNetworks: []string{"172.31.0.0/16"}, AllowedPorts: []string{"8812", "8813"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		endpoint, health string
		valid            bool
	}{
		{"http://172.31.3.219:8812", "http://172.31.3.219:8812/health", true},
		{"http://172.31.3.219:8812", "http://172.31.3.219:8813/health", false},
		{"http://127.0.0.1:8812", "http://127.0.0.1:8812/health", false},
		{"http://172.31.3.219:9999", "http://172.31.3.219:9999/health", false},
	} {
		err := cfg.Current().ValidateInstanceTarget(protocol.InstanceTarget{Protocol: protocol.EmbeddingProtocol, Endpoint: tc.endpoint, HealthURL: tc.health})
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.endpoint, err)
		}
	}
}
