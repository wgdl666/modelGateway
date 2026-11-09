package config

import "testing"

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
