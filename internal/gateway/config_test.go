package gateway

import "testing"

func TestHotReloadKeepsQueueDatabase(t *testing.T) {
	cfg := testConfig()
	cfg.DatabaseURL = "test-original-database"
	s, err := NewServer(&Store{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.cancel)
	next := cfg
	next.APIToken += "-new"
	if err := s.Reload(t.Context(), next); err != nil || s.config().APIToken != next.APIToken {
		t.Fatalf("same database reload: %v", err)
	}
	next.DatabaseURL = "test-other-database"
	if err := s.Reload(t.Context(), next); err == nil || s.config().DatabaseURL != cfg.DatabaseURL {
		t.Fatal("database hot reload could abandon accepted requests")
	}
}
