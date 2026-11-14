package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func workerFixture(t *testing.T) (*store.Store, *store.Store, *ent.Client, protocol.WorkerSession, []protocol.Instance) {
	t.Helper()
	a, b, db := stores(t)
	db.DeploymentDomain.Delete().Exec(t.Context())
	if err := db.DeploymentDomain.Create().SetID(1).SetRegion("cn/dev").Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DeploymentDomain.Delete().Exec(context.Background()) })
	session := protocol.WorkerSession{Region: "cn/dev", DeploymentID: "release-1", BootID: "boot-1"}
	specs := []protocol.Instance{testInstance(protocol.BodyModel, "http://127.0.0.1:8001", 1), testInstance(protocol.OutfitModel, "http://127.0.0.1:8001", 1)}
	for i := range specs {
		specs[i].DeploymentID = session.DeploymentID
		specs[i].ID = protocol.WorkerInstanceID("gpu1", session.DeploymentID, session.BootID, specs[i].ModelKey)
		session.Bindings = append(session.Bindings, protocol.WorkerBinding{ModelKey: specs[i].ModelKey, Artifact: specs[i].Artifact})
	}
	ensureTestGroup(t, a, specs[0])
	return a, b, db, session, specs
}

func workerBoot(session protocol.WorkerSession, specs []protocol.Instance, boot string) (protocol.WorkerSession, []protocol.Instance) {
	session.BootID = boot
	next := append([]protocol.Instance(nil), specs...)
	for i := range next {
		next[i].ID = protocol.WorkerInstanceID("gpu1", session.DeploymentID, boot, next[i].ModelKey)
	}
	return session, next
}

func TestWorkerLeaseExpiryRetainsUnknownExecutionAcrossReplicas(t *testing.T) {
	a, b, db, session, specs := workerFixture(t)
	ctx := t.Context()
	if _, err := a.RegisterWorker(ctx, "gpu1", session, specs); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RegisterWorker(ctx, "gpu1", session, specs); err != nil {
		t.Fatal("lost response retry", err)
	}
	if err := a.Drain(ctx, specs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WorkerSession(ctx, "gpu1", session, "heartbeat", specs); err != nil {
		t.Fatal(err)
	}
	row, _ := db.ModelInstance.Get(ctx, specs[0].ID)
	if !row.Draining {
		t.Fatal("heartbeat resumed operator pause")
	}
	id := uuid.NewString()
	if _, err := a.Admit(ctx, testAdmission(specs[1], id, 1)); err != nil {
		t.Fatal(err)
	}
	task, err := b.Claim(ctx, testConfig(), id)
	if err != nil || task == nil || task.BootID != session.BootID || task.DeploymentID != session.DeploymentID {
		t.Fatalf("claim lost boot identity: %+v %v", task, err)
	}
	if err := a.DispatchFailure(ctx, task.ID, task.PollClaim, 504, "deployment_execution_unknown", "unknown", true); err != nil {
		t.Fatal(err)
	}
	db.ModelInstance.Update().SetLeaseExpiresAt(time.Now().Add(-time.Minute)).Exec(ctx)
	// Fresh probes cannot revive expired leases, and expiry cannot release slots.
	for _, spec := range specs {
		b.RecordProbe(ctx, spec.ID, time.Now(), true)
	}
	if _, err := b.WorkerSession(ctx, "gpu1", session, "heartbeat", specs); !errors.Is(err, store.ErrWorkerSession) {
		t.Fatal("expired heartbeat accepted", err)
	}
	if _, err := a.RegisterWorker(ctx, "gpu1", session, specs); !errors.Is(err, store.ErrWorkerSession) {
		t.Fatal("expired boot revived by register", err)
	}
	next, nextSpecs := workerBoot(session, specs, "boot-2")
	if _, err := a.RegisterWorker(ctx, "gpu1", next, nextSpecs); !errors.Is(err, store.ErrConflict) {
		t.Fatal("replacement bypassed unknown execution", err)
	}
	held, _ := db.Request.Get(ctx, task.ID)
	if !held.SlotReserved {
		t.Fatal("expiry released reservation")
	}
	queued := uuid.NewString()
	a.Admit(ctx, testAdmission(specs[1], queued, 1))
	if got, err := b.Claim(ctx, testConfig(), queued); err != nil || got != nil {
		t.Fatalf("expired worker dispatched %+v %v", got, err)
	}
	// A known terminal outcome, unlike expiry, allows replacement.
	poll, err := b.ClaimPoll(ctx)
	if err != nil || poll == nil || poll.BootID != session.BootID || poll.DeploymentID != session.DeploymentID || poll.ErrorCode != "deployment_execution_unknown" {
		t.Fatalf("poll lost execution identity %+v %v", poll, err)
	}
	if err := a.DispatchFailure(ctx, task.ID, poll.PollClaim, 502, "confirmed_stopped", "confirmed", false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RegisterWorker(ctx, "gpu1", next, nextSpecs); err != nil {
		t.Fatal(err)
	}
	retired, _ := db.ModelInstance.Get(ctx, specs[1].ID)
	if retired.DeletedAt == nil {
		t.Fatal("old identity not retained as retired")
	}
	history, _ := db.Request.Get(ctx, task.ID)
	if history.InstanceID != specs[1].ID || history.BootID != "boot-1" {
		t.Fatal("history rewritten")
	}
	paused, _ := db.ModelInstance.Get(ctx, nextSpecs[0].ID)
	if !paused.Draining {
		t.Fatal("replacement lost operator pause")
	}
}

func TestWorkerConcurrentBootsDrainAndRetire(t *testing.T) {
	a, b, db, session, specs := workerFixture(t)
	second, secondSpecs := workerBoot(session, specs, "boot-2")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, replica := range []*store.Store{a, b} {
		wg.Add(1)
		go func(i int, r *store.Store) {
			defer wg.Done()
			s, rows := session, specs
			if i == 1 {
				s, rows = second, secondSpecs
			}
			_, err := r.RegisterWorker(t.Context(), "gpu1", s, rows)
			results <- err
		}(i, replica)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, store.ErrConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent owners: %d", successes)
	}
	active, _ := db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil()).All(t.Context())
	owner := session
	if active[0].BootID == second.BootID {
		owner = second
		specs = secondSpecs
	}
	a.Drain(t.Context(), specs[0].ID, true)
	if _, err := a.WorkerSession(t.Context(), "gpu1", owner, "unregister", specs); !errors.Is(err, store.ErrInstanceNotClearable) {
		t.Fatal("unregister without drain", err)
	}
	if result, err := a.WorkerSession(t.Context(), "gpu1", owner, "drain", specs); err != nil || !result.Draining || result.ReservedRequests != 0 {
		t.Fatal(result, err)
	}
	if result, err := b.WorkerSession(t.Context(), "gpu1", owner, "heartbeat", specs); err != nil || !result.Draining {
		t.Fatal("heartbeat cancelled process drain", result, err)
	}
	if _, err := b.RegisterWorker(t.Context(), "gpu1", owner, specs); !errors.Is(err, store.ErrWorkerSession) {
		t.Fatal("late register cancelled drain", err)
	}
	for i := 0; i < 2; i++ {
		if result, err := b.WorkerSession(t.Context(), "gpu1", owner, "unregister", specs); err != nil || !result.Retired {
			t.Fatal(result, err)
		}
	}
	if _, err := a.WorkerSession(t.Context(), "gpu1", owner, "heartbeat", specs); !errors.Is(err, store.ErrWorkerSession) {
		t.Fatal("retired heartbeat accepted", err)
	}
	fresh, freshSpecs := workerBoot(owner, specs, "boot-3")
	if _, err := a.RegisterWorker(t.Context(), "gpu1", fresh, freshSpecs); err != nil {
		t.Fatal(err)
	}
	row, _ := db.ModelInstance.Get(t.Context(), freshSpecs[0].ID)
	if !row.Draining {
		t.Fatal("clean restart lost admin pause")
	}
}

func TestWorkerRegistrationAPIIsScopedAndRegional(t *testing.T) {
	a, b, db, session, specs := workerFixture(t)
	grantSpecs := append([]protocol.Instance(nil), specs...)
	for i := range grantSpecs {
		grantSpecs[i].DeploymentID = ""
		grantSpecs[i].Artifact = ""
	}
	cfg := testConfig()
	credential := strings.Repeat("w", 40)
	cfg.WorkerRegistrations = map[string]config.WorkerRegistration{"gpu1": {Token: credential, Region: "cn/dev", Bindings: grantSpecs}}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	ready.Store(true)
	probe := func(_ context.Context, _ *config.Snapshot, target protocol.InstanceTarget) (protocol.InstanceHealth, error) {
		return protocol.InstanceHealth{Ready: ready.Load(), Region: "cn/dev", WorkerID: "gpu1", DeploymentID: target.DeploymentID, BootID: target.BootID, ArtifactDigest: target.ArtifactDigest}, nil
	}
	one := startHTTPServer(t, httpapi.New(a, runtime, probe))
	two := startHTTPServer(t, httpapi.New(b, runtime, probe))
	call := func(base, worker, action, token string, body protocol.WorkerSession, want int) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+"/v1/workers/"+worker+"/"+action, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			var detail any
			json.NewDecoder(response.Body).Decode(&detail)
			t.Fatalf("%s: %d want %d: %v", action, response.StatusCode, want, detail)
		}
	}
	call(one, "gpu1", "register", adminToken, session, 401)
	call(one, "other", "register", credential, session, 401)
	wrong := session
	wrong.Region = "us/east"
	call(one, "gpu1", "register", credential, wrong, 400)
	wrong = session
	wrong.Bindings = wrong.Bindings[:1]
	call(one, "gpu1", "register", credential, wrong, 400)
	ready.Store(false)
	call(one, "gpu1", "register", credential, session, 503)
	ready.Store(true)
	call(one, "gpu1", "register", credential, session, 200)
	call(two, "gpu1", "heartbeat", credential, session, 200)
	wrong = session
	wrong.BootID = "stale"
	call(two, "gpu1", "drain", credential, wrong, 409)
	call(two, "gpu1", "drain", credential, session, 200)
	call(one, "gpu1", "unregister", credential, session, 200)
	if count, _ := db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil()).Count(t.Context()); count != 0 {
		t.Fatal("retirement incomplete")
	}
}
