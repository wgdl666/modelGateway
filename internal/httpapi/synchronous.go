package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
	"github.com/wgdl666/modelGateway/internal/telemetry"
	"go.opentelemetry.io/otel/propagation"
)

// 同步协议仍由原连接返回；受理、暂停与容量和异步任务使用同一份持久账本。
func (s *Server) forwardSynchronous(ctx context.Context, c *app.RequestContext, client *http.Client, model, kind, path, contentType string, body []byte) {
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
	task, err := s.store.Admit(ctx, store.Admission{ID: id, ModelKey: model, Protocol: kind, TraceID: synchronousTraceID(c), APITokenID: c.GetString(apiTokenID),
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

// ModelHub 的 HTTP 客户端发送 W3C traceparent；沿用显式 X-Trace-ID，缺省时从标准头提取，
// 否则同步请求虽已完成，Ops 仍无法从模型调用的 Trace 关联到实例和占用记录。
func synchronousTraceID(c *app.RequestContext) string {
	if explicit := string(c.GetHeader("X-Trace-ID")); explicit != "" {
		return explicit
	}
	parts := strings.Split(string(c.GetHeader("traceparent")), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return ""
	}
	// 不把截断、全零或非法的上下文记成有效关联；当前调用链使用 W3C 00 版。
	for _, value := range parts[1:] {
		if _, err := hex.DecodeString(value); err != nil || value != strings.ToLower(value) {
			return ""
		}
	}
	if strings.Trim(parts[1], "0") == "" || strings.Trim(parts[2], "0") == "" {
		return ""
	}
	return parts[1]
}

func (s *Server) executeSynchronous(ctx context.Context, c *app.RequestContext, client *http.Client, task *ent.Request, path, contentType string, body []byte) {
	cfg := requestConfig(ctx)
	endpoint, pool := synchronousEndpoint(cfg.Config, task, path)
	// 客户端断开不代表 GPU 已停止。独立执行预算保证继续读完响应后才归还容量。
	execCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.NativeExecutionTimeout)
	if pool {
		execCtx = config.WithSubjectPoolPolicy(execCtx, cfg)
	}
	var wrote atomic.Bool
	execCtx = httptrace.WithClientTrace(execCtx, &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }})
	complete := func(result protocol.HTTPResult, known bool) {
		writeCtx, end := context.WithTimeout(context.Background(), store.ExecutionWriteTimeout)
		defer end()
		if err := s.store.CompleteSynchronous(writeCtx, task.ID, task.PollClaim, result, known); err != nil {
			telemetry.LogError(execCtx, "synchronous outcome persistence failed", err, "request_id", task.ID)
		}
		cancel()
	}
	req, err := http.NewRequestWithContext(execCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		complete(protocol.HTTPResult{StatusCode: 502}, true)
		writeError(c, 502, "upstream_unavailable", "instance endpoint is invalid", task.ID)
		return
	}
	if pool {
		req.Header.Set("X-Segment-Pool-Key", cfg.SubjectPoolAPIKey)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Request-ID", task.ID)
	// 网关 span 接在 ModelHub 后面；标准上下文与已有业务 Trace ID 一起传给同步执行端。
	(propagation.TraceContext{}).Inject(execCtx, propagation.HeaderCarrier(req.Header))
	if task.TraceID != "" {
		req.Header.Set("X-Trace-ID", task.TraceID)
	}
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
	if stream, convert := c.Get(decisionChatStreamKey); convert && task.Protocol == protocol.DecisionProtocol && resp.StatusCode == http.StatusOK {
		// 从同一个受跟踪响应读取分类结果，不能因转换响应而提前释放执行占用。
		writeDecisionChat(c, tracked, task.ID, task.ModelKey, stream == true)
		return
	}
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

func synchronousEndpoint(cfg config.Config, task *ent.Request, path string) (string, bool) {
	if _, ok := cfg.SubjectPoolFor(task.ModelKey, task.Protocol, task.Endpoint); ok {
		return task.Endpoint + "/api/segment_subject", true
	}
	endpoint := task.Endpoint + path
	port := strings.TrimPrefix(task.Endpoint, "http://127.0.0.1:")
	switch protocol.SynchronousKind(task.Protocol) {
	case protocol.SyncVision:
		endpoint = "http://" + cfg.VisionAddr + "/_dispatch/" + port + path
	case protocol.SyncDecision:
		endpoint = "http://" + cfg.DecisionAddr + "/v1/instances/" + port + "/predict"
	}
	return endpoint, false
}
