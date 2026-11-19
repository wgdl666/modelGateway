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
	Ready    *bool           `json:"ready"`
	Artifact json.RawMessage `json:"artifact_digest"`
	Models   []string        `json:"models"`
}

func (h healthReport) result(ready bool) protocol.InstanceHealth {
	result := protocol.InstanceHealth{Ready: ready}
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
func (w *Reconciler) ProbeInstance(ctx context.Context, cfg *config.Snapshot, target protocol.InstanceTarget) (protocol.InstanceHealth, error) {
	if p, ok := cfg.SubjectPoolFor(target.ModelKey, target.Protocol, target.Endpoint); ok && target.HealthURL == p.HealthURL {
		return protocol.InstanceHealth{Ready: w.readSubjectPoolHealth(ctx, cfg, p)}, nil
	}
	if kind := protocol.SynchronousKind(target.Protocol); kind != "" {
		probe := cfg.SynchronousHealthURL(target.Protocol)
		if kind == protocol.SyncEmbedding {
			probe = target.HealthURL
		}
		health := w.readSynchronousHealth(ctx, kind, probe)
		return protocol.InstanceHealth{Ready: health.ready(target.Protocol, target.Endpoint, 1)}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if backend, direct := cfg.NativeBackendFor(target.ModelKey, target.Endpoint); direct && !protocol.GenerationProtocol(target.Protocol) {
		return w.nativeHealth(ctx, cfg, target, backend)
	}
	var health healthReport
	if protocol.GenerationProtocol(target.Protocol) {
		// Never send an endpoint's private native credential to an independent
		// health service. Only paths under the registered endpoint inherit it.
		auth := nativeAuth{bearer: cfg.BackendToken}
		if target.HealthURL == target.Endpoint || strings.HasPrefix(target.HealthURL, target.Endpoint+"/") {
			auth = w.generationAuth(cfg, target.ModelKey, target.Endpoint)
		}
		if err := w.nativeJSON(ctx, auth, target.HealthURL, &health); err != nil {
			return protocol.InstanceHealth{}, err
		}
	} else {
		raw, err := w.modelCall(ctx, cfg, http.MethodGet, target.HealthURL, "", nil)
		if err != nil {
			return protocol.InstanceHealth{}, err
		}
		if err := json.Unmarshal(raw, &health); err != nil {
			return protocol.InstanceHealth{}, err
		}
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
