package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestGenericProbeRequiresAdvertisedModel(t *testing.T) {
	for _, body := range []string{`{"ready":true}`, `{"ready":true,"models":["other"]}`, `{"ready":true,"models":["selected"]}`} {
		t.Run(body, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer backend.Close()
			u, _ := url.Parse(backend.URL)
			rt, err := config.NewRuntime(config.Config{APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24), AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{u.Port()}})
			if err != nil {
				t.Fatal(err)
			}
			worker := &Reconciler{runtime: rt, client: backend.Client()}
			health, err := worker.ProbeInstance(context.Background(), rt.Current(), protocol.InstanceTarget{ModelKey: "selected", Protocol: protocol.ImageGenerationProtocol, Endpoint: backend.URL, HealthURL: backend.URL + "/health"})
			if err != nil || health.Ready != strings.Contains(body, `"selected"`) {
				t.Fatalf("health=%+v err=%v", health, err)
			}
		})
	}
}

func TestManagedProbeRequiresExactLoadedDeployment(t *testing.T) {
	digest := "sha256-" + strings.Repeat("a", 64)
	for _, body := range []string{
		`{"ready":true,"models":["selected"]}`,
		`{"ready":true,"models":["selected"],"deployment_id":"old","artifact_digest":"` + digest + `","boot_id":"b"}`,
		`{"ready":true,"models":["selected"],"deployment_id":"new","artifact_digest":"sha256-` + strings.Repeat("b", 64) + `","boot_id":"b"}`,
		`{"ready":true,"models":["selected"],"deployment_id":"new","artifact_digest":"` + digest + `"}`,
		`{"ready":true,"models":["selected"],"deployment_id":"new","artifact_digest":"` + digest + `","boot_id":"b"}`,
	} {
		t.Run(body, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer backend.Close()
			u, _ := url.Parse(backend.URL)
			rt, err := config.NewRuntime(config.Config{APIToken: strings.Repeat("a", 24), AdminToken: strings.Repeat("b", 24), AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{u.Port()}})
			if err != nil {
				t.Fatal(err)
			}
			w := &Reconciler{runtime: rt, client: backend.Client()}
			health, err := w.ProbeInstance(context.Background(), rt.Current(), protocol.InstanceTarget{ModelKey: "selected", Protocol: protocol.ImageGenerationProtocol, Endpoint: backend.URL, HealthURL: backend.URL + "/health", DeploymentID: "new", ArtifactDigest: digest})
			want := strings.Contains(body, `"deployment_id":"new"`) && strings.Contains(body, digest) && strings.Contains(body, `"boot_id":"b"`)
			if err != nil || health.Ready != want {
				t.Fatalf("health=%+v err=%v", health, err)
			}
		})
	}
}
