package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

const (
	workerResponseLimit = 1 << 20
	probeInterval       = 2 * time.Second
)

// Reconciler 按 PG 账本派发与核对结果，入站连接断开后仍可处理已受理的任务。
type Reconciler struct {
	store   *store.Store
	runtime *config.Runtime
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func New(tasks *store.Store, runtime *config.Runtime) *Reconciler {
	ctx, cancel := context.WithCancel(context.Background())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Reconciler{store: tasks, runtime: runtime, ctx: ctx, cancel: cancel, client: client}
}

func (w *Reconciler) Close() {
	w.cancel()
	w.wg.Wait()
	w.client.CloseIdleConnections()
}

type modelStatusError int

func (status modelStatusError) Error() string { return fmt.Sprintf("model HTTP %d", status) }

func (w *Reconciler) modelCall(ctx context.Context, cfg *config.Snapshot, method, origin, path string, body []byte) ([]byte, error) {
	if err := cfg.ValidateEndpoint(origin, path == ""); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(body) != 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if cfg.BackendToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BackendToken)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, modelStatusError(resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, workerResponseLimit+1))
	if err != nil || len(raw) > workerResponseLimit {
		return nil, errors.New("invalid model response body")
	}
	return raw, nil
}

func (w *Reconciler) execute(cfg *config.Snapshot, task *ent.Request) error {
	var payload protocol.Submission
	if err := json.Unmarshal(task.RequestPayload, &payload); err != nil {
		return w.failDispatch(task.ID, 500, "invalid_saved_request", "saved request is invalid", false)
	}
	raw, err := json.Marshal(protocol.ModelRequest{RequestID: task.ID, ModelKey: task.ModelKey, Capability: task.Capability, Submission: payload})
	if err != nil {
		return w.failDispatch(task.ID, 500, "invalid_saved_request", "saved request is invalid", false)
	}
	if !time.Now().Before(task.DeadlineAt) {
		return w.failDispatch(task.ID, 504, "queue_timeout", "request expired before dispatch", false)
	}
	if cfg.ValidateEndpoint(task.Endpoint, false) != nil {
		return w.failDispatch(task.ID, 503, "backend_unavailable", "selected model address is unavailable", false)
	}
	ctx, cancel := context.WithDeadline(w.ctx, task.DeadlineAt)
	defer cancel()
	response, err := w.modelCall(ctx, cfg, http.MethodPost, task.Endpoint, "/requests", raw)
	if err != nil {
		var status modelStatusError
		if errors.As(err, &status) {
			// 执行端约定这些错误在落受理记录前返回，可以确认 GPU 尚未接单。
			switch status {
			case 400, 413, 415:
				return w.failDispatch(task.ID, 400, "model_rejected", "model rejected the request before admission", false)
			case 401, 403:
				return w.failDispatch(task.ID, 502, "model_authentication_failed", "model rejected the gateway credential", false)
			}
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return w.failDispatch(task.ID, 503, "backend_unreachable", "model connection was not established", false)
		}
		// 发出之后的超时与断线无法证明模型已释放 GPU，改用原 ID 查询。
		if errors.Is(err, context.DeadlineExceeded) {
			return w.failDispatch(task.ID, 504, "backend_timeout", "model execution outcome is unknown", true)
		}
		return w.failDispatch(task.ID, 502, "backend_outcome_unknown", "model execution outcome is unknown", true)
	}
	return w.recordModelResponse(cfg, task, response, false)
}

func (w *Reconciler) failDispatch(id string, status int, code, message string, hold bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return w.store.DispatchFailure(ctx, id, status, code, message, hold)
}

func (w *Reconciler) recordModelResponse(cfg *config.Snapshot, task *ent.Request, raw []byte, polling bool) error {
	var response protocol.ModelResponse
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := protocol.DecodeJSON(raw, &response); err != nil || response.Validate(task.ID) != nil {
		if polling {
			return w.store.PollUnavailable(ctx, task.ID, task.PollClaim, cfg.PollInterval)
		}
		return w.store.DispatchFailure(ctx, task.ID, 502, "invalid_model_response", "model execution outcome is unknown", true)
	}
	return w.store.RecordResponse(ctx, task.ID, task.PollClaim, response, cfg.PollInterval)
}

func (w *Reconciler) pollModel(cfg *config.Snapshot, task *ent.Request) error {
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	defer cancel()
	raw, err := w.modelCall(ctx, cfg, http.MethodGet, task.Endpoint, "/requests/"+task.ID, nil)
	if err == nil {
		return w.recordModelResponse(cfg, task, raw, true)
	}
	writeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	return w.store.PollUnavailable(writeCtx, task.ID, task.PollClaim, cfg.PollInterval)
}

func (w *Reconciler) probe(cfg *config.Snapshot) error {
	rows, err := w.store.ProbeInstances(w.ctx)
	if err != nil {
		return err
	}
	// 同轮并行探测，单个模型超时不会阻断其他资源组的健康更新。
	var probes sync.WaitGroup
	for _, row := range rows {
		probes.Go(func() {
			checked := time.Now().UTC()
			ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
			defer cancel()
			raw, callErr := w.modelCall(ctx, cfg, http.MethodGet, row.HealthURL, "", nil)
			var health struct {
				Ready bool `json:"ready"`
			}
			ready := callErr == nil && json.Unmarshal(raw, &health) == nil && health.Ready
			if err := w.store.RecordProbe(w.ctx, row.ID, checked, ready); err != nil && w.ctx.Err() == nil {
				slog.Error("model health update failed", "error_type", fmt.Sprintf("%T", err))
			}
		})
	}
	probes.Wait()
	return nil
}

func (w *Reconciler) runWorker(label string, interval time.Duration, work func() error) {
	w.wg.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for w.ctx.Err() == nil {
			if err := work(); err != nil && w.ctx.Err() == nil {
				slog.Error(label, "error_type", fmt.Sprintf("%T", err))
			}
			select {
			case <-w.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// 派发、终态查询和排队过期分开，长视频不能占满等待队列的消费协程。
func (w *Reconciler) Start() {
	w.runWorker("model health probe failed", probeInterval, func() error { return w.probe(w.runtime.Current()) })
	w.runWorker("queue expiry failed", 200*time.Millisecond, func() error { return w.store.Expire(w.ctx) })
	for range 4 {
		w.runWorker("queue dispatch failed", 80*time.Millisecond, func() error {
			task, err := w.store.Claim(w.ctx)
			if err != nil || task == nil {
				return err
			}
			return w.execute(w.runtime.Current(), task)
		})
		w.runWorker("model reconciliation failed", 125*time.Millisecond, func() error {
			task, err := w.store.ClaimPoll(w.ctx)
			if err != nil || task == nil {
				return err
			}
			return w.pollModel(w.runtime.Current(), task)
		})
	}
}
