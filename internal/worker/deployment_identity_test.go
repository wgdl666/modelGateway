package worker

import (
	"encoding/json"
	"github.com/wgdl666/modelGateway/ent"
	"net/http"
	"strings"
	"testing"
)

func TestDeploymentFenceOnRequestsAndResponses(t *testing.T) {
	task := &ent.Request{DeploymentID: "cn-body-gpu1-v2", ArtifactDigest: "sha256-" + strings.Repeat("a", 64)}
	h := make(http.Header)
	setDeploymentHeaders(h, task)
	if h.Get("X-Expected-Deployment-ID") != task.DeploymentID || h.Get("X-Expected-Artifact-Digest") != task.ArtifactDigest {
		t.Fatal(h)
	}
	for _, values := range []map[string]string{
		{}, {"deployment_id": task.DeploymentID, "artifact_digest": task.ArtifactDigest},
		{"deployment_id": "old", "artifact_digest": task.ArtifactDigest, "boot_id": "boot"},
		{"deployment_id": task.DeploymentID, "artifact_digest": "wrong", "boot_id": "boot"},
		{"deployment_id": task.DeploymentID, "artifact_digest": task.ArtifactDigest, "boot_id": "boot"},
	} {
		raw, _ := json.Marshal(values)
		want := values["deployment_id"] == task.DeploymentID && values["artifact_digest"] == task.ArtifactDigest && values["boot_id"] != ""
		if deploymentResponseMatches(raw, task) != want {
			t.Fatal(values)
		}
	}
	if !deploymentResponseMatches([]byte(`{}`), &ent.Request{}) {
		t.Fatal("legacy changed")
	}
}

func TestWorkerBootMustMatchAssignmentAndResult(t *testing.T) {
	task := &ent.Request{DeploymentID: "d", BootID: "boot-2", ModelKey: "body", ArtifactDigest: "sha256-" + strings.Repeat("a", 64)}
	headers := make(http.Header)
	setDeploymentHeaders(headers, task)
	if headers.Get("X-Expected-Boot-ID") != task.BootID {
		t.Fatal("missing boot fence")
	}
	for _, boot := range []string{"", "boot-1", "boot-2"} {
		raw, _ := json.Marshal(map[string]string{"deployment_id": task.DeploymentID, "artifact_digest": task.ArtifactDigest, "boot_id": boot})
		if deploymentResponseMatches(raw, task) != (boot == task.BootID) {
			t.Fatal("incorrect boot accepted", boot)
		}
	}
}
