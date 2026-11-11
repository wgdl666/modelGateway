package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/httpapi"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

func TestSynchronousRegistrationUsesActualPhysicalGroups(t *testing.T) {
	owner, replica, _ := stores(t)
	cfg := testConfig()
	cfg.VLMAddr = "172.31.3.219:30332"
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := startHTTPServer(t, httpapi.New(owner, runtime, nil))
	ctx := t.Context()

	spec := testInstance(protocol.DreamModel, "http://"+cfg.VLMAddr, 1)
	spec.GPUDevices = []int{0, 1, 2, 3}
	spec.Artifact = ""
	send := func(path, token string, spec protocol.Instance, want int) {
		t.Helper()
		body, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: got %d, want %d", path, resp.StatusCode, want)
		}
	}

	const register = "/v1/instances/register"
	for _, gpu := range []int{0, 1, 2} {
		if _, err := owner.CreateResourceGroup(ctx, fmt.Sprintf("host/gpu%d", gpu), "host", gpu, 1); err != nil {
			t.Fatal(err)
		}
	}
	send(register, adminToken, spec, http.StatusConflict)
	if _, err := owner.CreateResourceGroup(ctx, "host/gpu3", "host", 3, 1); err != nil {
		t.Fatal(err)
	}
	bad := spec
	bad.HealthURL = "http://127.0.0.1:30332/health"
	send(register, adminToken, bad, http.StatusBadRequest)
	send(register, apiToken, spec, http.StatusUnauthorized)
	send(register, adminToken, spec, http.StatusOK)
	send(register, adminToken, spec, http.StatusOK)
	send("/v1/gateway/requests/"+spec.ModelKey, apiToken, spec, http.StatusBadRequest)

	if err := owner.RenameResourceGroupMachine(ctx, "host", "actual-host"); err != nil {
		t.Fatal(err)
	}
	rows, err := replica.ListInstances(ctx)
	wantGroups := []string{"host/gpu0", "host/gpu1", "host/gpu2", "host/gpu3"}
	if err != nil || len(rows) != 1 || rows[0].ID != spec.ID || !rows[0].Synchronous || rows[0].Eligible ||
		rows[0].MachineID != "actual-host" || !slices.Equal(rows[0].ResourceGroups, wantGroups) {
		t.Fatalf("instance memberships: %+v %v", rows, err)
	}
	groups, err := replica.ListResourceGroups(ctx)
	if err != nil || len(groups) != 4 {
		t.Fatalf("resource groups: %+v %v", groups, err)
	}
	for _, group := range groups {
		if group.InstanceCount != 1 || group.SynchronousInstanceCount != 1 || group.ReservedUnits != 0 {
			t.Fatalf("unexpected group membership and load: %+v", group)
		}
	}
}
