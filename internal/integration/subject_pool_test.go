package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func subjectPoolConfig() config.Config {
	cfg := testConfig()
	cfg.VisionAddr = "172.31.43.7:18280"
	cfg.AllowedNetworks = append(cfg.AllowedNetworks, "10.0.0.0/8")
	cfg.AllowedPorts = append(cfg.AllowedPorts, "80")
	cfg.SubjectPoolAPIKey = "fixture-secret"
	endpoint := "http://10.0.0.2/internal/segment-subject-pool"
	cfg.SubjectPools = map[string]config.SubjectPool{protocol.SegmentSubjectModel: {Protocol: protocol.SegmentSubjectProtocol, Endpoint: endpoint, HealthURL: endpoint + "/ready"}}
	return cfg
}
func TestSubjectPoolInitiallyDrainingAndAggregateCapacity(t *testing.T) {
	a, b, db := stores(t)
	cfg := subjectPoolConfig()
	p := cfg.SubjectPools[protocol.SegmentSubjectModel]
	spec := testInstance(protocol.SegmentSubjectModel, p.Endpoint, 2)
	spec.HealthURL = p.HealthURL
	spec.InitiallyDraining = true
	ensureTestGroup(t, a, spec)
	if err := a.Register(t.Context(), spec, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordProbe(t.Context(), spec.ID, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 3 {
		id := uuid.NewString()
		ids = append(ids, id)
		if _, err := a.Admit(t.Context(), testAdmission(spec, id, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := a.Claim(t.Context(), cfg, ids[0]); err != nil || got != nil {
		t.Fatalf("staged row claimed: %+v %v", got, err)
	}
	if err := a.Drain(t.Context(), spec.ID, false); err != nil {
		t.Fatal(err)
	}
	// Registration retries preserve the operator's current drain state.
	if err := a.Register(t.Context(), spec, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordProbe(t.Context(), spec.ID, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		task, err := b.Claim(t.Context(), cfg, id)
		if err != nil {
			t.Fatal(err)
		}
		if (task != nil) != (i < 2) {
			t.Fatalf("claim %d: %+v", i, task)
		}
	}
	held, err := db.Request.Query().Where(request.SlotReservedEQ(true)).Count(t.Context())
	if err != nil || held != 2 {
		t.Fatalf("reserved=%d %v", held, err)
	}
}
func TestSubjectPoolHTTPRegistrationAndUnknown504(t *testing.T) {
	a, _, db := stores(t)
	cfg := subjectPoolConfig()
	p := cfg.SubjectPools[protocol.SegmentSubjectModel]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_dispatch/6106/segment-person/api/segment_subject" {
			if r.Header.Get("X-Segment-Pool-Key") != "" {
				t.Error("pool secret leaked to legacy inference")
			}
			io.WriteString(w, `{"ok":true}`)
			return
		}
		if r.URL.Path != "/internal/segment-subject-pool/api/segment_subject" || r.Header.Get("X-Segment-Pool-Key") != "fixture-secret" {
			t.Errorf("wrong pool request %s", r.URL)
		}
		w.Header().Set("X-Model-Outcome", "unknown")
		w.WriteHeader(504)
		io.WriteString(w, `{"error":"active timeout"}`)
	}))
	defer upstream.Close()
	base := synchronousServer(t, a, cfg, upstream)
	spec := testInstance(protocol.SegmentSubjectModel, p.Endpoint, 2)
	spec.HealthURL = p.HealthURL
	spec.InitiallyDraining = true
	ensureTestGroup(t, a, spec)
	send := func(spec protocol.Instance, want int) {
		t.Helper()
		raw, _ := json.Marshal(spec)
		req, _ := http.NewRequestWithContext(t.Context(), "POST", base+"/v1/instances/register", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("register=%d %s", resp.StatusCode, body)
		}
	}
	bad := spec
	bad.ModelKey = protocol.SegmentPersonModel
	bad.Protocol = protocol.SegmentPersonProtocol
	send(bad, 400)
	bad = spec
	bad.HealthURL += "/other"
	send(bad, 400)
	send(spec, 200)
	row, err := db.ModelInstance.Get(t.Context(), spec.ID)
	if err != nil || !row.Draining {
		t.Fatalf("not atomically draining: %+v %v", row, err)
	}
	if err := a.Drain(t.Context(), spec.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordProbe(t.Context(), spec.ID, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	resp := sendSynchronous(t, base, "/v1/vision/segment-person/api/segment_subject", "multipart/form-data; boundary=fixture", "--fixture\r\nContent-Disposition: form-data; name=\"image\"\r\n\r\naW1hZ2U=\r\n--fixture--\r\n", id, protocol.SegmentSubjectModel)
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 504 {
		t.Fatalf("response=%d %v", resp.StatusCode, err)
	}
	result := waitSynchronousState(t, db, id, request.StateFailed, true)
	if result.Endpoint != p.Endpoint || result.ErrorCode != "backend_outcome_unknown" {
		t.Fatalf("lost pool ledger identity: %+v", result)
	}
	// Even with the pool configured and an unknown request held, a retained legacy
	// row keeps its loopback dispatch identity and receives no pool credentials.
	if err := a.Drain(t.Context(), spec.ID, true); err != nil {
		t.Fatal(err)
	}
	legacy := testInstance(protocol.SegmentSubjectModel, "http://127.0.0.1:6106", 1)
	legacy.HealthURL = cfg.SynchronousHealthURL(legacy.Protocol)
	legacy.ResourceGroup = "old-host/gpu0"
	legacy.MachineID = "old-host"
	registerReady(t, a, legacy)
	legacyID := uuid.NewString()
	legacyResp := sendSynchronous(t, base, "/v1/vision/segment-person/api/segment_subject", "multipart/form-data; boundary=fixture", "--fixture\r\nContent-Disposition: form-data; name=\"image\"\r\n\r\naW1hZ2U=\r\n--fixture--\r\n", legacyID, protocol.SegmentSubjectModel)
	_, err = io.ReadAll(legacyResp.Body)
	legacyResp.Body.Close()
	if err != nil || legacyResp.StatusCode != 200 {
		t.Fatalf("legacy response=%d %v", legacyResp.StatusCode, err)
	}
	waitSynchronousState(t, db, legacyID, request.StateSucceeded, false)

}
