package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func TestModelKeyMigrationKeepsExecutionOwnership(t *testing.T) {
	a, _, db := stores(t)
	ctx := t.Context()
	dsn := os.Getenv("TEST_DATABASE_URL")
	pg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	namespace := pg.RuntimeParams["search_path"]
	if namespace == "" {
		namespace = "model_gateway"
	}
	pg.RuntimeParams["search_path"] = namespace
	conn, err := pgx.ConnectConfig(ctx, pg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanup, "ROLLBACK")
		_, err := conn.Exec(cleanup, `
			ALTER TABLE model_instances DROP COLUMN IF EXISTS capability, DROP COLUMN IF EXISTS legacy_capability;
			ALTER TABLE requests DROP COLUMN IF EXISTS capability, DROP COLUMN IF EXISTS legacy_capability;
		`)
		if err != nil {
			t.Error(err)
		}
		_ = conn.Close(cleanup)
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}

	// 旧商品图和拆衣曾共享模型键。让旧登记排序更靠前，漏掉归档就会误派。
	old := testInstance(protocol.WardrobeModel, "http://127.0.0.1:8001", 2)
	old.ID = "a-legacy-product"
	wardrobe := testInstance(protocol.WardrobeModel, "http://127.0.0.1:8002", 2)
	wardrobe.ID = "b-wardrobe"
	video := testInstance(protocol.VideoModel, "http://127.0.0.1:8002", 2)
	for _, spec := range []protocol.Instance{old, wardrobe, video} {
		registerReady(t, a, spec)
	}
	admit := func(spec protocol.Instance) store.Admission {
		t.Helper()
		in := testAdmission(spec, uuid.NewString(), 1)
		in.BackendTimeout = time.Minute
		if _, err := a.Admit(ctx, in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	legacyQueued, legacyHeld := admit(old), admit(old)
	queued, running := admit(wardrobe), admit(video)
	pin := func(id string, spec protocol.Instance, job string) {
		t.Helper()
		err := db.Request.UpdateOneID(id).SetState(request.StateProcessing).SetSlotReserved(true).ClearQueuePublishAt().
			SetInstanceID(spec.ID).SetResourceGroup(spec.ResourceGroup).SetEndpoint(spec.Endpoint).SetArtifactDigest(spec.Artifact).
			SetStartedAt(time.Now().Add(-time.Minute)).SetNextPollAt(time.Now().Add(-time.Second)).SetBackendTaskID(job).Exec(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	pin(legacyHeld.ID, old, "")
	// 对外已失败的历史请求仍可能占用 GPU，迁移同样必须拦住。
	if err := a.DispatchFailure(ctx, legacyHeld.ID, 504, "backend_timeout", "unknown", true); err != nil {
		t.Fatal(err)
	}
	pin(running.ID, video, "existing-native-job")

	exec(`ALTER TABLE model_instances ADD COLUMN capability text NOT NULL DEFAULT 'garment_extraction';
		  ALTER TABLE requests ADD COLUMN capability text NOT NULL DEFAULT 'garment_extraction';`)
	exec("UPDATE model_instances SET capability = 'cloth_gen' WHERE instance_id = $1", old.ID)
	exec("UPDATE model_instances SET capability = 'video' WHERE instance_id = $1", video.ID)
	exec("UPDATE requests SET capability = 'cloth_gen' WHERE request_id IN ($1, $2)", legacyQueued.ID, legacyHeld.ID)
	exec("UPDATE requests SET capability = 'video' WHERE request_id = $1", running.ID)

	if client, err := store.OpenDatabase(ctx, dsn); err == nil {
		_ = client.Close()
		t.Fatal("gateway accepted the schema before the old combinations were archived")
	} else if !strings.Contains(err.Error(), "model-key-only") {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../deploy/migrations/model-key-only.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := strings.ReplaceAll(string(raw), "model_gateway.", pgx.Identifier{namespace}.Sanitize()+".")
	if _, err := conn.Exec(ctx, migration); err == nil || !strings.Contains(err.Error(), "reserved legacy request") {
		t.Fatalf("migration should block while a legacy request holds capacity: %v", err)
	}
	exec("ROLLBACK")
	if row, err := db.ModelInstance.Get(ctx, old.ID); err != nil || row.DeletedAt != nil {
		t.Fatalf("blocked migration changed a registration: %v %v", row, err)
	}
	if err := a.RecordResponse(ctx, legacyHeld.ID, "", succeeded(legacyHeld.ID), time.Second); err != nil {
		t.Fatal(err)
	}
	exec(migration)
	exec(migration) // 重试迁移也不能改写历史和当前任务。

	migrated, err := store.OpenDatabase(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	b := store.New(migrated)
	if row, err := migrated.ModelInstance.Get(ctx, old.ID); err != nil || row.DeletedAt == nil || row.ModelKey != old.ModelKey {
		t.Fatalf("legacy registration was not archived in place: %v %v", row, err)
	}
	if err := b.Drain(ctx, old.ID, false); !ent.IsNotFound(err) {
		t.Fatalf("archived registration can still be resumed: %v", err)
	}
	var historical string
	if err := conn.QueryRow(ctx, "SELECT legacy_capability FROM requests WHERE request_id = $1", legacyHeld.ID).Scan(&historical); err != nil || historical != "cloth_gen" {
		t.Fatalf("old execution identity was lost: %q %v", historical, err)
	}
	if row, err := migrated.Request.Get(ctx, legacyQueued.ID); err != nil || row.State != request.StateFailed ||
		row.ErrorCode != "model_retired" || row.QueuePublishAt != nil || row.ModelKey != old.ModelKey {
		t.Fatalf("legacy queue entry was reinterpreted or lost: %v %v", row, err)
	}
	if task, err := b.Claim(ctx, testConfig(), legacyQueued.ID); err != nil || task != nil {
		t.Fatalf("redelivery dispatched a retired request: %v %v", task, err)
	}
	if task, err := b.Claim(ctx, testConfig(), queued.ID); err != nil || task == nil ||
		task.InstanceID != wardrobe.ID || !task.SlotReserved {
		t.Fatalf("current queued request was not assigned to the matching registration: %v %v", task, err)
	}
	if row, err := migrated.Request.Get(ctx, running.ID); err != nil || !row.SlotReserved || row.Units != running.Units ||
		row.InstanceID != video.ID || row.ResourceGroup != video.ResourceGroup || row.ArtifactDigest != video.Artifact {
		t.Fatalf("in-flight ownership or units changed: %v %v", row, err)
	}
	poll, err := b.ClaimPoll(ctx)
	if err != nil || poll == nil || poll.ID != running.ID || poll.ModelKey != video.ModelKey ||
		poll.Endpoint != video.Endpoint || poll.BackendTaskID != "existing-native-job" {
		t.Fatalf("in-flight request did not resume with its original job: %v %v", poll, err)
	}
	if err := b.RecordResponse(ctx, poll.ID, poll.PollClaim, succeeded(poll.ID), time.Second); err != nil {
		t.Fatal(err)
	}
	if row, err := migrated.Request.Get(ctx, running.ID); err != nil || row.State != request.StateSucceeded || row.SlotReserved {
		t.Fatalf("terminal video did not release its original slot: %v %v", row, err)
	}
	// 新记录在迁移后的表里可写，旧列不能继续是受理或登记的必填项。
	newModel := testInstance(protocol.ClothGenModel, old.Endpoint, 2)
	if err := b.Register(ctx, newModel, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Admit(ctx, testAdmission(newModel, uuid.NewString(), 1)); err != nil {
		t.Fatal(err)
	}
}
