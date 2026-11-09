package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func TestResourceGroupOwnsCapacityAcrossModels(t *testing.T) {
	a, b, _ := stores(t)
	ctx := t.Context()
	cfg := testConfig()
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(a, runtime))
	call := func(method, path string, body any, token string, want int) []byte {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		response, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s %s got %d %s (%v), want %d", method, path, res.StatusCode, response, err, want)
		}
		return response
	}
	const groupsPath = "/v1/resource-groups"
	const registerPath = "/v1/instances/register"
	image := testInstance(protocol.OutfitModel, "http://127.0.0.1:8001", 2)
	product := testInstance(protocol.ClothGenModel, "http://127.0.0.1:8002", 2)
	call("POST", registerPath, image, adminToken, 404)

	created := map[string]any{"id": image.ResourceGroup, "machine_id": image.MachineID, "gpu_device": 0, "capacity_units": 1}
	call("POST", groupsPath, created, apiToken, 401)
	call("POST", groupsPath, created, adminToken, 201)
	created["id"] = "different-name-for-the-same-card"
	call("POST", groupsPath, created, adminToken, 409)
	checkGroup := func(capacity, reserved, members int) {
		t.Helper()
		var groups []store.ResourceGroupView
		raw := call("GET", groupsPath, nil, adminToken, 200)
		if err := json.Unmarshal(raw, &groups); err != nil || len(groups) != 1 ||
			groups[0].ID != image.ResourceGroup || groups[0].GpuDevice == nil || *groups[0].GpuDevice != 0 ||
			groups[0].CapacityUnits != capacity || groups[0].ReservedUnits != reserved || groups[0].InstanceCount != members {
			t.Fatalf("unexpected group list: %s (%v)", raw, err)
		}
	}
	checkGroup(1, 0, 0)
	wrong := image
	wrong.GPUDevices = []int{1}
	call("POST", registerPath, wrong, adminToken, 409)
	image.GroupCapacity = 50 // 旧部署提交的值不能覆盖组表容量。
	for _, spec := range []protocol.Instance{image, product} {
		call("POST", registerPath, spec, adminToken, 200)
		if err := a.RecordProbe(ctx, spec.ID, time.Now(), true); err != nil {
			t.Fatal(err)
		}
	}

	admit := func(spec protocol.Instance) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := a.Admit(ctx, testAdmission(spec, id, 1)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	claim := func(replica *store.Store, id string, want bool) {
		t.Helper()
		if task, err := replica.Claim(ctx, cfg, id); err != nil || (task != nil) != want {
			t.Fatalf("claim %s: task=%v err=%v, want dispatch=%t", id, task, err, want)
		}
	}
	finish := func(id string) {
		t.Helper()
		if err := a.RecordResponse(ctx, id, "", succeeded(id), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	edit := func(capacity int) {
		call("PATCH", groupsPath, map[string]any{"id": image.ResourceGroup, "capacity_units": capacity}, adminToken, 200)
	}

	first, second := admit(image), admit(product)
	claim(a, first, true)
	claim(b, second, false)
	checkGroup(1, 1, 2)
	finish(first)
	claim(b, second, true)
	edit(2)
	// 改组容量后重试原登记仍幂等，实例视图随组容量变化。
	call("POST", registerPath, image, adminToken, 200)
	rows, err := b.ListInstances(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list instances: %v %v", rows, err)
	}
	for _, row := range rows {
		if row.GroupCapacityUnits != 2 {
			t.Fatalf("instance returned stale group capacity: %+v", row)
		}
	}
	third, waiting := admit(image), admit(product)
	claim(b, third, true)
	edit(1)
	checkGroup(1, 2, 2)
	claim(b, waiting, false)
	finish(second)
	claim(b, waiting, false)
	finish(third)
	claim(b, waiting, true)
}
