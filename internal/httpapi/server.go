package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
	"github.com/wgdl666/modelGateway/internal/telemetry"
)

// Server 固定每次入站请求的配置快照，让鉴权、地址校验和受理使用同一份配置。
type objectReader interface {
	Get(ctx context.Context, object protocol.Object) (io.ReadCloser, int64, error)
}

type Server struct {
	store   *store.Store
	runtime *config.Runtime
	uploads chan struct{}
	direct  *http.Client
	probe   func(context.Context, *config.Snapshot, protocol.InstanceTarget) (protocol.InstanceHealth, error)

	objectsOnce sync.Once
	objects     objectReader
	objectsErr  error
}

func New(tasks *store.Store, runtime *config.Runtime, probe func(context.Context, *config.Snapshot, protocol.InstanceTarget) (protocol.InstanceHealth, error)) *Server {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 同步推理可包含原图，只允许直连配置中的私网模型地址。
	transport.Proxy = nil
	transport.DialContext = config.SubjectPoolDialer(transport.DialContext)
	return &Server{
		store: tasks, runtime: runtime, uploads: make(chan struct{}, 2), probe: probe,
		direct: &http.Client{
			Transport:     transport,
			Timeout:       120 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
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
	h.Use(observeRequest)
	h.GET("/_gateway/live", func(_ context.Context, c *app.RequestContext) { writeJSON(c, 200, map[string]bool{"live": true}) })
	h.GET("/_gateway/ready", s.ready)
	// 实例登记决定网关可以向哪些执行地址派单，因此使用管理凭据。
	h.GET("/v1/models/:model_key", s.auth("api", s.modelProtocol))
	h.POST("/v1/instances/register", s.auth("admin", s.register))
	h.POST("/v1/instances/probe", s.auth("admin", s.probeInstance))
	h.POST("/v1/instances/:id/drain", s.auth("admin", s.drain))
	h.POST("/v1/instances/:id/edit", s.auth("admin", s.editInstance))
	h.GET("/v1/instances", s.auth("admin", s.instances))
	h.DELETE("/v1/instances/:id", s.auth("admin", s.clearInstance))
	// 组容量会影响同卡所有模型，只允许用管理凭据变更。
	h.GET("/v1/resource-groups", s.auth("admin", s.listResourceGroups))
	h.POST("/v1/resource-groups", s.auth("admin", s.createResourceGroup))
	h.PATCH("/v1/resource-groups", s.auth("admin", s.updateResourceGroup))
	h.PATCH("/v1/resource-groups/machine", s.auth("admin", s.renameResourceGroupMachine))
	h.GET("/v1/admin/requests", s.auth("admin", s.listRequests))
	h.GET("/v1/admin/request-stats", s.auth("admin", s.requestStats))
	h.GET("/v1/admin/requests/:id/media", s.auth("admin", s.requestMedia))
	h.GET("/v1/admin/requests/:id", s.auth("admin", s.requestDetail))
	h.POST("/v1/gateway/requests/cancel", s.auth("api", s.cancelRequest))
	h.POST("/v1/gateway/requests/:model_key", s.auth("api", s.submit))
	h.GET("/v1/gateway/requests", s.auth("api", s.get))
	h.POST(chatCompletionsPath, s.auth("api", s.chatCompletions))
	h.POST(decisionPredictPath, s.auth("api", s.decisionPredict))
	h.GET("/v1/vision/health", s.auth("admin", s.visionHealth))
	for _, route := range visionRoutes {
		h.POST("/v1/vision"+route.path, s.auth("api", s.visionPredict(route)))
	}
	return h
}

// experimentalCredential 标记请求使用了实验凭据，受理时据此放行实验参数。
const experimentalCredential = "experimental_credential"

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
			if !allowed && cfg.ExperimentalToken != "" && match(cfg.ExperimentalToken) {
				allowed = true
				c.Set(experimentalCredential, true)
			}
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
	case errors.Is(err, store.ErrResourceGroupNotFound):
		writeError(c, 404, "resource_group_not_found", err.Error(), id)
	case errors.Is(err, store.ErrResourceMachineNotFound):
		writeError(c, 404, "resource_machine_not_found", err.Error(), id)
	case errors.Is(err, store.ErrResourceGroupExists):
		writeError(c, 409, "resource_group_exists", err.Error(), id)
	case errors.Is(err, store.ErrResourceGroupDeviceMismatch):
		writeError(c, 409, "resource_group_device_mismatch", err.Error(), id)
	case errors.Is(err, store.ErrLegacyResourceGroup):
		writeError(c, 409, "legacy_resource_group", err.Error(), id)
	case ent.IsNotFound(err):
		writeError(c, 404, "not_found", "record does not exist", id)
	case errors.Is(err, store.ErrConflict):
		writeError(c, 409, "conflict", "request or instance conflicts with an existing record", id)
	case errors.Is(err, store.ErrInstanceNotClearable):
		writeError(c, 409, "instance_not_clearable", err.Error(), id)
	case errors.Is(err, store.ErrFull):
		writeError(c, 429, "queue_full", "waiting queue is full", id)
	case errors.Is(err, store.ErrModelUnregistered):
		writeError(c, 503, "model_unregistered", "model has no registered instance", id)
	default:
		telemetry.LogError(context.Background(), "database operation failed", err, "request_id", id)
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
	raw, err := readPayload(c, limit)
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

func readPayload(c *app.RequestContext, limit int64) ([]byte, error) {
	reader := c.Request.BodyStream()
	if reader == nil {
		reader = bytes.NewReader(c.Request.Body())
	}
	return io.ReadAll(io.LimitReader(reader, limit+1))
}

func (s *Server) ready(ctx context.Context, c *app.RequestContext) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.store.CheckReady(ctx); err != nil {
		// 具体检查项由 Store 记录，响应保持脱敏且仍拒绝接流量。
		writeError(c, 503, "database_unavailable", "task storage is temporarily unavailable", "")
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
	if !spec.Valid() {
		writeError(c, 400, "invalid_instance", "invalid model instance specification", "")
		return
	}
	target := protocol.InstanceTarget{ModelKey: spec.ModelKey, Protocol: spec.Protocol, Endpoint: spec.Endpoint, HealthURL: spec.HealthURL, GPUDevices: spec.GPUDevices}
	if err := cfg.ValidateInstanceTarget(target); err != nil {
		writeError(c, 400, "invalid_instance", err.Error(), "")
		return
	}
	// 原生视频地址选择实际 worker，登记卡号必须与地址中的卡号一致。
	if !cfg.AcceptsInstanceGPU(spec.ModelKey, spec.Endpoint, spec.GPUDevices) {
		writeError(c, 400, "invalid_instance", "physical GPU does not match the video endpoint", "")
		return
	}
	var artifact string
	if spec.Artifact == "" && protocol.SynchronousKind(spec.Protocol) == "" {
		// 登记时重新读取服务上报，表单预览可能来自更早的一次部署。
		health, err := s.probe(ctx, cfg, target)
		if err == nil {
			artifact = health.ArtifactDigest
		}
	}
	if err := s.store.Register(ctx, spec, artifact); err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]string{"instance_id": spec.ID})
}

func (s *Server) probeInstance(ctx context.Context, c *app.RequestContext) {
	cfg := requestConfig(ctx)
	var target protocol.InstanceTarget
	if _, ok := readBody(c, 4096, &target, ""); !ok {
		return
	}
	target.Endpoint = strings.TrimRight(target.Endpoint, "/")
	if !protocol.ValidModelKey(target.ModelKey) || !protocol.ValidProtocol(target.Protocol) {
		writeError(c, 400, "invalid_instance", "invalid model_key or protocol", "")
		return
	}
	if err := cfg.ValidateInstanceTarget(target); err != nil {
		writeError(c, 400, "invalid_instance", err.Error(), "")
		return
	}
	// 手动探活只返回即时结果，持续健康仍由后台探针写入。
	health, err := s.probe(ctx, cfg, target)
	if err != nil {
		writeError(c, 502, "health_probe_failed", "health endpoint unavailable or returned an invalid response", "")
		return
	}
	writeJSON(c, 200, health)
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
	kind, ok := s.resolveModelProtocol(ctx, c, model)
	if !ok {
		return
	}
	if !protocol.MediaProtocol(kind) {
		writeError(c, 400, "invalid_protocol", "model does not use the media task protocol", id)
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
	// 模型存在性、输出能力和具体字段在同一次受理内校验，不依赖调用方预查询。
	if input.Output != "" && ((input.Output != "image" && input.Output != "video") || (input.Output == "video") != (kind == protocol.VideoProtocol)) {
		<-s.uploads
		writeError(c, 400, "invalid_protocol", "model protocol does not match the requested output", id)
		return
	}
	normalized, err := protocol.NormalizeMediaInput(kind, input.Input)
	if err == nil {
		input.Input = normalized
		validate := protocol.ValidateInput
		if c.GetBool(experimentalCredential) {
			validate = protocol.ValidateExperimentalInput
		}
		err = validate(kind, input.Input)
	}
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
	raw, err = json.Marshal(input)
	var media []byte
	if err == nil {
		// 输入已通过校验，这里只为列表记下每张图的角色和尺寸。
		var images []protocol.InputImage
		if _, images, err = protocol.DecodeInput(input.Input); err == nil {
			media, err = json.Marshal(protocol.InputMedia(kind, images))
		}
	}
	if err != nil {
		<-s.uploads
		writeError(c, 400, "invalid_input", "could not encode normalized input", id)
		return
	}
	task, err := s.store.Admit(ctx, store.Admission{
		ID: id, ModelKey: model, Protocol: kind, Payload: raw, Input: input.Input, InputMedia: media,
		// 占用单位随受理落库，后续配置更新不会改变已排队请求的重量。
		TraceID: input.Metadata.TraceID, Units: max(1, cfg.UnitsByModelKey[model]),
		MaxWaiting: cfg.MaxWaiting, BackendTimeout: cfg.BackendTimeout,
	})
	<-s.uploads
	if err != nil {
		storeError(c, err, id)
		return
	}
	// 视频调用方通过查询接口取终态，提交请求无需保持连接。
	if kind == protocol.VideoProtocol {
		writeJSON(c, 202, protocol.Response{RequestID: id, TraceID: task.TraceID, ModelKey: model, State: string(task.State)})
		return
	}
	// 响应等待不持有原始上传，也不取消已经提交到 PG 的生成。
	s.waitImage(ctx, c, id)
}

func taskResponse(task *ent.Request) protocol.Response {
	response := protocol.Response{RequestID: task.ID, TraceID: task.TraceID, ModelKey: task.ModelKey, State: string(task.State)}
	if task.ErrorCode != "" {
		response.Error = &protocol.RequestError{Code: task.ErrorCode, Message: task.ErrorMessage}
		if task.DispatchAttempts > 0 {
			retry := &protocol.RetrySummary{Count: task.DispatchAttempts - 1}
			if retry.Count > 0 {
				retry.LastReason = task.DispatchRetryReason
			}
			response.Error.Retries = &protocol.GenerationRetries{GatewayDispatch: retry}
		}
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
