package gateway

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
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
)

type Server struct {
	mu      sync.RWMutex
	current *runtimeConfig
	store   *Store
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	uploads chan struct{}
}

func NewServer(store *Store, c Config) (*Server, error) {
	state, err := prepareConfig(c)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Server{current: state, store: store, ctx: ctx, cancel: cancel, client: client, uploads: make(chan struct{}, 2)}, nil
}

func (s *Server) HTTPServer(listener net.Listener) *server.Hertz {
	// 图片接口可能等待完整生成预算。等待由任务截止时间控制，不能被监听器的短写超时截断。
	h := server.Default(server.WithListener(listener), server.WithReadTimeout(30*time.Second),
		server.WithIdleTimeout(60*time.Second), server.WithExitWaitTime(30*time.Second),
		server.WithHandleMethodNotAllowed(true), server.WithRedirectTrailingSlash(false), server.WithStreamBody(true),
		server.WithMaxRequestBodySize(4096), server.WithDisablePreParseMultipartForm(true), server.WithSenseClientDisconnection(true))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		c.Next(context.WithValue(ctx, runtimeKey{}, s.config()))
	})
	h.GET("/_gateway/live", func(_ context.Context, c *app.RequestContext) { writeJSON(c, 200, map[string]bool{"live": true}) })
	h.GET("/_gateway/ready", s.ready)
	h.POST("/v1/instances/register", s.auth("worker", s.register))
	h.POST("/v1/instances/:id/drain", s.auth("admin", s.drain))
	h.GET("/v1/instances", s.auth("admin", s.instances))
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
		case "worker":
			allowed = match(cfg.WorkerToken) || match(cfg.AdminToken)
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
	case errors.Is(err, ErrConflict):
		writeError(c, 409, "conflict", "request or instance conflicts with an existing record", id)
	case errors.Is(err, ErrBusy):
		writeError(c, 503, "no_healthy_instance", "no healthy model instance is available", id)
	case errors.Is(err, ErrFull):
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
	if err != nil || decodeJSON(raw, value) != nil {
		writeError(c, 400, "invalid_json", "invalid JSON request body", id)
		return nil, false
	}
	return raw, true
}

func (s *Server) ready(ctx context.Context, c *app.RequestContext) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := s.store.DB.ModelInstance.Query().Exist(ctx); err != nil {
		storeError(c, err, "")
		return
	}
	if _, err := s.store.DB.Request.Query().Exist(ctx); err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]bool{"ready": true})
}

func (s *Server) register(ctx context.Context, c *app.RequestContext) {
	cfg := requestConfig(ctx)
	var spec Instance
	if _, ok := readBody(c, 16<<10, &spec, ""); !ok {
		return
	}
	spec.Endpoint = strings.TrimRight(spec.Endpoint, "/")
	if !instanceID.MatchString(spec.ID) || !slices.Contains(capabilities(spec.ModelKey), spec.Capability) ||
		spec.ResourceGroup == "" || len(spec.ResourceGroup) > 256 || !validArtifact(spec.Artifact) ||
		spec.Capacity < 1 || spec.Capacity > 65536 || spec.GroupCapacity < 1 || spec.GroupCapacity > 65536 ||
		validateEndpoint(spec.Endpoint, false, cfg.networks, cfg.ports) != nil ||
		validateEndpoint(spec.HealthURL, true, cfg.networks, cfg.ports) != nil {
		writeError(c, 400, "invalid_instance", "invalid model instance specification", "")
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

func (s *Server) instances(ctx context.Context, c *app.RequestContext) {
	rows, err := s.store.DB.ModelInstance.Query().All(ctx)
	if err != nil {
		storeError(c, err, "")
		return
	}
	holds, err := reserved(ctx, s.store.DB.Request)
	if err != nil {
		storeError(c, err, "")
		return
	}
	units := make(map[string]int)
	requests := make(map[string]int)
	for _, hold := range holds {
		units[hold.InstanceID] += hold.Units
		requests[hold.InstanceID] += hold.Requests
	}
	type view struct {
		*ent.ModelInstance
		ReservedUnits    int  `json:"reserved_units"`
		ReservedRequests int  `json:"reserved_requests"`
		Healthy          bool `json:"healthy"`
		Eligible         bool `json:"eligible"`
	}
	result := make([]view, 0, len(rows))
	cutoff := time.Now().Add(-probeMaxAge)
	for _, row := range rows {
		// 预留单位和请求数分别返回，避免把加权容量显示为在途任务数。
		healthy := row.Ready && row.HealthCheckedAt.After(cutoff)
		result = append(result, view{row, units[row.ID], requests[row.ID], healthy, healthy && !row.Draining})
	}
	writeJSON(c, 200, result)
}

func requestID(c *app.RequestContext) (string, bool) {
	id := string(c.GetHeader("Idempotency-Key"))
	if !validID(id) || len(c.Request.URI().QueryString()) != 0 || len(c.GetHeader("X-Model-Version")) != 0 || len(c.GetHeader("X-Routing-Key")) != 0 {
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
	if len(capabilities(model)) == 0 {
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
	var input Submission
	raw, ok := readBody(c, cfg.MaxBodyBytes, &input, id)
	if !ok {
		<-s.uploads
		return
	}
	capability, err := route(model, input.Input)
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
	task, err := s.store.Admit(ctx, admission{
		id: id, model: model, capability: capability, raw: raw, input: input.Input,
		traceID: input.Metadata.TraceID, units: max(1, cfg.UnitsByCapability[capability]), config: cfg.Config,
	})
	<-s.uploads
	if err != nil {
		storeError(c, err, id)
		return
	}
	if model == videoModel {
		writeJSON(c, 202, Response{RequestID: id, ModelKey: model, State: string(task.State)})
		return
	}
	// 响应等待不持有原始上传，也不取消已经提交到 PG 的生成。
	s.waitImage(ctx, c, id)
}

func taskResponse(task *ent.Request) Response {
	response := Response{RequestID: task.ID, ModelKey: task.ModelKey, State: string(task.State)}
	if task.ErrorCode != "" {
		response.Error = &RequestError{Code: task.ErrorCode, Message: task.ErrorMessage}
	}
	if len(task.Result) != 0 {
		response.Result = &Result{}
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
	writeJSON(c, 200, Response{RequestID: id, State: "failed"})
}

func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
	s.client.CloseIdleConnections()
	_ = s.store.DB.Close()
}
