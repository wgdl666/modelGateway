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
	vision          map[string]upstreamhealth.VisionService
	dreamReady      bool
	decisionWorkers int
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
		Workers    int `json:"workers"`
		ReadyCount int `json:"ready_count"`
	}
	// Decision 只报告就绪总数。全部就绪时才能确认每个已登记的 worker。
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&status) == nil && status.ReadyCount == status.Workers {
		return synchronousHealth{decisionWorkers: status.Workers}
	}
	return synchronousHealth{}
}

func (h synchronousHealth) ready(model, endpoint string, instances int) bool {
	switch model {
	case protocol.DreamModel:
		return h.dreamReady
	case protocol.DecisionModel:
		return h.decisionWorkers >= instances
	}
	service := h.vision[protocol.VisionService(model)]
	switch model {
	case protocol.SegmentPersonModel, protocol.SegmentSubjectModel, protocol.LensCorrectionModel:
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
		return service.Ready != nil && *service.Ready
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
		if kind := protocol.SynchronousKind(row.ModelKey); kind != "" {
			batches[kind] = append(batches[kind], row)
		}
	}
	// 每轮每个上游只探一次，避免一张卡的多个模型登记重复触发整机探活。
	var probes sync.WaitGroup
	for kind, instances := range batches {
		probes.Go(func() {
			var health synchronousHealth
			if probe := cfg.SynchronousHealthURL(instances[0].ModelKey); probe != "" {
				health = w.readSynchronousHealth(w.ctx, kind, probe)
			}
			for _, row := range instances {
				ready := cfg.AcceptsInstanceAddress(row.ModelKey, row.Endpoint, row.HealthURL) &&
					health.ready(row.ModelKey, row.Endpoint, len(instances))
				if err := w.store.RecordProbe(w.ctx, row.ID, checked, ready); err != nil && w.ctx.Err() == nil {
					slog.Error("synchronous model health update failed", "error_type", fmt.Sprintf("%T", err))
				}
			}
		})
	}
	probes.Wait()
	return nil
}
