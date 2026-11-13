package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestTokenEncryptionKeyConfig(t *testing.T) {
	cfg := Config{AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{"8001"}, APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24)}
	cfg.TokenEncryptionKey = "bad"
	if _, err := NewRuntime(cfg); err == nil {
		t.Fatal("invalid key accepted")
	}
	cfg.TokenEncryptionKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	r, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TokenEncryptionKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 32)))
	if err := r.Reload(cfg); err == nil {
		t.Fatal("encryption key hot-swapped")
	}
	if r.Current().TokenEncryptionKey == cfg.TokenEncryptionKey {
		t.Fatal("failed reload changed key")
	}
	t.Setenv("MODEL_GATEWAY_TOKEN_ENCRYPTION_KEY", cfg.TokenEncryptionKey)
	parsed, err := Parse([]byte(`{"database_url":"test","api_token":"aaaaaaaaaaaaaaaaaaaaaaaa","admin_token":"bbbbbbbbbbbbbbbbbbbbbbbb"}`))
	if err != nil || parsed.TokenEncryptionKey != cfg.TokenEncryptionKey {
		t.Fatalf("env key not loaded: %v", err)
	}
}
