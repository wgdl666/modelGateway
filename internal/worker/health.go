package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

type healthReport struct {
	DeploymentID string          `json:"deployment_id"`
	BootID       string          `json:"boot_id"`
	Ready        *bool           `json:"ready"`
	Artifact     json.RawMessage `json:"artifact_digest"`
	Models       []string        `json:"models"`
}

func (h healthReport) result(ready bool) protocol.InstanceHealth {
	result := protocol.InstanceHealth{Ready: ready, DeploymentID: h.DeploymentID, BootID: h.BootID}
	var digest string
	// 摘要字段未上报或格式有误时，仍按健康字段判断服务能否接单。
	if json.Unmarshal(h.Artifact, &digest) == nil {
		digest = strings.Replace(digest, "sha256:", "sha256-", 1)
		if protocol.ValidArtifact(digest) {
			result.ArtifactDigest = strings.ToLower(digest)
		}
	}
	return result
}

// ProbeInstance 供后台探活和手动测试共用执行端协议及凭据。调用方先核对登记地址。
func (w *Reconciler) probeInstanceRaw(ctx context.Context, cfg *config.Snapshot, target protocol.InstanceTarget) (protocol.InstanceHealth, error) {
	if kind := protocol.SynchronousKind(target.Protocol); kind != "" {
		health := w.readSynchronousHealth(ctx, kind, cfg.SynchronousHealthURL(target.Protocol))
		return protocol.InstanceHealth{Ready: health.ready(target.Protocol, target.Endpoint, 1)}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if backend, direct := cfg.NativeBackendFor(target.ModelKey, target.Endpoint); direct && !protocol.GenerationProtocol(target.Protocol) {
		return w.nativeHealth(ctx, cfg, target, backend)
	}
	raw, err := w.modelCall(ctx, cfg, http.MethodGet, target.HealthURL, "", nil)
	if err != nil {
		return protocol.InstanceHealth{}, err
	}
	var health healthReport
	if err := json.Unmarshal(raw, &health); err != nil {
		return protocol.InstanceHealth{}, err
	}
	ready := health.Ready != nil && *health.Ready
	if protocol.GenerationProtocol(target.Protocol) {
		ready = ready && slices.Contains(health.Models, target.ModelKey)
	}
	result := health.result(ready)
	// 独立探针可报告健康，只有模型服务自身的响应能提供该实例的镜像摘要。
	if target.HealthURL != target.Endpoint && !strings.HasPrefix(target.HealthURL, target.Endpoint+"/") {
		result.ArtifactDigest = ""
	}
	return result, nil
}

// Strict deployments are routable only while the actual loaded identity matches.
// Historical registrations without deployment_id retain their original contract.
func (w *Reconciler) ProbeInstance(ctx context.Context, cfg *config.Snapshot, target protocol.InstanceTarget) (protocol.InstanceHealth, error) {
	h, err := w.probeInstanceRaw(ctx, cfg, target)
	if target.DeploymentID != "" {
		h.Ready = h.Ready && h.DeploymentID == target.DeploymentID && h.ArtifactDigest == target.ArtifactDigest && h.BootID != ""
	}
	return h, err
}
