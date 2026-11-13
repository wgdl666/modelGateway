package integration

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/testsupport"
	"github.com/wgdl666/modelGateway/internal/worker"
)

// Run VMind-Image/tests/generic_gateway_fixture.py, then set VMIND_IMAGE_TEST_URL.
// This verifies real Python request parsing/routing and Go admission/dispatch.
func TestVMindImageContract(t *testing.T) {
	upstream := os.Getenv("VMIND_IMAGE_TEST_URL")
	if upstream == "" {
		t.Skip("VMIND_IMAGE_TEST_URL required for cross-repository HTTP contract")
	}
	ledger, _, _ := stores(t)
	cfg := testConfig()
	cfg.RabbitMQURL, cfg.RabbitMQQueue, _ = testsupport.RabbitQueue(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	var mu sync.Mutex
	objects := map[string][]byte{}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "PUT" {
			raw, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = raw
			w.Header().Set("ETag", `"test"`)
			return
		}
		raw, ok := objects[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
	}))
	defer storage.Close()
	cfg.NativeResults = config.NativeResults{Provider: "s3", Region: "us-east-2", Bucket: "results", Endpoint: storage.URL}
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AllowedPorts = append(cfg.AllowedPorts, u.Port())
	cfg.PollInterval = 20 * time.Millisecond
	rt, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(ledger, rt)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	base := startHTTPServer(t, httpapi.New(ledger, rt, reconciler.ProbeInstance))
	for _, model := range []string{"contract-A", "contract-B"} {
		spec := testInstance(model, upstream, 1)
		spec.Protocol = protocol.ImageGenerationProtocol
		spec.HealthURL = upstream + "/v1/images/models/" + model + "/health"
		health, err := reconciler.ProbeInstance(t.Context(), rt.Current(), protocol.InstanceTarget{ModelKey: model, Protocol: spec.Protocol, Endpoint: upstream, HealthURL: spec.HealthURL})
		if err != nil || !health.Ready {
			t.Fatalf("model-specific probe %s: %+v %v", model, health, err)
		}
		registerReady(t, ledger, spec)
	}
	reconciler.Start()
	refs := []protocol.InputImage{}
	for _, c := range []color.RGBA{{R: 255, A: 255}, {B: 255, A: 255}} {
		im := image.NewRGBA(image.Rect(0, 0, 2, 2))
		im.Set(0, 0, c)
		var buf bytes.Buffer
		if err := png.Encode(&buf, im); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, protocol.InputImage{MIMEType: "image/png", Data: buf.Bytes()})
	}
	for i, model := range []string{"contract-A", "contract-B"} {
		body, _ := json.Marshal(protocol.ImageGenerationRequest{Model: model, ImageGenerationInput: protocol.ImageGenerationInput{Prompt: "preserve this prompt", Images: refs, Size: "64x64", Parameters: json.RawMessage(`{"seed":9007199254740993}`)}})
		id := uuid.NewString()
		var first []byte
		for attempt := range 2 {
			req, _ := http.NewRequestWithContext(t.Context(), "POST", base+"/v1/images/generations", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+apiToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", id)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("%s status=%d body=%s", model, res.StatusCode, raw)
			}
			var output struct {
				Data []struct {
					Bytes []byte `json:"b64_json"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &output); err != nil || len(output.Data) != 1 {
				t.Fatalf("bad response %s", raw)
			}
			if attempt == 0 {
				first = output.Data[0].Bytes
			} else if !bytes.Equal(first, output.Data[0].Bytes) {
				t.Fatal("replay changed output")
			}
			im, err := png.Decode(bytes.NewReader(output.Data[0].Bytes))
			if err != nil {
				t.Fatal(err)
			}
			r, _, b, _ := im.At(0, 0).RGBA()
			if i == 0 && r != 65535 || i == 1 && b != 65535 {
				t.Fatalf("wrong selected worker: %s r=%d b=%d", model, r, b)
			}
		}
	}
}
