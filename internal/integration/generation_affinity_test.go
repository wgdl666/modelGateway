package integration

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestGenerationRetryKeepsOriginalWorkerUnlessNotStarted(t *testing.T) {
	for _, reason := range []string{"dispatch_outcome_unknown", "worker_busy_before_execution"} {
		t.Run(reason, func(t *testing.T) {
			ledger, _, db := stores(t)
			a := testInstance("new-generic-model", "http://127.0.0.1:8001", 1)
			a.ID = "worker-a"
			a.Protocol = protocol.ImageGenerationProtocol
			b := a
			b.ID = "worker-b"
			b.Endpoint = "http://127.0.0.1:8002"
			b.HealthURL = b.Endpoint + "/health"
			registerReady(t, ledger, a)
			registerReady(t, ledger, b)
			cfg := testConfig()
			cfg.NativeResults = config.NativeResults{Provider: "s3", Region: "test", Bucket: "test"}
			cfg.NativeExecutionTimeout = time.Minute
			id := uuid.NewString()
			if _, err := ledger.Admit(t.Context(), testAdmission(a, id, 1)); err != nil {
				t.Fatal(err)
			}
			claimed, err := ledger.Claim(t.Context(), cfg, id)
			if err != nil || claimed == nil || claimed.InstanceID != a.ID {
				t.Fatalf("first claim: %+v %v", claimed, err)
			}
			if err := ledger.RequeueDispatch(t.Context(), id, *claimed.StartedAt, claimed.PollClaim, reason); err != nil {
				t.Fatal(err)
			}
			if err := ledger.Drain(t.Context(), a.ID, true); err != nil {
				t.Fatal(err)
			}
			if err := db.Request.UpdateOneID(id).SetQueuePublishAt(time.Now().Add(-time.Second)).Exec(t.Context()); err != nil {
				t.Fatal(err)
			}
			retry, err := ledger.Claim(t.Context(), cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			if reason == "worker_busy_before_execution" {
				if retry == nil || retry.InstanceID != b.ID {
					t.Fatalf("explicit not-started response did not allow another instance: %+v", retry)
				}
			} else {
				if retry != nil {
					t.Fatalf("ambiguous dispatch switched worker: %+v", retry)
				}
				saved, err := db.Request.Get(t.Context(), id)
				if err != nil || saved.Endpoint != a.Endpoint || saved.State != request.StateQueued {
					t.Fatalf("lost original endpoint: %+v %v", saved, err)
				}
			}
		})
	}
}
