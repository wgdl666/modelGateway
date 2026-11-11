package config

import (
	"net"
	"testing"
)

func TestSingleGPUEndpointRejectsPathAliases(t *testing.T) {
	_, network, _ := net.ParseCIDR("127.0.0.0/8")
	snapshot := Snapshot{networks: []*net.IPNet{network}, ports: map[string]bool{"8002": true}}
	origin := "http://127.0.0.1:8002"
	if err := snapshot.ValidateEndpoint(origin+"/gpu0", false); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.ValidateEndpoint(origin+"/gpu0/healthz", true); err != nil {
		t.Fatal(err)
	}
	// 探针和派发都不能用代理可能重写到其他卡的路径。
	for _, invalid := range []string{"/gpu00", "/%67pu0", "/gpu0/../gpu1", "/gpu0%2fgpu1"} {
		if err := snapshot.ValidateEndpoint(origin+invalid, false); err == nil {
			t.Errorf("accepted GPU endpoint alias %q", invalid)
		}
	}
	if err := snapshot.ValidateEndpoint(origin+"/gpu0/../gpu1/healthz", true); err == nil {
		t.Fatal("accepted a probe traversing another GPU path")
	}
}

func TestParseModelKeyCapacity(t *testing.T) {
	cfg, err := Parse([]byte(`{"database_url":"postgres://fixture/db","units_by_model_key":{"Vmind_outfit_v1.0":3}}`))
	if err != nil || cfg.UnitsByModelKey["Vmind_outfit_v1.0"] != 3 {
		t.Fatalf("model key units were not loaded: %v", err)
	}
	if _, err := Parse([]byte(`{"database_url":"postgres://fixture/db","units_by_capability":{"vton":3}}`)); err == nil {
		t.Fatal("legacy capacity config would silently lose its weight")
	}
}

func TestHotReloadKeepsQueueDatabase(t *testing.T) {
	cfg := Config{
		DatabaseURL:     "test-original-database",
		APIToken:        "test-client-token-at-least-24-chars",
		AdminToken:      "test-admin-token-at-least-24-chars",
		AllowedNetworks: []string{"127.0.0.0/8"},
		AllowedPorts:    []string{"8001", "8002"},
	}
	runtime, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.Current().MaxWaiting; got != 128 {
		t.Fatalf("default waiting limit = %d, want 128", got)
	}
	next := cfg
	next.APIToken += "-new"
	if err := runtime.Reload(next); err != nil || runtime.Current().APIToken != next.APIToken {
		t.Fatalf("same database reload: %v", err)
	}
	next.DatabaseURL = "test-other-database"
	if err := runtime.Reload(next); err == nil || runtime.Current().DatabaseURL != cfg.DatabaseURL {
		t.Fatal("database hot reload could abandon accepted requests")
	}
}

func TestLogfireConfigRequiresRestart(t *testing.T) {
	cfg, err := Parse([]byte(`{"database_url":"postgres://fixture/db","api_token":"test-api-token-at-least-24-chars","admin_token":"test-admin-token-at-least-24-chars","backend_cidrs":["127.0.0.0/8"],"backend_ports":["8001"],"logfire":{"token":"test-write-token","env":"US_ppe_exhibition","service":"model-gateway","version":"test-version","otel_log_level":"info"}}`))
	if err != nil || cfg.Logfire.Token != "test-write-token" || cfg.Logfire.Service != "model-gateway" ||
		cfg.Logfire.Env != "US_ppe_exhibition" || cfg.Logfire.Version != "test-version" || cfg.Logfire.OtelLogLevel != "info" {
		t.Fatalf("Logfire config was not loaded: %v", err)
	}
	runtime, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// 凭据、项目环境和服务身份都绑定 exporter；拒绝后必须保留原配置快照。
	for _, field := range []string{"token", "env", "service", "version", "level"} {
		next := cfg
		switch field {
		case "token":
			next.Logfire.Token = "different-token"
		case "env":
			next.Logfire.Env = "other-environment"
		case "service":
			next.Logfire.Service = "other-service"
		case "version":
			next.Logfire.Version = "other-version"
		case "level":
			next.Logfire.OtelLogLevel = "error"
		}
		if err := runtime.Reload(next); err == nil || runtime.Current().Logfire != cfg.Logfire {
			t.Fatalf("changing %s must reject the whole reload", field)
		}
	}
}
