package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

// 仍以真实私网配置校验地址，测试传输层仅把这些地址的连接送到本机假上游。
func synchronousServer(t *testing.T, tasks *store.Store, cfg config.Config, upstream *httptest.Server) string {
	t.Helper()
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	app := httpapi.New(tasks, runtime, nil)
	http.DefaultTransport = previous
	t.Cleanup(transport.CloseIdleConnections)
	return startHTTPServer(t, app)
}
func sendSynchronous(t *testing.T, base, path, kind, body, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", kind)
	req.Header.Set("Idempotency-Key", id)
	req.Header.Set("traceparent", "00-1234567890abcdef1234567890abcdef-1234567890abcdef-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func waitSynchronousState(t *testing.T, db *ent.Client, id string, state request.State, reserved bool) *ent.Request {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		row, err := db.Request.Get(t.Context(), id)
		if err == nil && row.State == state && row.SlotReserved == reserved {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}
	row, err := db.Request.Get(t.Context(), id)
	t.Fatalf("request state: %+v %v; want %s reserved=%v", row, err, state, reserved)
	return nil
}
func TestSynchronousRoutesUseRegisteredTargets(t *testing.T) {
	a, _, db := stores(t)
	cfg := testConfig()
	cfg.VisionAddr = "172.31.43.7:18280"
	cfg.DecisionAddr = "172.31.3.219:8874"
	cfg.VLMAddr = "172.31.3.219:30332"
	const multipart = "multipart/form-data; boundary=fixture"
	const form = "--fixture\r\nContent-Disposition: form-data; name=\"image\"\r\n\r\naW1hZ2U=\r\n--fixture--\r\n"
	cases := []struct{ model, port, path, target, kind, body string }{
		{protocol.HumanParseModel, "6105", "/v1/vision/human-parser/human_parser/predict", "/_dispatch/6105/human-parser/human_parser/predict", "application/json", `{"image":"aW1hZ2U="}`},
		{protocol.HumanYOLOModel, "6102", "/v1/vision/human-yolo/predict", "/_dispatch/6102/human-yolo/predict", multipart, form},
		{protocol.SegmentPersonModel, "6106", "/v1/vision/segment-person/api/segment", "/_dispatch/6106/segment-person/api/segment", multipart, form},
		{protocol.SegmentSubjectModel, "6106", "/v1/vision/segment-person/api/segment_subject", "/_dispatch/6106/segment-person/api/segment_subject", multipart, form},
		{protocol.LensCorrectionModel, "6117", "/v1/vision/lens_distortion_correction/correct", "/_dispatch/6117/lens_distortion_correction/correct", "application/json", `{"base64":"aW1hZ2U=","mirror_lens":false}`},
		{protocol.FaceSimModel, "6103", "/v1/vision/facesim/compare", "/_dispatch/6103/facesim/compare", multipart, strings.Replace(form, `name="image"`, `name="image_a"`, 1)},
		{protocol.DecisionModel, "19101", "/v1/decision/predict", "/v1/instances/19101/predict", multipart, form},
		{protocol.DreamModel, "", "/v1/chat/completions", "/v1/chat/completions", "application/json", `{"model":"Vmind_dream_v1","messages":[{"role":"user","content":"hello"}]}`},
	}
	var expected string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != expected || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Trace-ID") != "1234567890abcdef1234567890abcdef" {
			t.Errorf("wrong execution request: %s", r.URL)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			t.Errorf("lost request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"answer":"unchanged"}`)
	}))
	defer upstream.Close()
	base := synchronousServer(t, a, cfg, upstream)
	// 已配置地址而未登记时必须拒绝，不能偷偷继续直连。
	missing := sendSynchronous(t, base, cases[0].path, cases[0].kind, cases[0].body, uuid.NewString())
	missing.Body.Close()
	if missing.StatusCode != 503 {
		t.Fatalf("unregistered returned %d", missing.StatusCode)
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			spec := testInstance(tc.model, "http://127.0.0.1:"+tc.port, 1)
			if tc.model == protocol.DreamModel {
				spec.Endpoint = "http://" + cfg.VLMAddr
			}
			spec.HealthURL = cfg.SynchronousHealthURL(tc.model)
			if tc.model == protocol.FaceSimModel {
				spec.ResourceGroup = "host/cpu/facesim"
				spec.GPUDevices = nil
				if err := db.ResourceGroup.Create().SetID(spec.ResourceGroup).SetCapacityUnits(1).Exec(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := a.Register(t.Context(), spec, ""); err != nil {
					t.Fatal(err)
				}
				if err := a.RecordProbe(t.Context(), spec.ID, time.Now().UTC(), true); err != nil {
					t.Fatal(err)
				}
			} else {
				registerReady(t, a, spec)
			}
			expected = tc.target
			id := uuid.NewString()
			resp := sendSynchronous(t, base, tc.path+"?target=ignored", tc.kind, tc.body, id)
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || string(raw) != `{"answer":"unchanged"}` || resp.Header.Get("X-Model-Instance") != spec.ID {
				t.Fatalf("response: %d %s %v", resp.StatusCode, raw, err)
			}
			row := waitSynchronousState(t, db, id, request.StateSucceeded, false)
			if row.InstanceID != spec.ID || row.Endpoint != spec.Endpoint || row.TraceID != "1234567890abcdef1234567890abcdef" || row.QueuePublishAt != nil || strings.Contains(string(row.RequestPayload), "aW1hZ2U=") {
				t.Fatalf("wrong dispatch evidence: %+v", row)
			}
			replay := sendSynchronous(t, base, tc.path, tc.kind, tc.body, id)
			replay.Body.Close()
			if replay.StatusCode != 409 {
				t.Fatalf("replay must not execute again: %d", replay.StatusCode)
			}
		})
	}
}

func TestSynchronousDisconnectAndUnknownOutcome(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			a, _, db := stores(t)
			cfg := testConfig()
			cfg.VLMAddr = "172.31.3.219:30332"
			finish := make(chan struct{})
			defer close(finish)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if broken {
					w.Header().Set("Content-Length", "200")
					fmt.Fprint(w, "short")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				<-finish
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer upstream.Close()
			spec := testInstance(protocol.DreamModel, "http://"+cfg.VLMAddr, 1)
			registerReady(t, a, spec)
			base := synchronousServer(t, a, cfg, upstream)
			id := uuid.NewString()
			resp := sendSynchronous(t, base, "/v1/chat/completions", "application/json", `{"model":"Vmind_dream_v1","stream":true}`, id)
			if broken {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				waitSynchronousState(t, db, id, request.StateFailed, true)
			} else {
				if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				waitSynchronousState(t, db, id, request.StateProcessing, true)
				// 客户端已断开，上游仍在执行；只有完整结束后才归还容量。
				finish <- struct{}{}
				waitSynchronousState(t, db, id, request.StateSucceeded, false)
			}
		})
	}
}

func TestSynchronousSharesCapacityWithQueueAndAliases(t *testing.T) {
	a, b, db := stores(t)
	ctx := t.Context()
	cfg := testConfig()
	cfg.VLMAddr = "172.31.3.219:30332"
	cfg.VisionAddr = "172.31.43.7:18280"
	cfg.DecisionAddr = "172.31.3.219:8874"
	for gpu := 0; gpu < 4; gpu++ {
		if _, err := a.CreateResourceGroup(ctx, fmt.Sprintf("host/gpu%d", gpu), "host", gpu, 1); err != nil {
			t.Fatal(err)
		}
	}
	dream := testInstance(protocol.DreamModel, "http://"+cfg.VLMAddr, 1)
	dream.GPUDevices = []int{0, 1, 2, 3}
	registerReady(t, a, dream)
	queued := testInstance(protocol.OutfitModel, "http://127.0.0.1:8001", 1)
	queued.ResourceGroup = "host/gpu3"
	queued.GPUDevices = []int{3}
	registerReady(t, a, queued)
	claim := func(s *store.Store, spec protocol.Instance) *ent.Request {
		t.Helper()
		id := uuid.NewString()
		if _, err := s.Admit(ctx, testAdmission(spec, id, 1)); err != nil {
			t.Fatal(err)
		}
		row, err := s.Claim(ctx, cfg, id)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	held := claim(a, dream)
	if held == nil || len(held.ResourceGroups) != 4 {
		t.Fatalf("multi GPU hold: %+v", held)
	}
	if other := claim(b, queued); other != nil {
		t.Fatal("async task exceeded Dream GPU reservation")
	}
	groups, err := b.ListResourceGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.ReservedUnits != 1 {
			t.Fatalf("missing GPU occupancy %+v", g)
		}
	}
	if err := b.Drain(ctx, dream.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := a.CompleteSynchronous(ctx, held.ID, held.PollClaim, protocol.HTTPResult{StatusCode: 200}, true); err != nil {
		t.Fatal(err)
	}
	if next := claim(b, dream); next != nil {
		t.Fatal("paused instance accepted a new request")
	}
	if poll, err := b.ClaimPoll(ctx); err != nil || poll != nil {
		t.Fatalf("synchronous request entered async poll: %+v %v", poll, err)
	}
	// 同一个执行副本以两个模型键登记，即使 GPU 组额度更大也只能执行一个请求。
	if _, err := db.Request.Delete().Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateResourceGroupCapacity(ctx, "host/gpu0", 2); err != nil {
		t.Fatal(err)
	}
	person := testInstance(protocol.SegmentPersonModel, "http://127.0.0.1:6106", 1)
	person.HealthURL = cfg.SynchronousHealthURL(person.ModelKey)
	registerReady(t, a, person)
	subject := person
	subject.ID = uuid.NewString()
	subject.ModelKey = protocol.SegmentSubjectModel
	registerReady(t, a, subject)
	ids := []string{uuid.NewString(), uuid.NewString()}
	for i, spec := range []protocol.Instance{person, subject} {
		if _, err := a.Admit(ctx, testAdmission(spec, ids[i], 1)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan *ent.Request, 2)
	errs := make(chan error, 2)
	for i, s := range []*store.Store{a, b} {
		wg.Go(func() { r, e := s.Claim(ctx, cfg, ids[i]); results <- r; errs <- e })
	}
	wg.Wait()
	close(results)
	close(errs)
	count := 0
	for r := range results {
		if r != nil {
			count++
		}
	}
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if count != 1 {
		t.Fatalf("shared execution side received %d concurrent claims", count)
	}
	publications, err := a.PendingPublications(ctx)
	if err != nil || len(publications) != 0 {
		t.Fatalf("sync tasks published to MQ: %+v %v", publications, err)
	}

}

func TestSynchronousKnownFailureReleasesAndUnknownHolds(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			a, _, db := stores(t)
			cfg := testConfig()
			cfg.VLMAddr = "172.31.3.219:30332"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if unknown {
					w.Header().Set("X-Model-Outcome", "unknown")
				}
				w.WriteHeader(502)
				fmt.Fprint(w, `{"error":"failed"}`)
			}))
			defer upstream.Close()
			spec := testInstance(protocol.DreamModel, "http://"+cfg.VLMAddr, 1)
			registerReady(t, a, spec)
			base := synchronousServer(t, a, cfg, upstream)
			id := uuid.NewString()
			resp := sendSynchronous(t, base, "/v1/chat/completions", "application/json", `{"model":"Vmind_dream_v1"}`, id)
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			row := waitSynchronousState(t, db, id, request.StateFailed, unknown)
			if resp.StatusCode != 502 || unknown && row.ErrorCode != "backend_outcome_unknown" {
				t.Fatalf("wrong terminal response: %d %+v", resp.StatusCode, row)
			}
		})
	}
}
