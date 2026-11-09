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
	const (
		nacosPath     = "/nacos/v1/cs/configs"
		appConfigPath = "/applications/gateway/environments/dev/configurations/config"
	)
	for _, region := range []string{"CN", "SG", "US"} {
		t.Run(region, func(t *testing.T) {
			t.Setenv("XX_WG_REGION", region)
			nacos := region == "CN"
			configPath := appConfigPath
			if nacos {
				configPath = nacosPath
			}
			var logins atomic.Int32
			var version atomic.Int32
			var revoked atomic.Bool
			center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/nacos/v1/auth/login" {
					if !nacos || r.Method != "POST" || r.FormValue("username") != "worker" || r.FormValue("password") != "password" {
						t.Error("incorrect Nacos login")
					}
					logins.Add(1)
					_, _ = w.Write([]byte(`{"accessToken":"test-access-token","tokenTtl":18000}`))
					return
				}
				if r.URL.Path != configPath {
					t.Errorf("requested %q, want %q for %s", r.URL.Path, configPath, region)
				}
				if nacos && (r.URL.Query().Get("accessToken") != "test-access-token" || r.URL.Query().Get("dataId") != "gateway.json" || r.URL.Query().Get("tenant") != "cn-dev") {
					t.Error("incorrect Nacos configuration coordinates")
				}
				if revoked.Swap(false) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]int32{"version": version.Load()})
			}))
			defer center.Close()
			bootstrap := Bootstrap{
				NacosURL:     center.URL + nacosPath + "?dataId=gateway.json&group=DEFAULT_GROUP&tenant=cn-dev",
				AppConfigURL: center.URL + appConfigPath,
				Username:     "worker",
				Password:     "password",
			}
			path := filepath.Join(t.TempDir(), "bootstrap.json")
			data, _ := json.Marshal(bootstrap)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			loader, err := NewLoader(path)
			if err != nil {
				t.Fatal(err)
			}
			defer loader.Close()
			for _, n := range []int32{1, 2} {
				version.Store(n)
				raw, err := loader.Load(context.Background())
				var got map[string]int32
				if err != nil || json.Unmarshal(raw, &got) != nil || got["version"] != n {
					t.Fatalf("configuration did not refresh: %s %v", raw, err)
				}
			}
			if nacos {
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

func TestNewLoaderRequiresKnownRegion(t *testing.T) {
	for _, region := range []string{"", "EU"} {
		t.Run(region, func(t *testing.T) {
			t.Setenv("XX_WG_REGION", region)
			if loader, err := NewLoader("bootstrap.json"); err == nil {
				loader.Close()
				t.Fatal("accepted missing or unsupported XX_WG_REGION")
			}
		})
	}
}
