package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// Generation backends implement the same two JSON contracts as the public API.
// No model name, business task or model-specific parameter is interpreted here.
func generationRequest(task *ent.Request, input protocol.Submission) (string, []byte, error) {
	if err := protocol.ValidateGenerationInput(task.Protocol, input.Input); err != nil {
		return "", nil, err
	}
	switch task.Protocol {
	case protocol.ImageGenerationProtocol:
		var body protocol.ImageGenerationRequest
		if err := json.Unmarshal(input.Input, &body.ImageGenerationInput); err != nil {
			return "", nil, err
		}
		body.Model, body.User = task.ModelKey, input.Metadata.UserID
		if body.ResponseFormat == "" {
			body.ResponseFormat = "b64_json"
		}
		raw, err := json.Marshal(body)
		return "/v1/images/generations", raw, err
	case protocol.VideoGenerationProtocol:
		var body protocol.VideoGenerationRequest
		if err := json.Unmarshal(input.Input, &body.VideoGenerationInput); err != nil {
			return "", nil, err
		}
		body.Model, body.User = task.ModelKey, input.Metadata.UserID
		raw, err := json.Marshal(body)
		return "/v1/videos", raw, err
	default:
		return "", nil, errors.New("unsupported generation protocol")
	}
}

func (w *Reconciler) executeGeneration(ctx context.Context, cfg *config.Snapshot, task *ent.Request, input protocol.Submission) error {
	if w.results == nil {
		return w.failDispatch(task, 503, "result_storage_unavailable", "generation result storage is not configured", false)
	}
	path, raw, err := generationRequest(task, input)
	if err != nil {
		return w.failDispatch(task, 500, "invalid_saved_request", "saved generation input is invalid", false)
	}
	if !time.Now().Before(task.DeadlineAt) || ctx.Err() != nil {
		return w.failDispatch(task, 504, "queue_timeout", "request expired before dispatch", false)
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	// Every retry reuses the same key. Backends must deduplicate it, including
	// reconnects after a generation completed but its response was lost.
	headers.Set("Idempotency-Key", task.ID)
	headers.Set("X-Gateway-Request-ID", task.ID)
	headers.Set("X-Gateway-Dispatch", "immediate")
	if input.Metadata.TraceID != "" {
		headers.Set("X-Trace-ID", input.Metadata.TraceID)
	}
	if input.Metadata.UserID != "" {
		headers.Set("X-User-ID", input.Metadata.UserID)
	}
	resp, err := w.nativeHTTP(ctx, nativeAuth{bearer: cfg.BackendToken}, http.MethodPost, task.Endpoint+path, bytes.NewReader(raw), headers)
	if err != nil {
		var status modelStatusError
		if errors.Is(err, errNativeRouteUnavailable) {
			return w.failDispatch(task, 502, "generation_route_unavailable", "model generation route is unavailable", false)
		}
		if errors.As(err, &status) && status == 422 {
			return w.failDispatch(task, 400, "model_rejected", "model rejected generation parameters", false)
		}
		return w.dispatchError(task, err)
	}
	limit := int64(nativeImageLimit)
	if task.Protocol == protocol.VideoGenerationProtocol {
		limit = workerResponseLimit
	}
	raw, err = readModelBody(resp.Body, limit)
	_ = resp.Body.Close()
	if err != nil {
		return w.dispatchError(task, err)
	}
	if task.Protocol == protocol.VideoGenerationProtocol {
		var job generationVideoJob
		if json.Unmarshal(raw, &job) != nil || !nativeJobID.MatchString(job.ID) || !job.validStatus() {
			return w.requeueDispatch(task)
		}
		if job.Status == "failed" {
			return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "model_failed", "video generation failed"), false)
		}
		return w.saveModelResponse(cfg, task, protocol.ModelResponse{
			Response: protocol.Response{RequestID: task.ID, State: "processing"}, BackendTaskID: job.ID,
		}, false)
	}
	result, err := w.results.publishImages(ctx, task.ID, raw)
	if err != nil {
		return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "result_unavailable", "cannot publish image result"), false)
	}
	response := nativeSuccess(task.ID, result.Files)
	response.Result.Generation = result.Generation
	return w.saveModelResponse(cfg, task, response, false)
}

type generationVideoJob struct {
	ID         string               `json:"id"`
	Status     string               `json:"status"`
	Generation *protocol.Generation `json:"generation,omitempty"`
}

func (j generationVideoJob) validStatus() bool {
	switch j.Status {
	case "queued", "in_progress", "completed", "failed":
		return true
	default:
		return false
	}
}

func (w *Reconciler) pollGeneration(cfg *config.Snapshot, task *ent.Request) error {
	if task.Protocol != protocol.VideoGenerationProtocol || task.BackendTaskID == "" {
		if task.State == "failed" {
			return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "backend_outcome_unknown", "generation ended without a saved result"), true)
		}
		return w.requeueDispatch(task)
	}
	if !nativeJobID.MatchString(task.BackendTaskID) {
		return w.pollUnavailable(cfg, task)
	}
	jobURL := task.Endpoint + "/v1/videos/" + task.BackendTaskID
	auth := nativeAuth{bearer: cfg.BackendToken}
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	var job generationVideoJob
	err := w.nativeJSON(ctx, auth, jobURL, &job)
	cancel()
	if err != nil || job.ID != task.BackendTaskID || !job.validStatus() {
		return w.pollUnavailable(cfg, task)
	}
	switch job.Status {
	case "completed":
		select {
		case w.videoPublications <- func() error { return w.completeNativeVideo(cfg, task, auth, jobURL+"/content", job.Generation) }:
			return nil
		default:
		}
	case "failed":
		return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "model_failed", "video generation failed"), true)
	}
	return w.saveModelResponse(cfg, task, protocol.ModelResponse{Response: protocol.Response{RequestID: task.ID, State: "processing"}}, true)
}
