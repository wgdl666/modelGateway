package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/testsupport"
	"github.com/wgdl666/modelGateway/internal/worker"
)

// Real HTTP -> PostgreSQL -> RabbitMQ -> model -> object store -> HTTP. Model
// names occur only in this test; registration must be sufficient for dispatch.
func TestGenericGenerationEndToEnd(t *testing.T) {
	ledger, _, db := stores(t)
	cfg := testConfig()
	cfg.RabbitMQURL, cfg.RabbitMQQueue, _ = testsupport.RabbitQueue(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	cfg.BackendToken = "generic-backend-token"
	cfg.PollInterval = 20 * time.Millisecond
	cfg.NativeExecutionTimeout = 5 * time.Second
	png := []byte("\x89PNG\r\n\x1a\ngeneric output")
	video := []byte("generic video output")
	var objectsMu sync.Mutex
	objects := map[string][]byte{}
	objectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		objectsMu.Lock()
		defer objectsMu.Unlock()
		switch r.Method {
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			objects[r.URL.Path] = data
			w.Header().Set("ETag", `"test-etag"`)
		case http.MethodGet:
			data, ok := objects[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(data)
		default:
			t.Errorf("unexpected object method %s", r.Method)
			w.WriteHeader(405)
		}
	}))
	defer objectServer.Close()
	cfg.NativeResults = config.NativeResults{Provider: "s3", Region: "us-east-2", Bucket: "test-results", Endpoint: objectServer.URL}
	var imageCalls, videoCalls atomic.Int32
	var finishVideo atomic.Bool
	parameters := `{"seed":9007199254740993,"enhance":false,"adapter":{"name":"custom","strength":0.75}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cfg.BackendToken {
			t.Error("missing backend authentication")
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /pool/health":
			_, _ = io.WriteString(w, `{"ready":true}`)
		case "POST /pool/v1/images/generations":
			imageCalls.Add(1)
			var body protocol.ImageGenerationRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if body.Model != "new-image-A" && body.Model != "new-image-B" {
				t.Errorf("wrong model %s", body.Model)
			}
			if body.Prompt == "reject" {
				w.WriteHeader(422)
				return
			}
			if !protocol.ValidID(r.Header.Get("Idempotency-Key")) || r.Header.Get("Idempotency-Key") != r.Header.Get("X-Gateway-Request-ID") {
				t.Error("missing stable request identity")
			}
			if body.ResponseFormat != "b64_json" || body.N == nil || *body.N != 2 || body.Size != "1600x900" || !sameGenerationJSON(body.Parameters, []byte(parameters)) {
				t.Errorf("image parameters changed: %+v", body)
			}
			if body.Prompt == "combine" && (len(body.Images) != 2 || string(body.Images[0].Data) != "reference-0" || string(body.Images[1].Data) != "reference-1") {
				t.Errorf("references changed: %+v", body.Images)
			}
			if body.User != "caller" || r.Header.Get("X-Trace-ID") != "generation-test" {
				t.Error("lost metadata")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string][]byte{{"b64_json": png}, {"b64_json": png}}})
		case "POST /pool/v1/videos":
			videoCalls.Add(1)
			var body protocol.VideoGenerationRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if body.Model != "new-video" || body.Seconds == nil || *body.Seconds != 4.5 || !sameGenerationJSON(body.Parameters, []byte(`{"audio":true,"fps":24}`)) {
				t.Errorf("video input changed: %+v", body)
			}
			if body.Prompt == "animate" && (len(body.Images) != 1 || string(body.Images[0].Data) != "x") {
				t.Error("video reference lost")
			}
			job := "job-ok"
			if body.Prompt == "fail" {
				job = "job-failed"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": job, "status": "queued"})
		case "GET /pool/v1/videos/job-ok":
			status := "in_progress"
			if finishVideo.Load() {
				status = "completed"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-ok", "status": status})
		case "GET /pool/v1/videos/job-failed":
			_, _ = io.WriteString(w, `{"id":"job-failed","status":"failed"}`)
		case "GET /pool/v1/videos/job-ok/content":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(video)
		default:
			t.Errorf("unexpected backend route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	cfg.AllowedPorts = append(cfg.AllowedPorts, u.Port())
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(ledger, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	base := startHTTPServer(t, httpapi.New(ledger, runtime, reconciler.ProbeInstance))
	client := &http.Client{Timeout: 8 * time.Second}
	defer client.CloseIdleConnections()
	call := func(method, path, token, id, contentType string, body []byte, want int) ([]byte, http.Header) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Trace-ID", "generation-test")
		if id != "" {
			req.Header.Set("Idempotency-Key", id)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s %s: %d %s, err=%v, want=%d", method, path, res.StatusCode, raw, err, want)
		}
		return raw, res.Header
	}
	for _, item := range []struct{ model, kind string }{{"new-image-A", protocol.ImageGenerationProtocol}, {"new-image-B", protocol.ImageGenerationProtocol}, {"new-video", protocol.VideoGenerationProtocol}, {"legacy-body", protocol.BodyProtocol}} {
		spec := testInstance(item.model, backend.URL+"/pool", 1)
		spec.Protocol = item.kind
		ensureTestGroup(t, ledger, spec)
		if item.model == "legacy-body" {
			// Only the new generic routes reject legacy business registrations.
			if err := ledger.Register(t.Context(), spec, ""); err != nil {
				t.Fatal(err)
			}
			continue
		}
		raw, _ := json.Marshal(spec)
		call("POST", "/v1/instances/register", adminToken, "", "application/json", raw, 200)
	}
	reconciler.Start()
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/v1/images/generations", `{"model":"missing","prompt":"x"}`, 404},
		{"/v1/images/generations", `{"model":"new-video","prompt":"x"}`, 400},
		{"/v1/videos", `{"model":"new-image-A","prompt":"x"}`, 400},
		{"/v1/images/generations", `{"model":"legacy-body","prompt":"x"}`, 400},
	} {
		id := uuid.NewString()
		call("POST", tc.path, apiToken, id, "application/json", []byte(tc.body), tc.status)
		if _, err := db.Request.Get(t.Context(), id); err == nil {
			t.Fatal("rejected request reached queue")
		}
	}
	checkImages := func(raw []byte) {
		t.Helper()
		var result struct {
			Data []struct {
				Bytes []byte `json:"b64_json"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Data) != 2 || !bytes.Equal(result.Data[0].Bytes, png) || !bytes.Equal(result.Data[1].Bytes, png) {
			t.Fatalf("image result: %s", raw)
		}
	}
	for _, model := range []string{"new-image-A", "new-image-B"} {
		input := []byte(`{"model":"` + model + `","prompt":"draw","n":2,"size":"1600x900","user":"caller","parameters":` + parameters + `}`)
		raw, h := call("POST", "/v1/images/generations", apiToken, "", "application/json", input, 200)
		checkImages(raw)
		id := h.Get("X-Gateway-Request-ID")
		if !protocol.ValidID(id) {
			t.Fatal("missing generated request ID")
		}
		before := imageCalls.Load()
		raw, _ = call("POST", "/v1/images/generations", apiToken, id, "application/json", input, 200)
		checkImages(raw)
		if imageCalls.Load() != before {
			t.Fatal("idempotent replay executed again")
		}
		call("POST", "/v1/images/generations", apiToken, id, "application/json", bytes.Replace(input, []byte("draw"), []byte("changed"), 1), 409)
		detail, _ := call("GET", "/v1/admin/requests/"+id, adminToken, "", "", nil, 200)
		if !strings.Contains(string(detail), `"seed":9007199254740993`) {
			t.Fatalf("text-only preview: %s", detail)
		}
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	// Build explicit MIME types, preserving the order of references.
	for k, v := range map[string]string{"model": "new-image-A", "prompt": "combine", "n": "2", "size": "1600x900", "user": "caller", "parameters": parameters} {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 2 {
		part, err := writer.CreatePart(map[string][]string{"Content-Disposition": {`form-data; name="image[]"; filename="ref.png"`}, "Content-Type": {"image/png"}})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(part, "reference-"+strconv.Itoa(i))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := call("POST", "/v1/images/edits", apiToken, "", writer.FormDataContentType(), form.Bytes(), 200)
	checkImages(raw)
	call("POST", "/v1/images/generations", apiToken, "", "application/json", []byte(`{"model":"new-image-A","prompt":"reject"}`), 400)
	for _, prompt := range []string{"move", "animate", "fail"} {
		finishVideo.Store(false)
		refs := ""
		if prompt == "animate" {
			refs = `,"images":[{"mime_type":"image/png","data":"eA=="}]`
		}
		raw, _ := call("POST", "/v1/videos", apiToken, "", "application/json", []byte(`{"model":"new-video","prompt":"`+prompt+`","seconds":4.5,"parameters":{"audio":true,"fps":24}`+refs+`}`), 200)
		var job struct{ ID, Model, Status string }
		if json.Unmarshal(raw, &job) != nil || job.Model != "new-video" || !protocol.ValidID(job.ID) {
			t.Fatalf("video identity: %s", raw)
		}
		if prompt != "fail" {
			call("GET", "/v1/videos/"+job.ID+"/content", apiToken, "", "", nil, 409)
		}
		finishVideo.Store(true)
		want := "completed"
		if prompt == "fail" {
			want = "failed"
		}
		waitForNativeTest(t, "video completion", func() bool {
			raw, _ := call("GET", "/v1/videos/"+job.ID, apiToken, "", "", nil, 200)
			if json.Unmarshal(raw, &job) != nil {
				t.Fatalf("video status: %s", raw)
			}
			return job.Status == want
		})
		if want == "completed" {
			raw, h := call("GET", "/v1/videos/"+job.ID+"/content", apiToken, "", "", nil, 200)
			if !bytes.Equal(raw, video) || h.Get("Content-Type") != "video/mp4" {
				t.Fatal("video content changed")
			}
		}
	}
	if videoCalls.Load() != 3 {
		t.Fatalf("video dispatch count: %d", videoCalls.Load())
	}
}

func sameGenerationJSON(a, b []byte) bool {
	var left, right any
	decode := func(raw []byte, target *any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		return d.Decode(target)
	}
	return decode(a, &left) == nil && decode(b, &right) == nil && reflect.DeepEqual(left, right)
}
