package worker

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestNativeVideoHealthUsesRegisteredGPUWithAnyBasePath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ready":true,"instances":[{"name":"worker-seven","gpus":[7]}]}`)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	runtime, err := config.NewRuntime(config.Config{DatabaseURL: "test",
		APIToken: "test-api-token-at-least-24-chars", AdminToken: "test-admin-token-at-least-24-chars",
		AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{u.Port()},
		NativeModels: map[string]config.NativeBackend{protocol.VideoModel: {Video: true}}})
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &Reconciler{client: server.Client(), runtime: runtime}
	for _, test := range []struct {
		path    string
		devices []int
		ready   bool
	}{
		{"", []int{7}, true},
		{"/models/ltx-worker", []int{7}, true},
		{"", []int{3}, false},
		{"/gpu7", nil, true},
		{"/gpu3", nil, false},
		{"/gpu3", []int{7}, false},
	} {
		target := protocol.InstanceTarget{ModelKey: protocol.VideoModel, Endpoint: server.URL + test.path,
			HealthURL: server.URL + test.path + "/healthz", GPUDevices: test.devices}
		result, err := reconciler.ProbeInstance(t.Context(), runtime.Current(), target)
		if err != nil || result.Ready != test.ready {
			t.Errorf("%+v: got %+v, %v", target, result, err)
		}
	}
}
