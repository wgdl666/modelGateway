package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/testsupport"
	"github.com/wgdl666/modelGateway/internal/worker"
)

// 复现拆衣调用已发出后的四类失败，验证真实 MQ 回队、归还名额及同一请求的后续成功。
func TestNativeWardrobeRetriesDispatchFailures(t *testing.T) {
	for _, failure := range []string{"disconnect", "timeout", "worker-error", "truncated-body"} {
		t.Run(failure, func(t *testing.T) {
			ledger, _, db := stores(t)
			t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
			id := uuid.NewString()
			var calls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /health":
					_, _ = io.WriteString(w, `{"status":"ok"}`)
				case "POST /v2/images/garment_extraction":
					if r.Header.Get("X-Gateway-Request-ID") != id || r.FormValue("garment_name") != "shirt" {
						t.Error("retry changed request identity or input")
					}
					if calls.Add(1) == 1 {
						switch failure {
						case "disconnect":
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							_ = conn.Close()
						case "timeout":
							<-r.Context().Done()
						case "worker-error":
							w.WriteHeader(http.StatusInternalServerError)
						case "truncated-body":
							w.Header().Set("Content-Length", "100")
							_, _ = io.WriteString(w, `{"data":`)
						}
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string][]byte{{"b64_json": []byte("\x89PNG\r\n\x1a\nimage")}}})
				case "PUT /test-results/outputs/" + id + "/0.png":
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", `"test-etag"`)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected Worker call %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer model.Close()
			cfg := testConfig()
			cfg.RabbitMQURL, cfg.RabbitMQQueue, _ = testsupport.RabbitQueue(t)
			cfg.BackendTimeout = 5 * time.Second
			cfg.NativeExecutionTimeout = 300 * time.Millisecond
			cfg.NativeBackends = map[string]config.NativeBackend{model.URL: {}}
			cfg.NativeResults = config.NativeResults{Provider: "s3", Region: "us-east-2", Bucket: "test-results", Endpoint: model.URL}
			origin, _ := url.Parse(model.URL)
			cfg.AllowedPorts = append(cfg.AllowedPorts, origin.Port())
			runtime, err := config.NewRuntime(cfg)
			if err != nil {
				t.Fatal(err)
			}
			spec := testInstance(protocol.WardrobeModel, model.URL, 1)
			registerReady(t, ledger, spec)
			reconciler, err := worker.New(ledger, runtime)
			if err != nil {
				t.Fatal(err)
			}
			defer reconciler.Close()
			base := startHTTPServer(t, httpapi.New(ledger, runtime, nil))
			reconciler.Start()
			body := `{"input":{"garment_name":"shirt","bbox":[0,0,1,1],"images":[{"mime_type":"image/png","data":"eA=="}]}}`
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/gateway/requests/"+protocol.WardrobeModel, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+apiToken)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", id)
			result := make(chan error, 1)
			go func() {
				response, err := http.DefaultClient.Do(req)
				if err == nil {
					defer response.Body.Close()
					var output protocol.Response
					err = json.NewDecoder(response.Body).Decode(&output)
					if response.StatusCode != 200 || output.State != "succeeded" || output.RequestID != id {
						t.Errorf("retry failed to deliver final success: HTTP %d %+v error=%+v", response.StatusCode, output, output.Error)
					}
				}
				result <- err
			}()
			var deadline time.Time
			waitForNativeTest(t, "failed call back in MQ", func() bool {
				row, err := db.Request.Get(t.Context(), id)
				if err != nil || calls.Load() != 1 || row.State != request.StateQueued {
					return false
				}
				deadline = row.DeadlineAt
				return !row.SlotReserved && row.QueuePublishAt != nil && row.StartedAt == nil && row.NextPollAt == nil
			})
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			row, err := db.Request.Get(t.Context(), id)
			if err != nil || row.State != request.StateSucceeded || row.SlotReserved || calls.Load() != 2 || !row.DeadlineAt.Equal(deadline) {
				t.Fatalf("retry leaked capacity, duplicated dispatch or renewed deadline: %v %v", row, err)
			}
		})
	}
}

// 回队后同一 ID 会有新的领取，旧一轮成功、失败及回队操作都不能覆盖它。
func TestRequeuedDispatchRejectsLateAttemptWrites(t *testing.T) {
	a, b, db := stores(t)
	spec := testInstance(protocol.WardrobeModel, "http://127.0.0.1:8001", 1)
	registerReady(t, a, spec)
	id := uuid.NewString()
	admission := testAdmission(spec, id, 1)
	admission.BackendTimeout = 2 * time.Second
	admitted, err := a.Admit(t.Context(), admission)
	if err != nil {
		t.Fatal(err)
	}
	// Both sides must use database precision when checking deadline stability.
	admitted, err = db.Request.Get(t.Context(), admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.Claim(t.Context(), testConfig(), id)
	if err != nil || first == nil {
		t.Fatalf("first claim: %v %v", first, err)
	}
	if err := a.RequeueDispatch(t.Context(), id, *first.StartedAt, first.PollClaim); err != nil {
		t.Fatal(err)
	}
	if err := db.Request.UpdateOneID(id).SetQueuePublishAt(time.Now().Add(-time.Second)).Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := b.Claim(t.Context(), testConfig(), id)
	if err != nil || second == nil || second.PollClaim == first.PollClaim {
		t.Fatalf("second claim: %v %v", second, err)
	}
	if err := a.RecordResponse(t.Context(), id, first.PollClaim, succeeded(id), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := a.DispatchFailure(t.Context(), id, first.PollClaim, 502, "late_failure", "old attempt", false); err != nil {
		t.Fatal(err)
	}
	if err := a.RequeueDispatch(t.Context(), id, *first.StartedAt, first.PollClaim); err != nil {
		t.Fatal(err)
	}
	row, err := db.Request.Get(t.Context(), id)
	if err != nil || row.State != request.StateProcessing || !row.SlotReserved || row.PollClaim != second.PollClaim {
		t.Fatalf("late attempt overwrote retry: %v %v", row, err)
	}
	// 重试沿用总截止时间。期限已过后回队会进入既有超时处理，不能无限续命。
	if !row.DeadlineAt.Equal(admitted.DeadlineAt) {
		t.Fatal("retry extended the original deadline")
	}
	<-time.After(time.Until(admitted.DeadlineAt) + time.Millisecond)
	if err := b.RequeueDispatch(t.Context(), id, *second.StartedAt, second.PollClaim); err != nil {
		t.Fatal(err)
	}
	if err := b.Expire(context.Background()); err != nil {
		t.Fatal(err)
	}
	row, err = db.Request.Get(t.Context(), id)
	if err != nil || row.State != request.StateFailed || row.SlotReserved || row.ErrorCode != "queue_timeout" {
		t.Fatalf("expired retry kept its slot: %v %v", row, err)
	}
}
