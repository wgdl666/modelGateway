package protocol

import (
	"crypto/sha256"
	"fmt"
)

// Identity is supplied by the loaded process; addresses and capacity are granted
// separately by the regional operator, never accepted from a worker request.
type WorkerSession struct {
	Region       string          `json:"region"`
	DeploymentID string          `json:"deployment_id"`
	BootID       string          `json:"boot_id"`
	Bindings     []WorkerBinding `json:"bindings,omitempty"`
}

type WorkerBinding struct {
	ModelKey string `json:"model_key"`
	Artifact string `json:"artifact_digest"`
}

func WorkerInstanceID(worker, deployment, boot, model string) string {
	return fmt.Sprintf("worker-%x", sha256.Sum256([]byte(worker+"\x00"+deployment+"\x00"+boot+"\x00"+model)))
}
