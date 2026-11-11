package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/store"
)

func TestRequestStatsSummarizesModelsAndFailures(t *testing.T) {
	a, b, client := stores(t)
	to := time.Now().UTC().Truncate(time.Hour)
	from := to.Add(-24 * time.Hour)
	add := func(model string, state request.State, created time.Time, queue, run time.Duration, instance, code string) {
		t.Helper()
		create := client.Request.Create().SetID(uuid.NewString()).SetModelKey(model).SetProtocol("async").
			SetRequestPayload(json.RawMessage(`{}`)).SetPayloadBytes(2).SetUnits(1).SetState(state).
			SetDeadlineAt(created.Add(time.Hour)).SetCreatedAt(created).SetInstanceID(instance).SetErrorCode(code).
			SetErrorMessage(code + " message")
		if state != request.StateQueued {
			create.SetStartedAt(created.Add(queue))
		}
		if state == request.StateSucceeded || state == request.StateFailed {
			create.SetFinishedAt(created.Add(queue + run))
		}
		if _, err := create.Save(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	early := from.Add(30 * time.Minute)
	late := to.Add(-30 * time.Minute)
	for i := range 4 {
		add("Vmind_outfit_v1.0", request.StateSucceeded, early, time.Second, time.Duration(10+i)*time.Second, "outfit-1", "")
	}
	add("Vmind_outfit_v1.0", request.StateFailed, late, time.Second, time.Minute, "outfit-1", "provider_timeout")
	add("Vmind_outfit_v1.0", request.StateFailed, late, time.Second, time.Minute, "outfit-1", "provider_timeout")
	add("Vmind_outfit_v1.0", request.StateFailed, late, time.Second, time.Minute, "outfit-2", "invalid_output")
	add("Vmind_outfit_v1.0", request.StateQueued, late, 0, 0, "", "")
	// 窗口外的完成请求不计入量和分位，仍在执行的旧请求计入当前积压。
	add("Vmind_outfit_v1.0", request.StateSucceeded, from.Add(-time.Hour), time.Second, time.Hour, "outfit-1", "")
	add("Vmind_bodymodel_v1.0", request.StateProcessing, from.Add(-time.Hour), time.Second, 0, "body-1", "")

	stats, err := a.RequestStats(t.Context(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BucketSeconds != 3600 || len(stats.Models) != 2 {
		t.Fatalf("unexpected stats shape %+v", stats)
	}
	body, outfit := stats.Models[0], stats.Models[1]
	if body.ModelKey != "Vmind_bodymodel_v1.0" || body.Total != 0 || body.Processing != 1 || len(body.SeriesTotal) != store.StatsBuckets {
		t.Fatalf("backlog-only model %+v", body)
	}
	if outfit.Total != 8 || outfit.Succeeded != 4 || outfit.Failed != 3 || outfit.Queued != 1 || outfit.Processing != 0 {
		t.Fatalf("outfit counts %+v", outfit)
	}
	if outfit.ElapsedP50MS == nil || *outfit.ElapsedP50MS != 12500 || outfit.QueueP50MS == nil || *outfit.QueueP50MS != 1000 {
		t.Fatalf("outfit percentiles p50=%v queue=%v", outfit.ElapsedP50MS, outfit.QueueP50MS)
	}
	if outfit.SeriesTotal[0] != 4 || outfit.SeriesTotal[23] != 4 || outfit.SeriesFailed[23] != 3 || outfit.SeriesFailed[0] != 0 {
		t.Fatalf("outfit series total=%v failed=%v", outfit.SeriesTotal, outfit.SeriesFailed)
	}
	if len(stats.Failures) != 2 || stats.Failures[0].ErrorCode != "provider_timeout" || stats.Failures[0].Count != 2 ||
		stats.Failures[0].InstanceID != "outfit-1" || stats.Failures[0].SampleID == "" ||
		stats.Failures[0].SampleMessage != "provider_timeout message" || stats.Failures[1].ErrorCode != "invalid_output" {
		t.Fatalf("failure groups %+v", stats.Failures)
	}

	runtime, err := config.NewRuntime(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(b, runtime, nil))
	get := func(path, token string, want int) []byte {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s: status=%d response=%s err=%v", path, res.StatusCode, raw, err)
		}
		return raw
	}
	get("/v1/admin/request-stats", apiToken, 401)
	get("/v1/admin/request-stats?from=yesterday", adminToken, 400)
	get("/v1/admin/request-stats?from="+to.Add(-30*time.Minute).Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), adminToken, 400)
	get("/v1/admin/request-stats?from="+to.Add(-8*24*time.Hour).Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), adminToken, 400)
	raw := get("/v1/admin/request-stats?from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), adminToken, 200)
	var view store.RequestStats
	if err := json.Unmarshal(raw, &view); err != nil || len(view.Models) != 2 || len(view.Failures) != 2 {
		t.Fatalf("stats response %s err=%v", raw, err)
	}
}
