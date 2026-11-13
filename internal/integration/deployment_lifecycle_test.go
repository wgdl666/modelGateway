package integration

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
	"testing"
	"time"
)

func TestDeploymentFencesAcrossReplicasAndPersistsIdentity(t *testing.T) {
	a, b, db := stores(t)
	ctx := t.Context()
	spec := testInstance(protocol.BodyModel, "http://127.0.0.1:8001", 1)
	spec.DeploymentID = "body-release-2"
	spec.InitiallyDraining = true
	ensureTestGroup(t, a, spec)
	_, err := db.DeploymentLease.Create().SetID(spec.ResourceGroup).SetRegion("cn/dev").SetWorkerID("worker1").SetOperationID("op").SetOwner("owner").SetEpoch(1).SetLeaseUntil(time.Now().Add(time.Minute)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DeploymentLease.DeleteOneID(spec.ResourceGroup).Exec(context.Background()) })
	if err = a.Register(ctx, spec, ""); !errors.Is(err, store.ErrDeploymentFenced) {
		t.Fatalf("unfenced registration: %v", err)
	}
	fenced := store.WithDeploymentFence(ctx, store.DeploymentFence{Operation: "op", Owner: "owner", Epoch: 1})
	if err = a.Register(fenced, spec, ""); err != nil {
		t.Fatal(err)
	}
	if err = b.Register(fenced, spec, ""); err != nil {
		t.Fatal("idempotent register", err)
	}
	rows, err := a.ListInstances(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Draining {
		t.Fatalf("must start drained %+v %v", rows, err)
	}
	if err = b.Drain(ctx, spec.ID, false); !errors.Is(err, store.ErrDeploymentFenced) {
		t.Fatal("unfenced resume", err)
	}
	db.DeploymentLease.UpdateOneID(spec.ResourceGroup).SetOwner("new").SetEpoch(2).Exec(ctx)
	if err = b.Drain(fenced, spec.ID, false); !errors.Is(err, store.ErrDeploymentFenced) {
		t.Fatal("stale owner resumed", err)
	}
	fenced = store.WithDeploymentFence(ctx, store.DeploymentFence{Operation: "op", Owner: "new", Epoch: 2})
	if err = b.Drain(fenced, spec.ID, false); err != nil {
		t.Fatal(err)
	}
	if err = b.RecordProbe(ctx, spec.ID, time.Now().UTC(), true); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err = a.Admit(ctx, testAdmission(spec, id, 1)); err != nil {
		t.Fatal(err)
	}
	task, err := a.Claim(ctx, testConfig(), id)
	if err != nil || task == nil || task.DeploymentID != spec.DeploymentID {
		t.Fatalf("lost execution identity %+v %v", task, err)
	}
}
