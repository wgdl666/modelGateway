package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

// 同步协议仍由原连接返回；受理、暂停与容量和异步任务使用同一份持久账本。
func (s *Server) forwardSynchronous(ctx context.Context, c *app.RequestContext, client *http.Client, model, path, contentType string, body []byte) {
	cfg := requestConfig(ctx)
	id := string(c.GetHeader("Idempotency-Key"))
	if id == "" {
		id = uuid.NewString()
	} else if !protocol.ValidID(id) {
		writeError(c, 400, "invalid_request_id", "Idempotency-Key must be a UUID", "")
		return
	}
	// 原图/文本只由当前连接持有，不在只用于追溯的同步记录里再存一份敏感正文。
	digest := sha256.Sum256(body)
	input, _ := json.Marshal(map[string]any{"content_type": contentType, "body_bytes": len(body), "body_sha256": hex.EncodeToString(digest[:])})
	payload, _ := json.Marshal(protocol.Submission{Input: input})
	units := cfg.UnitsByModelKey[model]
	if units == 0 {
		units = 1
	}
	task, err := s.store.Admit(ctx, store.Admission{ID: id, ModelKey: model, TraceID: string(c.GetHeader("X-Trace-ID")),
		Payload: payload, Input: input, Units: units, MaxWaiting: cfg.MaxWaiting, BackendTimeout: cfg.BackendTimeout})
	if err != nil {
		storeError(c, err, id)
		return
	}
	c.Header("X-Request-ID", id)
	waitCtx, stopWaiting := context.WithDeadline(ctx, task.DeadlineAt)
	defer stopWaiting()
	for {
		task, err = s.store.Claim(waitCtx, cfg.Config, id)
		if err != nil || waitCtx.Err() != nil {
			// 取消只影响尚未派发的请求。领取若恰好已提交，仍必须执行并收敛其占用。
			if err == nil && task != nil {
				break
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), store.ExecutionWriteTimeout)
			_ = s.store.Cancel(cleanupCtx, id)
			cancel()
			if waitCtx.Err() != nil {
				writeError(c, 504, "queue_timeout", "request ended before an instance became available", id)
			} else {
				storeError(c, err, id)
			}
			return
		}
		if task != nil {
			break
		}
		current, getErr := s.store.Get(waitCtx, id)
		if getErr != nil {
			storeError(c, getErr, id)
			return
		}
		if current.State != request.StateQueued {
			writeError(c, 409, "request_not_queued", "request is no longer waiting for dispatch", id)
			return
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.executeSynchronous(ctx, c, client, task, path, contentType, body)
}

func (s *Server) executeSynchronous(ctx context.Context, c *app.RequestContext, client *http.Client, task *ent.Request, path, contentType string, body []byte) {
	cfg := requestConfig(ctx)
	endpoint := task.Endpoint + path
	port := strings.TrimPrefix(task.Endpoint, "http://127.0.0.1:")
	switch protocol.SynchronousKind(task.ModelKey) {
	case protocol.SyncVision:
		endpoint = "http://" + cfg.VisionAddr + "/_dispatch/" + port + path
	case protocol.SyncDecision:
		endpoint = "http://" + cfg.DecisionAddr + "/v1/instances/" + port + "/predict"
	}
	// 客户端断开不代表 GPU 已停止。独立执行预算保证继续读完响应后才归还容量。
	execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.NativeExecutionTimeout)
	var wrote atomic.Bool
	execCtx = httptrace.WithClientTrace(execCtx, &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	complete := func(result protocol.HTTPResult, known bool) {
		writeCtx, end := context.WithTimeout(context.Background(), store.ExecutionWriteTimeout)
		defer end()
		if err := s.store.CompleteSynchronous(writeCtx, task.ID, task.PollClaim, result, known); err != nil {
			slog.Error("synchronous outcome persistence failed", "request_id", task.ID, "error", err)
		}
		cancel()
	}
	req, err := http.NewRequestWithContext(execCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		complete(protocol.HTTPResult{StatusCode: 502}, true)
		writeError(c, 502, "upstream_unavailable", "instance endpoint is invalid", task.ID)
		return
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Request-ID", task.ID)
	resp, err := client.Do(req)
	if err != nil {
		status := 502
		if errors.Is(err, context.DeadlineExceeded) {
			status = 504
		}
		// 没发出请求的连接失败可以释放；发出后无完整响应必须保留占用待确认。
		complete(protocol.HTTPResult{StatusCode: status}, !wrote.Load())
		writeError(c, status, "upstream_unavailable", "instance response could not be completed", task.ID)
		return
	}
	c.Header("X-Model-Instance", task.InstanceID)
	c.Header("Content-Type", resp.Header.Get("Content-Type"))
	c.SetStatusCode(resp.StatusCode)
	tracked := &synchronousBody{ReadCloser: resp.Body, complete: complete,
		result:  protocol.HTTPResult{StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")},
		unknown: resp.Header.Get("X-Model-Outcome") == "unknown"}
	c.Response.SetBodyStream(tracked, int(resp.ContentLength))
}

// Hertz 在断开或读满 Content-Length 后都会 Close；后台读至 EOF 保持流式协议和真实执行生命周期一致。
type synchronousBody struct {
	io.ReadCloser
	result    protocol.HTTPResult
	unknown   bool
	complete  func(protocol.HTTPResult, bool)
	finish    sync.Once
	closeOnce sync.Once
}

func (b *synchronousBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.result.ResponseBytes += int64(n)
	if err != nil {
		b.finish.Do(func() {
			b.complete(b.result, err == io.EOF && !b.unknown)
			_ = b.ReadCloser.Close()
		})
	}
	return n, err
}

func (b *synchronousBody) Close() error {
	b.closeOnce.Do(func() {
		go func() { _, _ = io.Copy(io.Discard, b) }()
	})
	return nil
}
