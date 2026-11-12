package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/upstreamhealth"
)

type synchronousHealth struct {
	vision           map[string]upstreamhealth.VisionService
	dreamReady       bool
	decisionReplicas map[string]bool
}

func (w *Reconciler) readSynchronousHealth(ctx context.Context, kind, healthURL string) synchronousHealth {
	if kind == protocol.SyncVision {
		return synchronousHealth{vision: upstreamhealth.ReadVision(ctx, w.client, healthURL)}
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return synchronousHealth{}
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return synchronousHealth{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return synchronousHealth{}
	}
	if kind == protocol.SyncDream {
		return synchronousHealth{dreamReady: true}
	}
	var status struct {
		TargetedDispatch bool `json:"targeted_dispatch"`
		Replicas         []struct {
			Endpoint string `json:"endpoint"`
			Ready    bool   `json:"ready"`
		} `json:"replicas"`
	}
	// 必须由支持定向派发的前置服务报告每个 worker，聚合计数无法证明所选副本就绪。
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&status) == nil && status.TargetedDispatch {
		replicas := make(map[string]bool, len(status.Replicas))
		for _, replica := range status.Replicas {
			replicas[replica.Endpoint] = replica.Ready
		}
		return synchronousHealth{decisionReplicas: replicas}
	}
	return synchronousHealth{}
}

func (h synchronousHealth) ready(kind, endpoint string, _ int) bool {
	switch kind {
	case protocol.ChatProtocol:
		return h.dreamReady
	case protocol.DecisionProtocol:
		return h.decisionReplicas[endpoint]
	}
	service := h.vision[protocol.VisionService(kind)]
	// 旧前置服务会自行改派副本，未支持定向协议时不能进入统一调度。
	if !service.TargetedDispatch {
		return false
	}
	switch kind {
	case protocol.SegmentPersonProtocol, protocol.SegmentSubjectProtocol, protocol.LensCorrectionProtocol:
		// 聚合服务可能还有可用副本，按本机端口判断这条登记对应的副本。
		port, _ := strconv.Atoi(strings.TrimPrefix(endpoint, "http://127.0.0.1:"))
		var detail struct {
			Replicas []struct {
				Port  int  `json:"port"`
				Ready bool `json:"ready"`
			} `json:"replicas"`
		}
		if json.Unmarshal(service.Health, &detail) != nil {
			return false
		}
		for _, replica := range detail.Replicas {
			if replica.Port == port {
				return replica.Ready
			}
		}
		return false
	default:
		return service.Endpoint == endpoint && service.Ready != nil && *service.Ready
	}
}

func (w *Reconciler) probeSynchronous(cfg *config.Snapshot) error {
	checked := time.Now().UTC()
	rows, err := w.store.ProbeInstances(w.ctx)
	if err != nil {
		return err
	}
	batches := make(map[string][]*ent.ModelInstance)
	for _, row := range rows {
		if kind := protocol.SynchronousKind(row.Protocol); kind != "" {
			key := kind
			if _, pool := cfg.SubjectPoolFor(row.ModelKey, row.Protocol, row.Endpoint); pool {
				key = "subject_pool:" + row.Endpoint
			}
			batches[key] = append(batches[key], row)
		}
	}
	// 每轮每个上游只探一次，避免一张卡的多个模型登记重复触发整机探活。
	var probes sync.WaitGroup
	for kind, instances := range batches {
		probes.Go(func() {
			var health synchronousHealth
			p, pool := cfg.SubjectPoolFor(instances[0].ModelKey, instances[0].Protocol, instances[0].Endpoint)
			poolReady := false
			if pool {
				poolReady = w.readSubjectPoolHealth(w.ctx, cfg, p)
			}
			if probe := cfg.SynchronousHealthURL(instances[0].Protocol); probe != "" && !pool {
				health = w.readSynchronousHealth(w.ctx, kind, probe)
			}
			for _, row := range instances {
				ready := cfg.AcceptsInstanceAddress(row.ModelKey, row.Protocol, row.Endpoint, row.HealthURL) &&
					((pool && poolReady) || (!pool && health.ready(row.Protocol, row.Endpoint, len(instances))))
				if err := w.store.RecordProbe(w.ctx, row.ID, checked, ready); err != nil && w.ctx.Err() == nil {
					slog.Error("synchronous model health update failed", "error_type", fmt.Sprintf("%T", err))
				}
			}
		})
	}
	probes.Wait()
	return nil
}

// A successful /ready proves pool ingress availability, not replica count.
func (w *Reconciler) readSubjectPoolHealth(ctx context.Context, cfg *config.Snapshot, p config.SubjectPool) bool {
	ctx, cancel := context.WithTimeout(config.WithSubjectPoolPolicy(ctx, cfg), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.HealthURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-Segment-Pool-Key", cfg.SubjectPoolAPIKey)
	resp, err := w.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
