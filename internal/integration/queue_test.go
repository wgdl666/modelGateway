package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ariga.io/atlas/sql/postgres"
	atlasschema "ariga.io/atlas/sql/schema"
	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

const (
	apiToken   = "test-client-token-at-least-24-chars"
	adminToken = "test-admin-token-at-least-24-chars"
)

func stores(t *testing.T) (*store.Store, *store.Store, *ent.Client) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required for PostgreSQL integration tests")
	}
	pgConfig, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasSuffix(pgConfig.Database, "_test") {
		t.Fatal("TEST_DATABASE_URL must point to a _test database")
	}
	// 预建 schema 的测试账号只需在该 schema 内建表，无需数据库级 CREATE 权限。
	namespace := pgConfig.RuntimeParams["search_path"]
	if namespace == "" {
		namespace = "model_gateway"
	}
	admin := stdlib.OpenDB(*pgConfig)
	defer admin.Close()
	driver, err := postgres.Open(admin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = driver.InspectSchema(t.Context(), namespace, &atlasschema.InspectOptions{Mode: atlasschema.InspectSchemas})
	if atlasschema.IsNotExistError(err) {
		err = driver.ApplyChanges(t.Context(), []atlasschema.Change{&atlasschema.AddSchema{S: atlasschema.New(namespace)}})
	}
	if err != nil {
		t.Fatal(err)
	}
	a, err := store.OpenDatabase(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.Schema.Create(t.Context(), entschema.WithSchemaName(namespace), entschema.WithForeignKeys(false)); err != nil {
		t.Fatal(err)
	}
	b, err := store.OpenDatabase(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if _, err := a.Request.Delete().Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ModelInstance.Delete().Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store.New(a), store.New(b), b
}

func testConfig() config.Config {
	return config.Config{
		APIToken: apiToken, AdminToken: adminToken,
		AllowedNetworks: []string{"127.0.0.0/8"}, AllowedPorts: []string{"8001", "8002"},
		BackendTimeout: 10 * time.Second, MaxWaiting: 64, MaxWaitingBytes: 128 << 20,
	}
}

func testInstance(model, capability, endpoint string, capacity int) protocol.Instance {
	return protocol.Instance{
		ID: uuid.NewString(), ModelKey: model, Capability: capability, Endpoint: endpoint,
		HealthURL: endpoint + "/health", Artifact: "sha256-" + strings.Repeat("a", 64),
		ResourceGroup: "host/gpu0", Capacity: capacity, GroupCapacity: capacity,
	}
}

func registerReady(t *testing.T, replica *store.Store, spec protocol.Instance) {
	t.Helper()
	if err := replica.Register(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := replica.RecordProbe(t.Context(), spec.ID, time.Now().UTC(), true); err != nil {
		t.Fatal(err)
	}
}

func testAdmission(i protocol.Instance, id string, units int) store.Admission {
	input := json.RawMessage(`{"prompt":"same input","images":[{"mime_type":"image/png","data":"eA=="}]}`)
	raw, _ := json.Marshal(protocol.Submission{Input: input})
	cfg := testConfig()
	return store.Admission{
		ID: id, ModelKey: i.ModelKey, Capability: i.Capability, Payload: raw, Input: input, Units: units,
		MaxWaiting: cfg.MaxWaiting, MaxWaitingBytes: cfg.MaxWaitingBytes, BackendTimeout: cfg.BackendTimeout,
	}
}

func succeeded(id string) protocol.ModelResponse {
	return protocol.ModelResponse{Response: protocol.Response{RequestID: id, State: "succeeded", Result: &protocol.Result{
		Files: []protocol.Object{{Provider: "s3", Region: "us-east-2", Bucket: "vmind-results", Key: id + "/result"}},
	}}}
}

func TestTwoReplicasQueueBudgetAndIdempotency(t *testing.T) {
	a, b, db := stores(t)
	i := testInstance(protocol.VideoModel, "video", "http://127.0.0.1:8001", 1)
	registerReady(t, a, i)
	same := testAdmission(i, uuid.NewString(), 1)
	same.MaxWaiting = 1
	var wg sync.WaitGroup
	for n := range 20 {
		wg.Go(func() {
			replica := []*store.Store{a, b}[n%2]
			task, err := replica.Admit(t.Context(), same)
			if err != nil || task.ID != same.ID {
				t.Errorf("concurrent duplicate: %v", err)
			}
		})
	}
	wg.Wait()
	count, err := db.Request.Query().Count(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("idempotency ledger count=%d err=%v", count, err)
	}
	different := same
	different.Input = json.RawMessage(`{"prompt":"changed"}`)
	if _, err := b.Admit(t.Context(), different); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
	newTask := same
	newTask.ID = uuid.NewString()
	if _, err := b.Admit(t.Context(), newTask); !errors.Is(err, store.ErrFull) {
		t.Fatalf("expected count limit, got %v", err)
	}
	newTask.MaxWaiting, newTask.MaxWaitingBytes = 64, int64(len(same.Payload)+len(newTask.Payload)-1)
	if _, err := b.Admit(t.Context(), newTask); !errors.Is(err, store.ErrFull) {
		t.Fatalf("expected byte limit, got %v", err)
	}
}

func TestTwoReplicasReserveSharedGPUAndReleaseExactlyOnce(t *testing.T) {
	a, b, db := stores(t)
	image := testInstance(protocol.OutfitModel, "vton", "http://127.0.0.1:8001", 2)
	video := testInstance(protocol.VideoModel, "video", "http://127.0.0.1:8002", 2)
	registerReady(t, a, image)
	registerReady(t, b, video)
	for _, instance := range []protocol.Instance{image, video} {
		if _, err := a.Admit(t.Context(), testAdmission(instance, uuid.NewString(), 2)); err != nil {
			t.Fatal(err)
		}
	}
	var won atomic.Int32
	claims := make(chan string, 20)
	var wg sync.WaitGroup
	for n := range 20 {
		wg.Go(func() {
			task, err := []*store.Store{a, b}[n%2].Claim(t.Context(), testConfig())
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			if task != nil {
				won.Add(1)
				claims <- task.ID
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("shared GPU admitted %d tasks", won.Load())
	}
	first := <-claims
	if err := a.DispatchFailure(t.Context(), first, 504, "backend_timeout", "unknown", true); err != nil {
		t.Fatal(err)
	}
	if next, err := b.Claim(t.Context(), testConfig()); next != nil || err != nil {
		t.Fatalf("unknown task released capacity: %v %v", next, err)
	}
	poll, err := b.ClaimPoll(t.Context())
	if err != nil || poll == nil || poll.ID != first {
		t.Fatalf("other replica did not take over query: %v %v", poll, err)
	}
	if err := b.RecordResponse(t.Context(), first, poll.PollClaim, succeeded(first), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordResponse(t.Context(), first, "", succeeded(first), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	task, err := db.Request.Get(t.Context(), first)
	if err != nil || task.State != request.StateFailed || task.SlotReserved {
		t.Fatalf("late completion rewrote failed state or leaked capacity: %v %v", task, err)
	}
	next, err := b.Claim(t.Context(), testConfig())
	if err != nil || next == nil || next.ID == first || next.Units != 2 {
		t.Fatalf("queued task was not dispatched after slot release: %v %v", next, err)
	}
}

func TestQueuedUntilMatchingInstanceHasFreshProbe(t *testing.T) {
	a, b, _ := stores(t)
	matching := testInstance("Vmind_wardrobe_v1.0", "garment_extraction", "http://127.0.0.1:8001", 1)
	in := testAdmission(matching, uuid.NewString(), 1)
	if task, err := a.Admit(t.Context(), in); err != nil || task.State != request.StateQueued {
		t.Fatalf("admit with no instances: task=%v err=%v", task, err)
	}
	// 模拟拆分模型键前留下的注册记录，确认它不能接走新拆衣任务。
	legacy := testInstance(matching.ModelKey, "cloth_gen", "http://127.0.0.1:8002", 1)
	registerReady(t, a, legacy)
	if err := a.Register(t.Context(), matching); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordProbe(t.Context(), matching.ID, time.Now().Add(-time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if task, err := b.Claim(t.Context(), testConfig()); err != nil || task != nil {
		t.Fatalf("claimed with wrong capability or stale probe: task=%v err=%v", task, err)
	}
	if task, err := b.Get(t.Context(), in.ID); err != nil || task.State != request.StateQueued {
		t.Fatalf("task no longer queued: task=%v err=%v", task, err)
	}
	if err := a.RecordProbe(t.Context(), matching.ID, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if task, err := b.Claim(t.Context(), testConfig()); err != nil || task == nil || task.ID != in.ID || task.InstanceID != matching.ID || task.State != request.StateProcessing {
		t.Fatalf("claim after fresh probe: task=%v err=%v", task, err)
	}
}

func TestGroupRegistrationAndQueueExpiry(t *testing.T) {
	a, b, _ := stores(t)
	i := testInstance(protocol.VideoModel, "video", "http://127.0.0.1:8001", 1)
	registerReady(t, a, i)
	inconsistent := testInstance(protocol.OutfitModel, "vton", "http://127.0.0.1:8002", 2)
	if err := b.Register(t.Context(), inconsistent); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("inconsistent group capacity accepted: %v", err)
	}
	in := testAdmission(inconsistent, uuid.NewString(), 1)
	in.BackendTimeout = time.Millisecond
	if _, err := b.Admit(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := a.Expire(context.Background()); err != nil {
		t.Fatal(err)
	}
	task, err := b.Get(t.Context(), in.ID)
	if err != nil || task.State != request.StateFailed || task.ErrorCode != "queue_timeout" || task.ErrorStatus != 504 {
		t.Fatalf("expired task: %v %v", task, err)
	}
}
