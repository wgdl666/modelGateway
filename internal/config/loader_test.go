package config

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

func TestAppConfigListen(t *testing.T) {
	const resource = "/applications/gateway/environments/dev/configurations/config"
	for _, region := range []string{"SG", "US"} {
		t.Run(region, func(t *testing.T) {
			t.Setenv("XX_WG_REGION", region)
			var body atomic.Value
			body.Store(configDocument(":18580", 10))
			center := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != resource {
					t.Errorf("unexpected AppConfig request: %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write(body.Load().([]byte))
			}))
			defer center.Close()
			path := filepath.Join(t.TempDir(), "bootstrap.json")
			bootstrap, _ := json.Marshal(Bootstrap{AppConfigURL: center.URL + resource})
			if err := os.WriteFile(path, bootstrap, 0600); err != nil {
				t.Fatal(err)
			}
			loader, err := NewLoader(path)
			if err != nil {
				t.Fatal(err)
			}
			defer loader.Close()
			runtime, err := loader.Load(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			loader.pollInterval = 10 * time.Millisecond
			stop, err := loader.Listen(t.Context(), runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			body.Store(configDocument(":18580", 20))
			waitForQueueLimit(t, runtime, 20)
		})
	}
}

type fakeNacosClient struct {
	body        atomic.Value
	listening   vo.ConfigParam
	unlistening vo.ConfigParam
}

func (n *fakeNacosClient) GetConfig(vo.ConfigParam) (string, error) {
	return n.body.Load().(string), nil
}

func (n *fakeNacosClient) ListenConfig(key vo.ConfigParam) error {
	n.listening = key
	return nil
}

func (n *fakeNacosClient) CancelListenConfig(key vo.ConfigParam) error {
	n.unlistening = key
	return nil
}

func (*fakeNacosClient) CloseClient() {}

func TestNacosListenRefreshesCurrentDocument(t *testing.T) {
	nacos := &fakeNacosClient{}
	nacos.body.Store(string(configDocument(":18580", 10)))
	key := vo.ConfigParam{DataId: "gateway.json", Group: "DEFAULT_GROUP"}
	loader := &Loader{nacos: nacos, nacosKey: key}
	defer loader.Close()
	runtime, err := loader.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stop, err := loader.Listen(t.Context(), runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("notification", func(t *testing.T) {
		if nacos.listening.DataId != key.DataId || nacos.listening.Group != key.Group || nacos.listening.OnChange == nil {
			t.Fatal("did not subscribe to the configured Nacos document")
		}
		nacos.body.Store(string(configDocument(":18580", 30)))
		// 模拟旧回调晚于新版本到达，网关应读取配置中心当前正文。
		nacos.listening.OnChange("tenant", key.Group, key.DataId, string(configDocument(":18580", 20)))
		waitForQueueLimit(t, runtime, 30)
	})
	stop()
	if nacos.unlistening.DataId != key.DataId || nacos.unlistening.Group != key.Group {
		t.Fatal("did not unsubscribe from Nacos on stop")
	}
}

func TestReloadKeepsActiveConfigWhenAddressIsOccupied(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg, err := Parse(configDocument("127.0.0.1:18580", 10))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.Current()
	err = applyConfig(configDocument(busy.Addr().String(), 20), runtime, func(net.Listener) {
		t.Error("replaced the listener although the new address was occupied")
	})
	if err == nil || runtime.Current() != before {
		t.Fatal("occupied address changed the active configuration")
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

func configDocument(listen string, maxWaiting int) []byte {
	raw, _ := json.Marshal(Config{
		DatabaseURL:     "postgresql://localhost/gateway_test",
		Listen:          listen,
		APIToken:        "test-api-token-at-least-24-characters",
		AdminToken:      "test-admin-token-at-least-24-characters",
		AllowedNetworks: []string{"127.0.0.0/8"},
		AllowedPorts:    []string{"8001"},
		MaxWaiting:      maxWaiting,
	})
	return raw
}

func waitForQueueLimit(t *testing.T, runtime *Runtime, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.Current().MaxWaiting == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue limit = %d, want %d", runtime.Current().MaxWaiting, want)
}
