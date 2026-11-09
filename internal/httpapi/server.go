package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

// Server 固定每次入站请求的配置快照，让鉴权、地址校验和受理使用同一份配置。
type Server struct {
	store   *store.Store
	runtime *config.Runtime
	uploads chan struct{}
}

func New(tasks *store.Store, runtime *config.Runtime) *Server {
	return &Server{store: tasks, runtime: runtime, uploads: make(chan struct{}, 2)}
}

type runtimeKey struct{}

func requestConfig(ctx context.Context) *config.Snapshot {
	return ctx.Value(runtimeKey{}).(*config.Snapshot)
}

func (s *Server) HTTPServer(listener net.Listener) *server.Hertz {
	// 图片接口可能等待完整生成预算。等待由任务截止时间控制，不能被监听器的短写超时截断。
	h := server.Default(server.WithListener(listener), server.WithReadTimeout(30*time.Second),
		server.WithIdleTimeout(60*time.Second), server.WithExitWaitTime(30*time.Second),
		server.WithHandleMethodNotAllowed(true), server.WithRedirectTrailingSlash(false), server.WithStreamBody(true),
		server.WithMaxRequestBodySize(4096), server.WithDisablePreParseMultipartForm(true), server.WithSenseClientDisconnection(true))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Next(context.WithValue(ctx, runtimeKey{}, s.runtime.Current()))
	})
	h.GET("/_gateway/live", func(_ context.Context, c *app.RequestContext) { writeJSON(c, 200, map[string]bool{"live": true}) })
	h.GET("/_gateway/ready", s.ready)
	// 实例登记决定网关可以向哪些执行地址派单，因此使用管理凭据。
	h.POST("/v1/instances/register", s.auth("admin", s.register))
	h.POST("/v1/instances/:id/drain", s.auth("admin", s.drain))
	h.GET("/v1/instances", s.auth("admin", s.instances))
	h.DELETE("/v1/instances/:id", s.auth("admin", s.clearInstance))
	h.GET("/v1/admin/requests", s.auth("admin", s.listRequests))
	h.GET("/v1/admin/requests/:id", s.auth("admin", s.requestDetail))
	h.POST("/v1/gateway/requests/cancel", s.auth("api", s.cancelRequest))
	h.POST("/v1/gateway/requests/:model_key", s.auth("api", s.submit))
	h.GET("/v1/gateway/requests", s.auth("api", s.get))
	return h
}

func (s *Server) auth(role string, next app.HandlerFunc) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		cfg := requestConfig(ctx)
		header := c.GetHeader("Authorization")
		match := func(token string) bool {
			return subtle.ConstantTimeCompare(header, []byte("Bearer "+token)) == 1
		}
		allowed := false
		switch role {
		case "admin":
			allowed = match(cfg.AdminToken)
		case "api":
			allowed = match(cfg.APIToken)
		}
		if !allowed {
			writeError(c, 401, "unauthorized", "invalid gateway credential", "")
			return
		}
		next(ctx, c)
	}
}

func writeJSON(c *app.RequestContext, status int, value any) {
	c.Header("Content-Type", "application/json")
	c.SetStatusCode(status)
	_ = json.NewEncoder(c).Encode(value)
}

var errorTypes = map[int]string{
	400: "invalid_request_error", 401: "authentication_error", 404: "not_found_error",
	409: "conflict_error", 413: "request_too_large", 415: "invalid_request_error",
	429: "rate_limit_error", 500: "api_error", 502: "upstream_error",
	503: "service_unavailable_error", 504: "timeout_error",
}

func writeError(c *app.RequestContext, status int, code, message, id string) {
	if status == 429 || status == 503 {
		c.Header("Retry-After", "2")
	}
	writeJSON(c, status, map[string]any{
		"error":      map[string]any{"type": errorTypes[status], "code": code, "message": message, "param": nil},
		"request_id": id,
	})
}

func storeError(c *app.RequestContext, err error, id string) {
	switch {
	case ent.IsNotFound(err):
		writeError(c, 404, "not_found", "record does not exist", id)
	case errors.Is(err, store.ErrConflict):
		writeError(c, 409, "conflict", "request or instance conflicts with an existing record", id)
	case errors.Is(err, store.ErrLegacyRegistration):
		writeError(c, 409, "legacy_registration", "register a new instance with the current model key", id)
	case errors.Is(err, store.ErrInstanceNotClearable):
		writeError(c, 409, "instance_not_clearable", err.Error(), id)
	case errors.Is(err, store.ErrFull):
		writeError(c, 429, "queue_full", "waiting queue is full", id)
	default:
		slog.Error("database operation failed", "error_type", fmt.Sprintf("%T", err))
		writeError(c, 503, "database_unavailable", "task storage is temporarily unavailable", id)
	}
}

func readBody(c *app.RequestContext, limit int64, value any, id string) ([]byte, bool) {
	mimeType, _, _ := mime.ParseMediaType(string(c.GetHeader("Content-Type")))
	if mimeType != "application/json" {
		c.SetConnectionClose()
		writeError(c, 415, "unsupported_media_type", "request must use application/json", id)
		return nil, false
	}
	reader := c.Request.BodyStream()
	if reader == nil {
		reader = bytes.NewReader(c.Request.Body())
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if int64(len(raw)) > limit {
		c.SetConnectionClose()
		writeError(c, 413, "request_too_large", "request exceeds its size limit", id)
		return nil, false
	}
	if err != nil || protocol.DecodeJSON(raw, value) != nil {
		writeError(c, 400, "invalid_json", "invalid JSON request body", id)
		return nil, false
	}
	return raw, true
}

func (s *Server) ready(ctx context.Context, c *app.RequestContext) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.store.CheckReady(ctx); err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]bool{"ready": true})
}

func (s *Server) register(ctx context.Context, c *app.RequestContext) {
	cfg := requestConfig(ctx)
	var spec protocol.Instance
	if _, ok := readBody(c, 16<<10, &spec, ""); !ok {
		return
	}
	spec.Endpoint = strings.TrimRight(spec.Endpoint, "/")
	if !spec.Valid() || cfg.ValidateEndpoint(spec.Endpoint, false) != nil || cfg.ValidateEndpoint(spec.HealthURL, true) != nil {
		writeError(c, 400, "invalid_instance", "invalid model instance specification", "")
		return
	}
	if !cfg.AcceptsInstanceAddress(spec.Capability, spec.Endpoint, spec.HealthURL, spec.TargetInstance) {
		writeError(c, 400, "invalid_instance", "backend capability, endpoint or health URL does not match", "")
		return
	}
	if err := s.store.Register(ctx, spec); err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]string{"instance_id": spec.ID})
}

func (s *Server) drain(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Draining *bool `json:"draining"`
	}
	if _, ok := readBody(c, 1024, &body, ""); !ok {
		return
	}
	if body.Draining == nil {
		writeError(c, 400, "invalid_instance", "draining is required", "")
		return
	}
	if err := s.store.Drain(ctx, c.Param("id"), *body.Draining); err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]bool{"draining": *body.Draining})
}

func (s *Server) clearInstance(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if !protocol.ValidInstanceID(id) {
		writeError(c, 400, "invalid_instance", "invalid instance ID", "")
		return
	}
	if err := s.store.ClearInstance(ctx, id); err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]bool{"deleted": true})
}

func (s *Server) instances(ctx context.Context, c *app.RequestContext) {
	result, err := s.store.ListInstances(ctx)
	if err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, result)
}

func requestID(c *app.RequestContext) (string, bool) {
	id := string(c.GetHeader("Idempotency-Key"))
	if !protocol.ValidID(id) || len(c.Request.URI().QueryString()) != 0 || len(c.GetHeader("X-Model-Version")) != 0 || len(c.GetHeader("X-Routing-Key")) != 0 {
		writeError(c, 400, "invalid_request", "supply a UUID Idempotency-Key without extra routing selectors", "")
		return "", false
	}
	c.Header("X-Gateway-Request-ID", id)
	return id, true
}

func (s *Server) submit(ctx context.Context, c *app.RequestContext) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	model := c.Param("model_key")
	if protocol.Capability(model) == "" {
		writeError(c, 404, "model_not_found", "model is not configured", id)
		return
	}
	// 同时解码的大图受限，连接等待响应时不占上传名额。
	select {
	case s.uploads <- struct{}{}:
	case <-ctx.Done():
		return
	}
	cfg := requestConfig(ctx)
	var input protocol.Submission
	raw, ok := readBody(c, cfg.MaxBodyBytes, &input, id)
	if !ok {
		<-s.uploads
		return
	}
	capability, err := protocol.Route(model, input.Input)
	for _, value := range []string{input.Metadata.TraceID, input.Metadata.UserID} {
		if len(value) > 256 || strings.ContainsFunc(value, func(r rune) bool { return r < 32 || r == 127 }) {
			err = errors.New("invalid metadata")
		}
	}
	if err != nil {
		<-s.uploads
		writeError(c, 400, "invalid_input", err.Error(), id)
		return
	}
	task, err := s.store.Admit(ctx, store.Admission{
		ID: id, ModelKey: model, Capability: capability, Payload: raw, Input: input.Input,
		TraceID: input.Metadata.TraceID, Units: max(1, cfg.UnitsByCapability[capability]),
		MaxWaiting: cfg.MaxWaiting, BackendTimeout: cfg.BackendTimeout,
	})
	<-s.uploads
	if err != nil {
		storeError(c, err, id)
		return
	}
	// 视频调用方通过查询接口取终态，提交请求无需保持连接。
	if capability == "video" {
		writeJSON(c, 202, protocol.Response{RequestID: id, ModelKey: model, State: string(task.State)})
		return
	}
	// 响应等待不持有原始上传，也不取消已经提交到 PG 的生成。
	s.waitImage(ctx, c, id)
}

func taskResponse(task *ent.Request) protocol.Response {
	response := protocol.Response{RequestID: task.ID, ModelKey: task.ModelKey, State: string(task.State)}
	if task.ErrorCode != "" {
		response.Error = &protocol.RequestError{Code: task.ErrorCode, Message: task.ErrorMessage}
	}
	if len(task.Result) != 0 {
		response.Result = &protocol.Result{}
		_ = json.Unmarshal(task.Result, response.Result)
	}
	return response
}

func (s *Server) waitImage(ctx context.Context, c *app.RequestContext, id string) {
	ticker := time.NewTicker(45 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := s.store.Get(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				storeError(c, err, id)
			}
			return
		}
		c.Header("X-Model-Key", task.ModelKey)
		if task.InstanceID != "" {
			c.Header("X-Model-Instance", task.InstanceID)
		}
		switch task.State {
		case request.StateSucceeded:
			writeJSON(c, 200, taskResponse(task))
			return
		case request.StateFailed:
			writeError(c, task.ErrorStatus, task.ErrorCode, task.ErrorMessage, id)
			return
		}
		if !time.Now().Before(task.DeadlineAt) {
			writeError(c, 504, "response_wait_timeout", "response wait timed out; query the same request ID", id)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) get(ctx context.Context, c *app.RequestContext) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	task, err := s.store.Get(ctx, id)
	if err != nil {
		storeError(c, err, id)
		return
	}
	writeJSON(c, 200, taskResponse(task))
}

func (s *Server) cancelRequest(ctx context.Context, c *app.RequestContext) {
	id, ok := requestID(c)
	if !ok {
		return
	}
	if err := s.store.Cancel(ctx, id); err != nil {
		storeError(c, err, id)
		return
	}
	writeJSON(c, 200, protocol.Response{RequestID: id, State: "failed"})
}
