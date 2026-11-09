package config

import "testing"

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
