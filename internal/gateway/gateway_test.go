package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
)

const apiToken = "test-client-token-at-least-24-chars"
const workerToken = "test-worker-token-at-least-24-chars"
const adminToken = "test-admin-token-at-least-24-chars"

var files = json.RawMessage(`{"files":[{"provider":"s3","region":"us-east-2","bucket":"test-results","key":"image.png"}]}`)

func stores(t *testing.T) (*Store, *Store) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for PostgreSQL integration tests")
	}
	config, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasSuffix(config.Database, "_test") {
		t.Fatal("test database name must end in _test")
	}
	a, e := OpenDatabase(context.Background(), dsn, true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, e := OpenDatabase(context.Background(), dsn, false)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = b.Close() })
	// 仅清理显式测试库中的业务记录，表结构由 Ent 创建。
	if _, e := a.Request.Delete().Exec(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := a.ModelInstance.Delete().Exec(context.Background()); e != nil {
		t.Fatal(e)
	}
	return &Store{a, 30 * time.Second}, &Store{b, 30 * time.Second}
}
func instance(endpoint string, capacity int) Instance {
	return Instance{ID: uuid.NewString(), ModelKey: "vton_v3", Endpoint: endpoint, MachineID: "test-machine", GPUDevices: []int{0, 1}, Capacity: capacity, Ready: true}
}
func submission(units int) Submission {
	return Submission{ID: uuid.NewString(), ModelKey: "vton_v3", Units: units, Input: json.RawMessage(`{"prompt":"原始请求","seed":42}`)}
}
func payload(s Submission) []byte { b, _ := json.Marshal(s); return b }
func used(t *testing.T, s *Store, id string) int {
	t.Helper()
	i, e := s.DB.ModelInstance.Get(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	return i.UsedUnits
}
func register(t *testing.T, s *Store, i Instance) {
	t.Helper()
	if e := s.Register(context.Background(), i); e != nil {
		t.Fatal(e)
	}
}

func TestTwoReplicasAtomicUnits(t *testing.T) {
	a, b := stores(t)
	i := instance("http://127.0.0.1:8001", 6)
	register(t, a, i)
	var won atomic.Int32
	var idsMu sync.Mutex
	var ids []string
	var wg sync.WaitGroup
	for n := 0; n < 30; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s := a
			if n%2 == 1 {
				s = b
			}
			in := submission(2)
			r, _, created, e := s.Reserve(context.Background(), in, payload(in))
			if errors.Is(e, ErrBusy) {
				return
			}
			if e != nil {
				t.Error(e)
				return
			}
			if created {
				won.Add(1)
				idsMu.Lock()
				ids = append(ids, r.ID)
				idsMu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	if won.Load() < 1 || won.Load() > 3 || used(t, a, i.ID) != int(won.Load())*2 {
		t.Fatalf("incorrect capacity won=%d used=%d", won.Load(), used(t, a, i.ID))
	}
	for _, id := range ids {
		if _, e := a.Start(context.Background(), id); e != nil {
			t.Fatal(e)
		}
		for n := 0; n < 8; n++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if e := b.Apply(context.Background(), id, BackendResponse{Status: "succeeded", Result: files}); e != nil {
					t.Error(e)
				}
			}()
		}
	}
	wg.Wait()
	if used(t, a, i.ID) != 0 {
		t.Fatal("duplicate completion decremented incorrectly")
	}
}
func TestDuplicateIDPreservesOriginal(t *testing.T) {
	a, b := stores(t)
	i := instance("http://127.0.0.1:8001", 8)
	register(t, a, i)
	in := submission(3)
	raw := payload(in)
	var created atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s := a
			if n%2 == 1 {
				s = b
			}
			_, _, fresh, e := s.Reserve(context.Background(), in, raw)
			if e != nil {
				t.Error(e)
			}
			if fresh {
				created.Add(1)
			}
		}(n)
	}
	wg.Wait()
	if created.Load() != 1 || used(t, a, i.ID) != 3 {
		t.Fatal("duplicate reserved more than once")
	}
	in.Input = json.RawMessage(`{"prompt":"changed"}`)
	r, _, fresh, e := b.Reserve(context.Background(), in, payload(in))
	if e != nil || fresh || !bytes.Contains(r.Payload, []byte("原始请求")) {
		t.Fatal("original payload replaced")
	}
}
func TestRegistrationDoesNotResetCapacityOrDrain(t *testing.T) {
	a, b := stores(t)
	i := instance("http://127.0.0.1:8001", 4)
	register(t, a, i)
	in := submission(2)
	if _, _, _, e := a.Reserve(context.Background(), in, payload(in)); e != nil {
		t.Fatal(e)
	}
	if e := a.Drain(context.Background(), i.ID, true); e != nil {
		t.Fatal(e)
	}
	if e := b.Heartbeat(context.Background(), i.ID, true); e != nil {
		t.Fatal(e)
	}
	register(t, b, i)
	if used(t, a, i.ID) != 2 {
		t.Fatal("registration reset count")
	}
	in = submission(1)
	if _, _, _, e := b.Reserve(context.Background(), in, payload(in)); !errors.Is(e, ErrBusy) {
		t.Fatal("drained instance accepted work")
	}
	i.Capacity = 1
	if e := b.Register(context.Background(), i); !errors.Is(e, ErrConflict) {
		t.Fatal("capacity reduced below use")
	}
	i.Capacity = 4
	i.ID = uuid.NewString()
	if e := b.Register(context.Background(), i); !errors.Is(e, ErrConflict) {
		t.Fatal("duplicate endpoint reset capacity")
	}
}

func TestConcurrentRegistrationKeepsEndpointOwnership(t *testing.T) {
	a, b := stores(t)
	first := instance("http://127.0.0.1:8001", 4)
	second := first
	second.ID = uuid.NewString()
	start := make(chan struct{})
	results := make(chan error, 2)
	for n, i := range []Instance{first, second} {
		store := []*Store{a, b}[n]
		go func() { <-start; results <- store.Register(context.Background(), i) }()
	}
	close(start)
	winners := 0
	for range 2 {
		if err := <-results; err == nil {
			winners++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected one endpoint owner, got %d", winners)
	}
	active, err := a.DB.ModelInstance.Query().Where(modelinstance.ReadyEQ(true)).Count(context.Background())
	if err != nil || active != 1 {
		t.Fatal("duplicate endpoint registered", active, err)
	}
}

func TestConcurrentRegistrationOfSameInstance(t *testing.T) {
	a, b := stores(t)
	i := instance("http://127.0.0.1:8001", 4)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, store := range []*Store{a, b} {
		go func() { <-start; results <- store.Register(context.Background(), i) }()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("same-instance registration should be idempotent", err)
		}
	}
}

func TestReplicasClaimDistinctReconciliationBatches(t *testing.T) {
	a, b := stores(t)
	i := instance("http://127.0.0.1:8001", 4)
	register(t, a, i)
	for range 4 {
		in := submission(1)
		if _, _, _, err := a.Reserve(context.Background(), in, payload(in)); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Start(context.Background(), in.ID); err != nil {
			t.Fatal(err)
		}
		if err := a.Apply(context.Background(), in.ID, BackendResponse{Status: "unknown"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DB.Request.Update().SetUpdatedAt(time.Now().Add(-10 * time.Second)).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan []Pending, 2)
	for _, store := range []*Store{a, b} {
		go func() {
			<-start
			batch, err := store.Reconcile(context.Background(), 5*time.Second, time.Second)
			if err != nil {
				t.Error(err)
			}
			results <- batch
		}()
	}
	close(start)
	seen := map[string]bool{}
	for range 2 {
		for _, p := range <-results {
			if seen[p.ID] || p.Endpoint != i.Endpoint {
				t.Fatal("request claimed twice or routed to wrong endpoint", p)
			}
			seen[p.ID] = true
		}
	}
	if len(seen) != 4 || used(t, a, i.ID) != 4 {
		t.Fatal("claim lost requests or released unknown capacity", len(seen))
	}
}
func TestUnknownRetainsCapacityAndOtherReplicaRecovers(t *testing.T) {
	a, b := stores(t)
	i := instance("http://127.0.0.1:8001", 4)
	register(t, a, i)
	in := submission(2)
	r, _, _, e := a.Reserve(context.Background(), in, payload(in))
	if e != nil {
		t.Fatal(e)
	}
	a.Start(context.Background(), r.ID)
	if e = a.Apply(context.Background(), r.ID, BackendResponse{Status: "unknown"}); e != nil {
		t.Fatal(e)
	}
	if used(t, b, i.ID) != 2 {
		t.Fatal("unknown released capacity")
	}
	if e = b.Apply(context.Background(), r.ID, BackendResponse{Status: "succeeded", Result: files}); e != nil {
		t.Fatal(e)
	}
	if used(t, a, i.ID) != 0 {
		t.Fatal("recovery did not release")
	}
}
func TestReservedRecoveryAndExpiredInstance(t *testing.T) {
	a, b := stores(t)
	i := instance("http://127.0.0.1:8001", 4)
	register(t, a, i)
	in := submission(2)
	a.Reserve(context.Background(), in, payload(in))
	if e := a.DB.Request.Update().SetUpdatedAt(time.Now().Add(-31 * time.Second)).Exec(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := b.Reconcile(context.Background(), time.Second, time.Second); e != nil {
		t.Fatal(e)
	}
	r, e := a.Get(context.Background(), in.ID)
	if e != nil || r.Status != "failed" || used(t, a, i.ID) != 0 {
		t.Fatal("reserved recovery failed")
	}
	if started, _ := a.Start(context.Background(), in.ID); started {
		t.Fatal("expired reservation dispatched")
	}
	if e := a.DB.ModelInstance.Update().SetLeaseExpiresAt(time.Now().Add(-time.Second)).Exec(context.Background()); e != nil {
		t.Fatal(e)
	}
	in = submission(1)
	if _, _, _, e = a.Reserve(context.Background(), in, payload(in)); !errors.Is(e, ErrBusy) {
		t.Fatal("expired instance accepted")
	}
}
func TestReservationRollback(t *testing.T) {
	a, _ := stores(t)
	i := instance("http://127.0.0.1:8001", 4)
	register(t, a, i)
	in := submission(2)
	if _, _, _, e := a.Reserve(context.Background(), in, json.RawMessage(`invalid-json`)); e == nil {
		t.Fatal("expected insert failure")
	}
	if used(t, a, i.ID) != 0 {
		t.Fatal("failed insert leaked capacity")
	}
}

func httpApp(t *testing.T, s *Store, backend string, timeout time.Duration) (*Server, string) {
	t.Helper()
	u, _ := url.Parse(backend)
	app, e := NewServer(s, Config{APIToken: apiToken, WorkerToken: workerToken, AdminToken: adminToken, AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{u.Port()}, BackendTimeout: timeout})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(app.Close)
	server := startHTTPServer(t, app)
	return app, server
}
func post(t *testing.T, endpoint, token string, body []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	r, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	data, _ := io.ReadAll(r.Body)
	return r.StatusCode, data
}
func waitState(t *testing.T, s *Store, id, status string) Request {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, e := s.Get(context.Background(), id)
		if e == nil && r.Status == status {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := s.Get(context.Background(), id)
	t.Fatalf("expected %s got %s", status, r.Status)
	return r
}
func TestHTTPAsyncObjectResultAcrossReplicas(t *testing.T) {
	a, b := stores(t)
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			calls.Add(1)
			writeBackendJSON(w, 202, map[string]string{"status": "running", "backend_task_id": "backend-001"})
		} else {
			writeBackendJSON(w, 200, BackendResponse{Status: "succeeded", Result: files})
		}
	}))
	defer backend.Close()
	_, one := httpApp(t, a, backend.URL, time.Second)
	twoApp, two := httpApp(t, b, backend.URL, time.Second)
	i := instance(backend.URL, 4)
	body, _ := json.Marshal(i)
	if status, _ := post(t, one+"/v1/instances/register", workerToken, body); status != 200 {
		t.Fatal(status)
	}
	in := submission(2)
	if status, _ := post(t, one+"/v1/requests", apiToken, payload(in)); status != 202 {
		t.Fatal(status)
	}
	waitState(t, a, in.ID, "running")
	if used(t, a, i.ID) != 2 {
		t.Fatal("async accepted released early")
	}
	if status, _ := post(t, two+"/v1/requests", apiToken, payload(in)); status != 202 {
		t.Fatal(status)
	}
	if e := a.DB.Request.UpdateOneID(in.ID).SetUpdatedAt(time.Now().Add(-10 * time.Second)).Exec(context.Background()); e != nil {
		t.Fatal(e)
	}
	twoApp.ReconcileOnce(context.Background())
	r := waitState(t, a, in.ID, "succeeded")
	if calls.Load() != 1 || used(t, a, i.ID) != 0 || !bytes.Contains(r.Result, []byte("bucket")) {
		t.Fatal("incorrect completion")
	}
}
func TestHTTPTimeoutUnknownAndAuthentication(t *testing.T) {
	a, _ := stores(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writeBackendJSON(w, 200, BackendResponse{Status: "succeeded", Result: files})
	}))
	defer backend.Close()
	_, server := httpApp(t, a, backend.URL, 20*time.Millisecond)
	i := instance(backend.URL, 2)
	register(t, a, i)
	in := submission(2)
	if status, _ := post(t, server+"/v1/requests", workerToken, payload(in)); status != 401 {
		t.Fatal("worker token accepted as caller")
	}
	if status, _ := post(t, server+"/v1/requests", apiToken, payload(in)); status != 202 {
		t.Fatal(status)
	}
	waitState(t, a, in.ID, "unknown")
	if used(t, a, i.ID) != 2 {
		t.Fatal("timeout released capacity")
	}
}
func TestObjectResultValidation(t *testing.T) {
	for _, result := range []string{`{"files":[{"url":"https://public/file"}]}`, `{"files":[{"provider":"s3","region":"x","bucket":"b","key":"k","url":"https://public"}]}`, `{"files":[]}`} {
		if (BackendResponse{Status: "succeeded", Result: json.RawMessage(result)}).validate() == nil {
			t.Fatalf("accepted %s", result)
		}
	}
	for _, provider := range []string{"s3", "oss"} {
		result := strings.ReplaceAll(string(files), "s3", provider)
		if e := (BackendResponse{Status: "succeeded", Result: json.RawMessage(result)}).validate(); e != nil {
			t.Fatal(e)
		}
	}
}

// 模拟后端仍使用独立标准库服务，确保客户端测试经过实际 Hertz HTTP 协议栈。
func writeBackendJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
