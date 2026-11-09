package gateway

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
	"github.com/wgdl666/modelGateway/ent/modelinstance"
)

const workerResponseLimit = 1 << 20

type modelStatusError int

func (status modelStatusError) Error() string { return fmt.Sprintf("model HTTP %d", status) }

func (s *Server) modelCall(ctx context.Context, cfg *runtimeConfig, method, origin, path string, body []byte) ([]byte, error) {
	if err := validateEndpoint(origin, path == "", cfg.networks, cfg.ports); err != nil {
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
	resp, err := s.client.Do(req)
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

func (s *Server) execute(cfg *runtimeConfig, task *ent.Request) error {
	var payload Submission
	if err := json.Unmarshal(task.RequestPayload, &payload); err != nil {
		return s.failDispatch(task.ID, 500, "invalid_saved_request", "saved request is invalid", false)
	}
	raw, err := json.Marshal(ModelRequest{RequestID: task.ID, ModelKey: task.ModelKey, Capability: task.Capability, Submission: payload})
	if err != nil {
		return s.failDispatch(task.ID, 500, "invalid_saved_request", "saved request is invalid", false)
	}
	if !time.Now().Before(task.DeadlineAt) {
		return s.failDispatch(task.ID, 504, "queue_timeout", "request expired before dispatch", false)
	}
	if validateEndpoint(task.Endpoint, false, cfg.networks, cfg.ports) != nil {
		return s.failDispatch(task.ID, 503, "backend_unavailable", "selected model address is unavailable", false)
	}
	ctx, cancel := context.WithDeadline(s.ctx, task.DeadlineAt)
	defer cancel()
	response, err := s.modelCall(ctx, cfg, http.MethodPost, task.Endpoint, "/requests", raw)
	if err != nil {
		var status modelStatusError
		if errors.As(err, &status) {
			// 执行端约定这些错误在落受理记录前返回，可以确认 GPU 尚未接单。
			switch status {
			case 400, 413, 415:
				return s.failDispatch(task.ID, 400, "model_rejected", "model rejected the request before admission", false)
			case 401, 403:
				return s.failDispatch(task.ID, 502, "model_authentication_failed", "model rejected the gateway credential", false)
			}
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return s.failDispatch(task.ID, 503, "backend_unreachable", "model connection was not established", false)
		}
		// 发出之后的超时与断线无法证明模型已释放 GPU，改用原 ID 查询。
		if errors.Is(err, context.DeadlineExceeded) {
			return s.failDispatch(task.ID, 504, "backend_timeout", "model execution outcome is unknown", true)
		}
		return s.failDispatch(task.ID, 502, "backend_outcome_unknown", "model execution outcome is unknown", true)
	}
	return s.recordModelResponse(cfg, task, response, false)
}

func (s *Server) failDispatch(id string, status int, code, message string, hold bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.store.DispatchFailure(ctx, id, status, code, message, hold)
}

func (s *Server) recordModelResponse(cfg *runtimeConfig, task *ent.Request, raw []byte, polling bool) error {
	var response ModelResponse
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := decodeJSON(raw, &response); err != nil || response.validate(task.ID) != nil {
		if polling {
			return s.store.PollUnavailable(ctx, task.ID, task.PollClaim, cfg.PollInterval)
		}
		return s.store.DispatchFailure(ctx, task.ID, 502, "invalid_model_response", "model execution outcome is unknown", true)
	}
	return s.store.RecordResponse(ctx, task.ID, task.PollClaim, response, cfg.PollInterval)
}

func (s *Server) pollModel(cfg *runtimeConfig, task *ent.Request) error {
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	raw, err := s.modelCall(ctx, cfg, http.MethodGet, task.Endpoint, "/requests/"+task.ID, nil)
	if err == nil {
		return s.recordModelResponse(cfg, task, raw, true)
	}
	writeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	return s.store.PollUnavailable(writeCtx, task.ID, task.PollClaim, cfg.PollInterval)
}

func (s *Server) probe(cfg *runtimeConfig) error {
	rows, err := s.store.DB.ModelInstance.Query().Where(modelinstance.DrainingEQ(false)).All(s.ctx)
	if err != nil {
		return err
	}
	// 同轮并行探测，单个模型超时不会阻断其他资源组的健康更新。
	var probes sync.WaitGroup
	for _, row := range rows {
		probes.Go(func() {
			checked := time.Now().UTC()
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			defer cancel()
			raw, callErr := s.modelCall(ctx, cfg, http.MethodGet, row.HealthURL, "", nil)
			var health struct {
				Ready bool `json:"ready"`
			}
			ready := callErr == nil && json.Unmarshal(raw, &health) == nil && health.Ready
			if err := s.store.RecordProbe(s.ctx, row.ID, checked, ready); err != nil && s.ctx.Err() == nil {
				slog.Error("model health update failed", "error_type", fmt.Sprintf("%T", err))
			}
		})
	}
	probes.Wait()
	return nil
}

func (s *Server) runWorker(label string, interval time.Duration, work func() error) {
	s.wg.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for s.ctx.Err() == nil {
			if err := work(); err != nil && s.ctx.Err() == nil {
				slog.Error(label, "error_type", fmt.Sprintf("%T", err))
			}
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// 派发、终态查询和排队过期分开，长视频不能占满等待队列的消费协程。
func (s *Server) StartReconciler() {
	s.runWorker("model health probe failed", probeInterval, func() error { return s.probe(s.config()) })
	s.runWorker("queue expiry failed", 200*time.Millisecond, func() error { return s.store.Expire(s.ctx) })
	for range 4 {
		s.runWorker("queue dispatch failed", 80*time.Millisecond, func() error {
			task, err := s.store.Claim(s.ctx)
			if err != nil || task == nil {
				return err
			}
			return s.execute(s.config(), task)
		})
		s.runWorker("model reconciliation failed", 125*time.Millisecond, func() error {
			task, err := s.store.ClaimPoll(s.ctx)
			if err != nil || task == nil {
				return err
			}
			return s.pollModel(s.config(), task)
		})
	}
}
