package config

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestConfigurationCentersRefresh(t *testing.T) {
	for _, source := range []string{"appconfig", "nacos"} {
		t.Run(source, func(t *testing.T) {
			var logins atomic.Int32
			var version atomic.Int32
			var revoked atomic.Bool
			center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/nacos/v1/auth/login" {
					if r.Method != "POST" || r.FormValue("username") != "worker" || r.FormValue("password") != "password" {
						t.Error("incorrect Nacos login")
					}
					logins.Add(1)
					_, _ = w.Write([]byte(`{"accessToken":"test-access-token","tokenTtl":18000}`))
					return
				}
				if source == "nacos" && (r.URL.Query().Get("accessToken") != "test-access-token" || r.URL.Query().Get("dataId") != "gateway.json" || r.URL.Query().Get("tenant") != "cn-dev") {
					t.Error("incorrect Nacos configuration coordinates")
				}
				if revoked.Swap(false) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]int32{"version": version.Load()})
			}))
			defer center.Close()
			bootstrap := Bootstrap{Source: source, URL: center.URL + "/applications/gateway/environments/dev/configurations/config"}
			if source == "nacos" {
				bootstrap.URL = center.URL + "/nacos/v1/cs/configs?dataId=gateway.json&group=DEFAULT_GROUP&tenant=cn-dev"
				bootstrap.Username, bootstrap.Password = "worker", "password"
			}
			path := filepath.Join(t.TempDir(), "bootstrap.json")
			data, _ := json.Marshal(bootstrap)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			loader := NewLoader(path)
			defer loader.Close()
			for _, n := range []int32{1, 2} {
				version.Store(n)
				raw, err := loader.Load(context.Background())
				var got map[string]int32
				if err != nil || json.Unmarshal(raw, &got) != nil || got["version"] != n {
					t.Fatalf("configuration did not refresh: %s %v", raw, err)
				}
			}
			if source == "nacos" {
				if logins.Load() != 1 {
					t.Fatal("Nacos token was not reused")
				}
				revoked.Store(true)
				if _, err := loader.Load(context.Background()); err == nil {
					t.Fatal("revoked token accepted")
				}
				if _, err := loader.Load(context.Background()); err != nil || logins.Load() != 2 {
					t.Fatal("Nacos token did not recover")
				}
			}
		})
	}
}
