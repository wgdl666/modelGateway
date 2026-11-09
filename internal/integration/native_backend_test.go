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

			const bucket, target, jobID = "test-results", "ltx-gpu1", "ltx-job-1"
			var targetAdvertised atomic.Bool
			targetAdvertised.Store(provider != "oss")
			png := []byte("\x89PNG\r\n\x1a\nimage")
			video := []byte("video fixture")
			imageID, videoID := uuid.NewString(), uuid.NewString()
			var imageCalls, videoPolls atomic.Int32
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
				switch r.Method + " " + r.URL.Path {
				case "GET /health":
					if !targetAdvertised.Load() {
						_, _ = io.WriteString(w, `{"ready":true,"instances":[{"name":"ltx-gpu0"}]}`)
						return
					}
					_, _ = fmt.Fprintf(w, `{"ready":true,"instances":[{"name":%q}]}`, target)
				case "POST /vton":
					if r.FormValue("target_instance") != target || r.FormValue("motion_tips") != "turn" ||
						r.Header.Get("X-Gateway-Request-ID") != videoID || r.Header.Get("X-Gateway-Dispatch") != "immediate" {
						t.Error("video request lost target or model fields")
					}
					checkNativeTestFile(t, r, "image", "first frame")
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
				case "GET /jobs/" + jobID:
					status := "processing"
					if videoPolls.Add(1) > 1 {
						status = "done"
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"job_id": jobID, "status": status})
				case "GET /jobs/" + jobID + "/video":
					_, _ = w.Write(video)
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
				case "/" + bucket + "/outputs/" + videoID + "/0.mp4":
					if !bytes.Equal(body, video) || r.Header.Get("Content-Type") != "video/mp4" {
						t.Error("uploaded video did not match model output")
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
			cfg.NativeBackends = map[string]config.NativeBackend{imageModel.URL: {}, videoModelServer.URL: {Video: true}}
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
			imageSpec := testInstance(protocol.OutfitModel, imageModel.URL, 1)
			videoSpec := testInstance(protocol.VideoModel, videoModelServer.URL, 1)
			videoSpec.ResourceGroup = "host/gpu1"
			if provider == "oss" {
				raw, _ := json.Marshal(videoSpec)
				call(http.MethodPost, "/v1/instances/register", adminToken, "", string(raw), 400)
			}
			videoSpec.TargetInstance = target
			for _, spec := range []protocol.Instance{imageSpec, videoSpec} {
				raw, _ := json.Marshal(spec)
				call(http.MethodPost, "/v1/instances/register", adminToken, "", string(raw), 200)
			}
			if provider == "oss" {
				// 同一个登记 ID 不能把已经绑定的 GPU 目标改到另一卡。
				changed := videoSpec
				changed.TargetInstance = "ltx-gpu0"
				raw, _ := json.Marshal(changed)
				call(http.MethodPost, "/v1/instances/register", adminToken, "", string(raw), 409)
			}
			reconciler.Start()
			if provider == "oss" {
				waitForNativeTest(t, "probe of the unadvertised video target", func() bool {
					row, err := db.ModelInstance.Get(t.Context(), videoSpec.ID)
					if err != nil || row.HealthCheckedAt.Unix() == 0 {
						return false
					}
					if row.Ready {
						t.Fatal("probe marked the registration ready with only another LTX target advertised")
					}
					return true
				})
				targetAdvertised.Store(true)
			}
			waitForNativeTest(t, "model readiness", func() bool {
				count, err := db.ModelInstance.Query().Where(modelinstance.ReadyEQ(true)).Count(t.Context())
				return err == nil && count == 2
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
			call(http.MethodPost, "/v1/gateway/requests/"+protocol.VideoModel, apiToken, videoID, videoBody, 202)
			var response protocol.Response
			waitForNativeTest(t, "video terminal response", func() bool {
				response = call(http.MethodGet, "/v1/gateway/requests", apiToken, videoID, "", 200)
				return response.State == "succeeded" || response.State == "failed"
			})
			assertResult(response, videoID, "0.mp4")
			row, err := db.Request.Get(t.Context(), videoID)
			if err != nil || row.InstanceID != videoSpec.ID || row.ResourceGroup != videoSpec.ResourceGroup ||
				row.BackendTaskID != jobID || row.SlotReserved || len(uploads) != 2 {
				t.Fatalf("unexpected video record or upload count: row=%+v uploads=%d error=%v", row, len(uploads), err)
			}

			if provider == "oss" {
				rejected, disconnected := uuid.NewString(), uuid.NewString()
				call(http.MethodPost, imagePath, apiToken, rejected, strings.Replace(imageBody, "try on", "reject", 1), 502)
				disconnectedBody := strings.Replace(imageBody, "try on", "disconnect", 1)
				call(http.MethodPost, imagePath, apiToken, disconnected, disconnectedBody, 502)
				call(http.MethodPost, imagePath, apiToken, disconnected, disconnectedBody, 502)
				rejectedRow, err := db.Request.Get(t.Context(), rejected)
				if err != nil || rejectedRow.SlotReserved || rejectedRow.ErrorCode != "model_rejected" {
					t.Fatalf("model rejection did not release its slot: %v (%v)", rejectedRow, err)
				}
				disconnectedRow, err := db.Request.Get(t.Context(), disconnected)
				if err != nil || !disconnectedRow.SlotReserved || disconnectedRow.ErrorCode != "backend_outcome_unknown" || imageCalls.Load() != 3 {
					t.Fatalf("unknown image outcome lost its slot or was sent again: %v (%v)", disconnectedRow, err)
				}
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
	native.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, status := r.PathValue("id"), "done"
		if id == checkJob {
			status = "error"
		}
		_, _ = fmt.Fprintf(w, `{"job_id":%q,"status":%q}`, id, status)
	})
	native.HandleFunc("GET /jobs/{id}/video", func(w http.ResponseWriter, r *http.Request) {
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
	cfg.NativeBackends = map[string]config.NativeBackend{model.URL: {Video: true}}
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
			SetEndpoint(model.URL).SetBackendTaskID(jobID).SetDeadlineAt(now.Add(time.Minute)).SetNextPollAt(now).Exec(t.Context())
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
