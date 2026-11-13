package worker

import (
	"encoding/json"
	"github.com/wgdl666/modelGateway/ent"
	"net/http"
)

func setDeploymentHeaders(headers http.Header, task *ent.Request) {
	if task.DeploymentID == "" {
		return
	}
	headers.Set("X-Expected-Deployment-ID", task.DeploymentID)
	headers.Set("X-Expected-Model-Key", task.ModelKey)
	if task.BootID != "" {
		headers.Set("X-Expected-Boot-ID", task.BootID)
	}
	headers.Set("X-Expected-Artifact-Digest", task.ArtifactDigest)
}
func deploymentResponseMatches(raw []byte, task *ent.Request) bool {
	if task.DeploymentID == "" {
		return true
	}
	var identity struct {
		Deployment string `json:"deployment_id"`
		Artifact   string `json:"artifact_digest"`
		Boot       string `json:"boot_id"`
	}
	return json.Unmarshal(raw, &identity) == nil && identity.Deployment == task.DeploymentID && identity.Artifact == task.ArtifactDigest && identity.Boot != "" && (task.BootID == "" || identity.Boot == task.BootID)
}
