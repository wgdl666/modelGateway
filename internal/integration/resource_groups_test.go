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
	a, b, db := stores(t)
	ctx := t.Context()
	cfg := testConfig()
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(a, runtime, nil))
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
	const machinePath = groupsPath + "/machine"
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
			groups[0].ID != image.ResourceGroup || groups[0].MachineID != image.MachineID ||
			groups[0].GpuDevice == nil || *groups[0].GpuDevice != 0 ||
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
	finish(waiting)

	active := admit(image)
	claim(a, active, true)
	// 一次更正多张卡和关联实例，允许合并到卡号互不冲突的现有机器标识。
	sibling := testInstance(protocol.BodyModel, "http://127.0.0.1:8001", 1)
	sibling.ResourceGroup, sibling.GPUDevices = "host/gpu2", []int{2}
	registerReady(t, a, sibling)
	const canonical = "i-0123456789abcdef0"
	call("POST", groupsPath, map[string]any{"id": "canonical/gpu1", "machine_id": canonical, "gpu_device": 1, "capacity_units": 1}, adminToken, 201)
	rename := map[string]string{"machine_id": image.MachineID, "new_machine_id": canonical}
	call("PATCH", machinePath, rename, apiToken, 401)
	call("PATCH", machinePath, rename, adminToken, 200)
	call("POST", registerPath, image, adminToken, 409)
	image.MachineID, product.MachineID, sibling.MachineID = canonical, canonical, canonical

	// 另一台登记了相同卡号时，整台机器的更名必须回滚。
	call("POST", groupsPath, map[string]any{"id": "other/gpu2", "machine_id": "other", "gpu_device": 2, "capacity_units": 1}, adminToken, 201)
	call("PATCH", machinePath, map[string]string{"machine_id": canonical, "new_machine_id": "other"}, adminToken, 409)
	for _, spec := range []protocol.Instance{image, sibling} {
		group, err := db.ResourceGroup.Get(ctx, spec.ResourceGroup)
		if err != nil || group.MachineID != canonical || group.GpuDevice == nil ||
			*group.GpuDevice != spec.GPUDevices[0] || group.CapacityUnits != 1 {
			t.Fatalf("unexpected resource group after rename and conflict: %v (%v)", group, err)
		}
	}
	for _, spec := range []protocol.Instance{image, product, sibling} {
		call("POST", registerPath, spec, adminToken, 200)
	}
	held, err := db.Request.Get(ctx, active)
	if err != nil || !held.SlotReserved || held.ResourceGroup != image.ResourceGroup {
		t.Fatalf("machine rename changed in-flight allocation: %v (%v)", held, err)
	}

	// 历史组保留容量入口，空标识不能把无机器归属的组误选成一台机器。
	legacy, err := db.ResourceGroup.Create().SetID("cn-dev/vmind").SetCapacityUnits(1).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	call("PATCH", machinePath, map[string]string{"machine_id": "", "new_machine_id": canonical}, adminToken, 400)
	call("PATCH", groupsPath, map[string]any{"id": legacy.ID, "capacity_units": 2}, adminToken, 200)

	// 实例编辑可同时调整地址和资源组，历史请求仍按派发时的账本回查。
	path := "/v1/instances/" + image.ID
	lookup := func() store.InstanceView {
		t.Helper()
		var rows []store.InstanceView
		if err := json.Unmarshal(call("GET", "/v1/instances", nil, adminToken, 200), &rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == image.ID {
				return row
			}
		}
		t.Fatal("instance missing from registry")
		return store.InstanceView{}
	}
	settings := protocol.InstanceSettings{
		Endpoint: "http://127.0.0.2:8002", HealthURL: "http://127.0.0.2:8002/health",
		ResourceGroup: "host/gpu1", Capacity: 2, ConfigRevision: lookup().ConfigRevision,
	}
	call("POST", path+"/edit", settings, apiToken, 401)
	call("POST", path+"/edit", settings, adminToken, 409)
	call("POST", path+"/drain", map[string]bool{"draining": true}, adminToken, 200)
	settings.ConfigRevision = lookup().ConfigRevision
	call("POST", path+"/edit", settings, adminToken, 409)
	finish(active)
	call("POST", path+"/edit", settings, adminToken, 404)
	call("POST", groupsPath, map[string]any{"id": settings.ResourceGroup, "machine_id": "host", "gpu_device": 1, "capacity_units": 3}, adminToken, 201)
	oldProbe := time.Now()
	call("POST", path+"/edit", settings, adminToken, 200)
	row := lookup()
	if row.Endpoint != settings.Endpoint || row.HealthURL != settings.HealthURL || row.ResourceGroup != settings.ResourceGroup ||
		row.GroupCapacityUnits != 3 || row.ArtifactDigest != image.Artifact || !row.Draining || row.Healthy ||
		row.ConfigRevision == settings.ConfigRevision || len(row.GpuDevices) != 1 || row.GpuDevices[0] != 1 {
		t.Fatalf("incorrect readback: %+v", row)
	}
	call("POST", path+"/edit", settings, adminToken, 200)
	stale := settings
	stale.Capacity++
	call("POST", path+"/edit", stale, adminToken, 409)
	previous, err := db.Request.Get(ctx, active)
	if err != nil || previous.Endpoint != image.Endpoint || previous.ResourceGroup != image.ResourceGroup {
		t.Fatalf("historical dispatch changed: %v %v", previous, err)
	}
	call("POST", path+"/drain", map[string]bool{"draining": false}, adminToken, 200)
	if err := a.RecordProbe(ctx, image.ID, oldProbe, true); err != nil || lookup().Healthy {
		t.Fatalf("old probe marked edited endpoint healthy: %v", err)
	}
	if err := a.RecordProbe(ctx, image.ID, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	next := admit(image)
	task, err := b.Claim(ctx, cfg, next)
	if err != nil || task == nil || task.Endpoint != settings.Endpoint || task.ResourceGroup != settings.ResourceGroup {
		t.Fatalf("new task did not use edited instance: %v %v", task, err)
	}
}
