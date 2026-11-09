package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/testsupport"
	"github.com/wgdl666/modelGateway/internal/worker"
)

// 从登记和 HTTP 受理开始验证真实派发、轮询和状态写入，模型与对象存储只模拟外部协议。
func TestNativeBackendsThroughGateway(t *testing.T) {
	for _, provider := range []string{"oss", "s3"} {
		t.Run(provider, func(t *testing.T) {
			ledger, _, db := stores(t)
			t.Setenv("OSS_ACCESS_KEY_ID", "test-access-key")
			t.Setenv("OSS_ACCESS_KEY_SECRET", "test-secret")
			t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			t.Setenv("MODEL_GATEWAY_NATIVE_IMAGE_USERNAME", "image-user")
			t.Setenv("MODEL_GATEWAY_NATIVE_IMAGE_PASSWORD", "image-password")
			t.Setenv("MODEL_GATEWAY_NATIVE_IMAGE_TOKEN", "")
			t.Setenv("MODEL_GATEWAY_NATIVE_VIDEO_USERNAME", "")
			t.Setenv("MODEL_GATEWAY_NATIVE_VIDEO_PASSWORD", "")
			t.Setenv("MODEL_GATEWAY_NATIVE_VIDEO_TOKEN", "video-token")

			const bucket = "test-results"
			var matchingProbe atomic.Bool
			matchingProbe.Store(provider != "oss")
			png := []byte("\x89PNG\r\n\x1a\nimage")
			videos := [2][]byte{[]byte("video from gpu0"), []byte("video from gpu1")}
			imageID := uuid.NewString()
			videoIDs := [2]string{uuid.NewString(), uuid.NewString()}
			jobIDs := [2]string{"ltx-job-0", "ltx-job-1"}
			var imageCalls atomic.Int32
			var videoAccepted [2]atomic.Bool
			imageModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != "image-user" || password != "image-password" {
					t.Error("invalid native image authentication")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.Method + " " + r.URL.Path {
				case "GET /health":
					_, _ = io.WriteString(w, `{"status":"ok","workers":[{"healthy":true}]}`)
				case "POST /v2/images/vton":
					imageCalls.Add(1)
					switch r.FormValue("prompt") {
					case "reject":
						w.WriteHeader(http.StatusUnprocessableEntity)
						return
					case "route-not-found":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `{"detail":"Not Found"}`)
						return
					case "unknown-404":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `{"detail":"garment not found"}`)
						return
					case "disconnect":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					if r.Header.Get("X-Gateway-Request-ID") != imageID ||
						r.Header.Get("X-Gateway-Dispatch") != "immediate" ||
						r.Header.Get("X-Trace-ID") != "test-trace" || r.Header.Get("X-User-ID") != "test-user" ||
						r.FormValue("trace_id") != "test-trace" || r.FormValue("prompt") != "try on" {
						t.Error("image request lost model fields or metadata")
					}
					checkNativeTestFile(t, r, "person_image", "person")
					checkNativeTestFile(t, r, "garment_image", "garment")
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string][]byte{{"b64_json": png}}})
				default:
					t.Errorf("unexpected image call %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer imageModel.Close()
			videoModelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Token") != "video-token" || r.Header.Get("Authorization") != "" {
					t.Error("invalid native video authentication")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				prefix, route, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
				gpu, ok := map[string]int{"gpu0": 0, "gpu1": 1}[prefix]
				if !ok {
					t.Errorf("video request lost its GPU prefix: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				switch r.Method + " /" + route {
				case "GET /healthz":
					reportedGPU := gpu
					if gpu == 1 && !matchingProbe.Load() {
						reportedGPU = 0
					}
					_, _ = fmt.Fprintf(w, `{"ready":true,"instances":[{"name":"ltx-gpu%d","gpus":[%d]}]}`, reportedGPU, reportedGPU)
				case "POST /vton":
					if r.FormValue("motion_tips") != "turn" || r.Header.Get("X-Gateway-Request-ID") != videoIDs[gpu] ||
						r.Header.Get("X-Gateway-Dispatch") != "immediate" {
						t.Errorf("video request was dispatched to the wrong GPU or lost its business fields: %s", r.URL.Path)
					}
					checkNativeTestFile(t, r, "image", "first frame")
					if _, sent := r.MultipartForm.Value["target_instance"]; sent {
						t.Error("GPU selection must come from the address")
					}
					videoAccepted[gpu].Store(true)
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobIDs[gpu]})
				case "GET /jobs/" + jobIDs[gpu]:
					status := "processing"
					// 保持 GPU 0 的任务占槽，确认第二个任务派到同地址的 GPU 1 路径。
					if videoAccepted[0].Load() && videoAccepted[1].Load() {
						status = "done"
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobIDs[gpu], "status": status})
				case "GET /jobs/" + jobIDs[gpu] + "/video":
					_, _ = w.Write(videos[gpu])
				default:
					t.Errorf("unexpected video call %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer videoModelServer.Close()
			uploads := make(chan string, 4)
			objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPut {
					t.Errorf("invalid object request: %s %v", r.Method, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch r.URL.Path {
				case "/" + bucket + "/outputs/" + imageID + "/0.png":
					if !bytes.Equal(body, png) || r.Header.Get("Content-Type") != "image/png" {
						t.Error("uploaded image did not match model output")
					}
				case "/" + bucket + "/outputs/" + videoIDs[0] + "/0.mp4",
					"/" + bucket + "/outputs/" + videoIDs[1] + "/0.mp4":
					gpu := 0
					if strings.Contains(r.URL.Path, videoIDs[1]) {
						gpu = 1
					}
					if !bytes.Equal(body, videos[gpu]) || r.Header.Get("Content-Type") != "video/mp4" {
						t.Error("uploaded video did not come from the reserved GPU")
					}
				default:
					t.Errorf("unexpected object key %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				uploads <- r.URL.Path
				w.Header().Set("ETag", `"test-etag"`)
				w.WriteHeader(http.StatusOK)
			}))
			defer objects.Close()

			cfg := testConfig()
			cfg.RabbitMQURL, cfg.RabbitMQQueue, _ = testsupport.RabbitQueue(t)
			cfg.BackendToken = "legacy-token"
			cfg.PollInterval = 25 * time.Millisecond
			imageSpec := testInstance(protocol.OutfitModel, imageModel.URL, 1)
			videoSpecs := [2]protocol.Instance{}
			cfg.NativeBackends = map[string]config.NativeBackend{imageSpec.Endpoint: {}}
			for gpu := range videoSpecs {
				s := testInstance(protocol.VideoModel, fmt.Sprintf("%s/gpu%d", videoModelServer.URL, gpu), 1)
				s.ID = fmt.Sprintf("native-gpu%d", gpu)
				s.HealthURL = s.Endpoint + "/healthz"
				s.ResourceGroup = fmt.Sprintf("host/gpu%d", gpu)
				s.GPUDevices = []int{gpu}
				videoSpecs[gpu] = s
				cfg.NativeBackends[s.Endpoint] = config.NativeBackend{Video: true}
			}
			cfg.NativeResults = config.NativeResults{Provider: provider, Region: "us-east-2", Bucket: bucket, Endpoint: objects.URL}
			for endpoint := range cfg.NativeBackends {
				u, _ := url.Parse(endpoint)
				cfg.AllowedPorts = append(cfg.AllowedPorts, u.Port())
			}
			runtime, err := config.NewRuntime(cfg)
			if err != nil {
				t.Fatal(err)
			}
			reconciler, err := worker.New(ledger, runtime)
			if err != nil {
				t.Fatal(err)
			}
			base := startHTTPServer(t, httpapi.New(ledger, runtime))
			defer reconciler.Close()
			client := &http.Client{Timeout: 7 * time.Second}
			defer client.CloseIdleConnections()
			call := func(method, path, token, id, body string, want int) protocol.Response {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Idempotency-Key", id)
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				var response protocol.Response
				if err := json.NewDecoder(res.Body).Decode(&response); err != nil || res.StatusCode != want {
					t.Fatalf("%s %s: got HTTP %d and %+v (%v), want %d", method, path, res.StatusCode, response, err, want)
				}
				return response
			}
			if provider == "oss" {
				wrong := videoSpecs[0]
				wrong.HealthURL = videoSpecs[1].HealthURL
				raw, _ := json.Marshal(wrong)
				call(http.MethodPost, "/v1/instances/register", adminToken, "", string(raw), 400)
				wrong = videoSpecs[0]
				wrong.GPUDevices = videoSpecs[1].GPUDevices
				raw, _ = json.Marshal(wrong)
				call(http.MethodPost, "/v1/instances/register", adminToken, "", string(raw), 400)
			}
			for _, spec := range []protocol.Instance{imageSpec, videoSpecs[0], videoSpecs[1]} {
				ensureTestGroup(t, ledger, spec)
				raw, _ := json.Marshal(spec)
				call(http.MethodPost, "/v1/instances/register", adminToken, "", string(raw), 200)
			}
			if provider == "oss" {
				// 同一个登记 ID 不能把已绑定的地址改到另一卡。
				changed := videoSpecs[1]
				changed.Endpoint, changed.HealthURL = videoSpecs[0].Endpoint, videoSpecs[0].HealthURL
				changed.ResourceGroup, changed.GPUDevices = videoSpecs[0].ResourceGroup, videoSpecs[0].GPUDevices
				raw, _ := json.Marshal(changed)
				call(http.MethodPost, "/v1/instances/register", adminToken, "", string(raw), 409)
			}
			reconciler.Start()
			if provider == "oss" {
				waitForNativeTest(t, "probe reporting the wrong GPU", func() bool {
					row, err := db.ModelInstance.Get(t.Context(), videoSpecs[1].ID)
					if err != nil || row.HealthCheckedAt.Unix() == 0 {
						return false
					}
					if row.Ready {
						t.Fatal("probe marked the registration ready for another GPU")
					}
					return true
				})
				matchingProbe.Store(true)
			}
			waitForNativeTest(t, "model readiness", func() bool {
				count, err := db.ModelInstance.Query().Where(modelinstance.ReadyEQ(true)).Count(t.Context())
				return err == nil && count == 3
			})

			assertResult := func(response protocol.Response, id, filename string) {
				t.Helper()
				want := protocol.Object{Provider: provider, Region: cfg.NativeResults.Region, Bucket: bucket, Key: "outputs/" + id + "/" + filename}
				if response.RequestID != id || response.State != "succeeded" || response.Result == nil ||
					len(response.Result.Files) != 1 || response.Result.Files[0] != want {
					t.Fatalf("unexpected final response %+v, want %+v", response, want)
				}
			}
			imageBody := `{"input":{"prompt":"try on","images":[{"mime_type":"image/png","data":"cGVyc29u"},{"mime_type":"image/png","data":"Z2FybWVudA=="}]},"metadata":{"trace_id":"test-trace","user_id":"test-user"}}`
			imagePath := "/v1/gateway/requests/" + protocol.OutfitModel
			assertResult(call(http.MethodPost, imagePath, apiToken, imageID, imageBody, 200), imageID, "0.png")
			videoBody := `{"input":{"motion_tips":"turn","images":[{"mime_type":"image/png","data":"Zmlyc3QgZnJhbWU="}]}}`
			videoPath := "/v1/gateway/requests/" + protocol.VideoModel
			call(http.MethodPost, videoPath, apiToken, videoIDs[0], videoBody, 202)
			waitForNativeTest(t, "first GPU reservation", func() bool {
				row, err := db.Request.Get(t.Context(), videoIDs[0])
				return err == nil && row.InstanceID == videoSpecs[0].ID && row.BackendTaskID == jobIDs[0]
			})
			call(http.MethodPost, videoPath, apiToken, videoIDs[1], videoBody, 202)
			for gpu, id := range videoIDs {
				var response protocol.Response
				waitForNativeTest(t, "video terminal response", func() bool {
					response = call(http.MethodGet, "/v1/gateway/requests", apiToken, id, "", 200)
					return response.State == "succeeded" || response.State == "failed"
				})
				assertResult(response, id, "0.mp4")
				row, err := db.Request.Get(t.Context(), id)
				if err != nil || row.InstanceID != videoSpecs[gpu].ID || row.ResourceGroup != videoSpecs[gpu].ResourceGroup ||
					row.Endpoint != videoSpecs[gpu].Endpoint || row.BackendTaskID != jobIDs[gpu] || row.SlotReserved {
					t.Fatalf("video did not keep its reserved GPU through completion: row=%+v error=%v", row, err)
				}
			}
			if len(uploads) != 3 {
				t.Fatalf("expected one image and two video uploads, got %d", len(uploads))
			}

			if provider == "oss" {
				for _, rejection := range []struct{ prompt, code string }{
					{"reject", "model_rejected"},
					{"route-not-found", "native_route_unavailable"},
				} {
					id := uuid.NewString()
					call(http.MethodPost, imagePath, apiToken, id, strings.Replace(imageBody, "try on", rejection.prompt, 1), 502)
					row, err := db.Request.Get(t.Context(), id)
					if err != nil || row.State != request.StateFailed || row.SlotReserved || row.ErrorCode != rejection.code {
						t.Fatalf("%s did not finish and release its slot: %v (%v)", rejection.prompt, row, err)
					}
				}
			}
			// 断线和无法判定的 404 都要保留原占用，重试同一请求不能再发一次。
			unknownPrompt := "disconnect"
			if provider == "s3" {
				unknownPrompt = "unknown-404"
			}
			unknownID := uuid.NewString()
			unknownBody := strings.Replace(imageBody, "try on", unknownPrompt, 1)
			callsBefore := imageCalls.Load()
			call(http.MethodPost, imagePath, apiToken, unknownID, unknownBody, 502)
			call(http.MethodPost, imagePath, apiToken, unknownID, unknownBody, 502)
			unknownRow, err := db.Request.Get(t.Context(), unknownID)
			if err != nil || !unknownRow.SlotReserved || unknownRow.ErrorCode != "backend_outcome_unknown" || imageCalls.Load() != callsBefore+1 {
				t.Fatalf("unknown image outcome lost its slot or was sent again: %v (%v)", unknownRow, err)
			}
		})
	}
}

func TestSlowNativeVideoUploadsDoNotBlockPolling(t *testing.T) {
	ledger, _, db := stores(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	const videoCount = 4
	const checkJob = "ltx-status-check"
	allowUploads := make(chan struct{})
	releaseUploads := sync.OnceFunc(func() { close(allowUploads) })
	uploadsStarted := make(chan struct{}, videoCount)
	var downloadsWhileBlocked atomic.Int32
	native := http.NewServeMux()
	native.HandleFunc("GET /gpu0/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, status := r.PathValue("id"), "done"
		if id == checkJob {
			status = "error"
		}
		_, _ = fmt.Fprintf(w, `{"job_id":%q,"status":%q}`, id, status)
	})
	native.HandleFunc("GET /gpu0/jobs/{id}/video", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-allowUploads:
		default:
			if downloadsWhileBlocked.Add(1) > 2 {
				t.Error("a third video download started while both publishers were busy")
			}
		}
		_, _ = io.WriteString(w, "video fixture")
	})
	model := httptest.NewServer(native)
	defer model.Close()
	endpoint := model.URL + "/gpu0"
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case <-allowUploads:
		case uploadsStarted <- struct{}{}:
		}
		select {
		case <-allowUploads:
			w.Header().Set("ETag", `"test-etag"`)
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer objects.Close()

	cfg := testConfig()
	cfg.RabbitMQURL, cfg.RabbitMQQueue, _ = testsupport.RabbitQueue(t)
	cfg.BackendTimeout = 30 * time.Second
	cfg.PollInterval = 50 * time.Millisecond
	cfg.NativeBackends = map[string]config.NativeBackend{endpoint: {Video: true}}
	cfg.NativeResults = config.NativeResults{Provider: "s3", Region: "us-east-2", Bucket: "test-results", Endpoint: objects.URL}
	address, _ := url.Parse(model.URL)
	cfg.AllowedPorts = append(cfg.AllowedPorts, address.Port())
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(ledger, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	defer releaseUploads()

	// 从模型已受理的账本状态开始，控制多个任务同时到期回查。
	addPollingVideo := func(jobID string) string {
		t.Helper()
		id, now := uuid.NewString(), time.Now().UTC()
		err := db.Request.Create().SetID(id).SetModelKey(protocol.VideoModel).
			SetRequestPayload(json.RawMessage(`{}`)).SetPayloadBytes(2).SetUnits(1).
			SetState(request.StateProcessing).SetSlotReserved(true).SetInstanceID("ltx-test").SetResourceGroup("test/gpu").
			SetEndpoint(endpoint).SetBackendTaskID(jobID).SetDeadlineAt(now.Add(time.Minute)).SetNextPollAt(now).Exec(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var videoIDs []string
	for i := range videoCount {
		videoIDs = append(videoIDs, addPollingVideo(fmt.Sprintf("ltx-publish-%d", i)))
	}
	reconciler.Start()
	waitForNativeTest(t, "two blocked uploads", func() bool { return len(uploadsStarted) >= 2 })
	checkID := addPollingVideo(checkJob)
	waitForNativeTest(t, "another video status while both uploads are blocked", func() bool {
		row, err := db.Request.Get(t.Context(), checkID)
		return err == nil && row.State == request.StateFailed && row.ErrorCode == "model_failed"
	})
	count, err := db.Request.Query().Where(request.IDIn(videoIDs...), request.StateEQ(request.StateProcessing), request.SlotReservedEQ(true)).Count(t.Context())
	if err != nil || count != videoCount {
		t.Fatalf("video state advanced before upload: processing=%d err=%v", count, err)
	}
	releaseUploads()
	waitForNativeTest(t, "all video results to be published", func() bool {
		count, err := db.Request.Query().Where(request.IDIn(videoIDs...), request.StateEQ(request.StateSucceeded), request.SlotReservedEQ(false)).Count(t.Context())
		return err == nil && count == videoCount
	})
}

// 上游先拿到超时后，单槽 GPU 仍要等原请求跑完才接下一单。
func TestNativeImageWaitTimeoutReleasesSlotAfterExecution(t *testing.T) {
	ledger, _, db := stores(t)
	t.Setenv("OSS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("OSS_ACCESS_KEY_SECRET", "test-secret")
	aID, bID := uuid.NewString(), uuid.NewString()
	finishA, finishedA := make(chan struct{}), make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /health":
			_, _ = io.WriteString(w, `{"status":"ok","workers":[{"ready":true,"healthy":false}]}`)
		case "POST /v1/images/edits":
			if r.FormValue("model") != "FLUX.2-klein-9B" || r.FormValue("lora_name") != "vton" ||
				r.FormValue("response_format") != "b64_json" || r.MultipartForm == nil {
				t.Error("invalid CN FLUX form")
			} else if images := r.MultipartForm.File["image"]; len(images) != 2 ||
				images[0].Filename != "person_image.png" || images[1].Filename != "garment_image.png" {
				t.Error("CN FLUX requires the two ordered image files")
			}
			switch id := r.Header.Get("X-Gateway-Request-ID"); id {
			case aID:
				select {
				case <-finishA:
					close(finishedA)
				case <-r.Context().Done():
					return
				}
			case bID:
				select {
				case <-finishedA:
				default:
					t.Error("B reached the GPU before A finished")
				}
			default:
				t.Errorf("unexpected model request %q", id)
				http.NotFound(w, r)
				return
			}
			png := []byte("\x89PNG\r\n\x1a\nimage")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string][]byte{{"b64_json": png}}})
		case "PUT /test-results/outputs/" + aID + "/0.png", "PUT /test-results/outputs/" + bID + "/0.png":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer model.Close()

	cfg := testConfig()
	cfg.RabbitMQURL, cfg.RabbitMQQueue, _ = testsupport.RabbitQueue(t)
	cfg.BackendTimeout = time.Second
	cfg.NativeExecutionTimeout = 8 * time.Second
	cfg.NativeBackends = map[string]config.NativeBackend{model.URL: {VTONEdits: true}}
	cfg.NativeResults = config.NativeResults{Provider: "oss", Region: "cn-shenzhen", Bucket: "test-results", Endpoint: model.URL}
	origin, _ := url.Parse(model.URL)
	cfg.AllowedPorts = append(cfg.AllowedPorts, origin.Port())
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(ledger, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	spec := testInstance(protocol.OutfitModel, model.URL, 1)
	ensureTestGroup(t, ledger, spec)
	if err := ledger.Register(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(ledger, runtime))
	reconciler.Start()
	// FLUX 实例依赖真实探针变为可派发，避免预置 Ready 掩盖协议差异。
	waitForNativeTest(t, "FLUX readiness", func() bool {
		row, err := db.ModelInstance.Get(t.Context(), spec.ID)
		return err == nil && row.Ready
	})
	client := &http.Client{Timeout: 4 * time.Second}
	defer client.CloseIdleConnections()

	type outcome struct {
		status   int
		response protocol.Response
		err      error
	}
	submit := func(id string) outcome {
		body := `{"input":{"images":[{"mime_type":"image/png","data":"eA=="},{"mime_type":"image/png","data":"eQ=="}]}}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/gateway/requests/"+protocol.OutfitModel, strings.NewReader(body))
		if err != nil {
			return outcome{err: err}
		}
		req.Header.Set("Authorization", "Bearer "+apiToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", id)
		res, err := client.Do(req)
		if err != nil {
			return outcome{err: err}
		}
		defer res.Body.Close()
		got := outcome{status: res.StatusCode}
		got.err = json.NewDecoder(res.Body).Decode(&got.response)
		return got
	}
	if a := submit(aID); a.err != nil || a.status != 504 || a.response.Error == nil || a.response.Error.Code != "response_wait_timeout" {
		t.Fatalf("A should hit the upstream wait deadline: %+v", a)
	}
	a, err := db.Request.Get(t.Context(), aID)
	if err != nil || a.State != request.StateProcessing || !a.SlotReserved || a.StartedAt == nil ||
		a.NextPollAt == nil || !a.NextPollAt.After(a.StartedAt.Add(cfg.NativeExecutionTimeout)) {
		t.Fatalf("A must retain its slot and execution window after upstream timeout: %v (%v)", a, err)
	}

	bOutcome := make(chan outcome, 1)
	go func() { bOutcome <- submit(bID) }()
	waitForNativeTest(t, "B queued behind A", func() bool {
		b, err := db.Request.Get(t.Context(), bID)
		return err == nil && b.State == request.StateQueued
	})
	close(finishA)
	if b := <-bOutcome; b.err != nil || b.status != 200 || b.response.State != "succeeded" {
		t.Fatalf("B should complete after A frees its slot: %+v", b)
	}
	a, err = db.Request.Get(t.Context(), aID)
	if err != nil || a.State != request.StateSucceeded || a.SlotReserved || len(a.Result) == 0 {
		t.Fatalf("A must save its late result and release its slot: %v (%v)", a, err)
	}
}

func checkNativeTestFile(t *testing.T, r *http.Request, field, want string) {
	t.Helper()
	file, header, err := r.FormFile(field)
	if err != nil {
		t.Error(err)
		return
	}
	defer file.Close()
	body, err := io.ReadAll(file)
	if err != nil || string(body) != want || header.Header.Get("Content-Type") != "image/png" {
		t.Errorf("%s: got %q (%v)", field, body, err)
	}
}

func waitForNativeTest(t *testing.T, condition string, ready func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if ready() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", condition)
}
