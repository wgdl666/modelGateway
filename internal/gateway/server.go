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
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/wgdl666/modelGateway/ent"
)

type Server struct {
	mu            sync.RWMutex
	current       *runtimeConfig
	configChanged chan struct{}
	retired       sync.WaitGroup
	client        *http.Client
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

func NewServer(store *Store, c Config) (*Server, error) {
	state, err := prepareConfig(c)
	if err != nil {
		return nil, err
	}
	state.db = &database{pool: store.DB}
	state.store = &Store{DB: store.DB, Lease: state.InstanceLease}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{current: state, configChanged: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	s.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return s, nil
}

// 每个监听器使用独立 Hertz 引擎，共享业务状态，使地址热更新不会终止已受理的模型任务。
func (s *Server) HTTPServer(listener net.Listener) *server.Hertz {
	h := server.Default(server.WithListener(listener), server.WithReadTimeout(30*time.Second),
		server.WithWriteTimeout(30*time.Second), server.WithIdleTimeout(60*time.Second), server.WithExitWaitTime(30*time.Second),
		server.WithHandleMethodNotAllowed(true), server.WithRedirectTrailingSlash(false), server.WithStreamBody(true),
		server.WithMaxRequestBodySize(4096), server.WithDisablePreParseMultipartForm(true), server.WithSenseClientDisconnection(true))
	h.Use(func(ctx context.Context, c *app.RequestContext) {
		state, release := s.acquire()
		defer release()
		// 鉴权、请求体上限与数据库操作共用请求开始时的快照，避免热加载中途混用配置。
		c.Next(context.WithValue(ctx, runtimeKey{}, state))
	})
	live := func(ctx context.Context, c *app.RequestContext) { writeJSON(c, 200, map[string]bool{"live": true}) }
	ready := func(ctx context.Context, c *app.RequestContext) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if _, err := requestConfig(ctx).store.DB.ModelInstance.Query().Exist(ctx); err != nil {
			writeError(c, 503, "database_unavailable")
			return
		}
		writeJSON(c, 200, map[string]bool{"ready": true})
	}
	h.GET("/health/live", live)
	h.HEAD("/health/live", live)
	h.GET("/health/ready", ready)
	h.HEAD("/health/ready", ready)
	h.POST("/v1/instances/register", s.auth("worker", s.register))
	h.POST("/v1/instances/:id/heartbeat", s.auth("worker", s.heartbeat))
	h.POST("/v1/instances/:id/drain", s.auth("admin", s.drain))
	h.POST("/v1/requests", s.auth("api", s.submit))
	h.GET("/v1/requests/:id", s.auth("api", s.get))
	h.HEAD("/v1/requests/:id", s.auth("api", s.get))
	return h
}

func (s *Server) auth(role string, next app.HandlerFunc) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		cfg := requestConfig(ctx).Config
		token := cfg.APIToken
		switch role {
		case "worker":
			token = cfg.WorkerToken
		case "admin":
			token = cfg.AdminToken
		}
		if subtle.ConstantTimeCompare(c.GetHeader("Authorization"), []byte("Bearer "+token)) != 1 {
			writeError(c, 401, "unauthorized")
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
func writeError(c *app.RequestContext, status int, code string) {
	writeJSON(c, status, map[string]string{"error_code": code})
}
func (s *Server) body(ctx context.Context, c *app.RequestContext, out any) ([]byte, error) {
	limit := requestConfig(ctx).MaxBodyBytes
	// 流式读取按当前快照限长；独立分配的原文可交给后台任务，避免引用 Hertz 回收的请求缓冲。
	raw, err := io.ReadAll(io.LimitReader(c.Request.BodyStream(), limit+1))
	if err != nil || int64(len(raw)) > limit {
		c.SetConnectionClose()
		return nil, errors.New("invalid request body size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("expected one JSON value")
	}
	return raw, nil
}
func storeError(c *app.RequestContext, e error) {
	switch {
	case ent.IsNotFound(e):
		writeError(c, 404, "not_found")
	case errors.Is(e, ErrConflict):
		writeError(c, 409, "instance_conflict")
	case errors.Is(e, ErrBusy):
		writeError(c, 503, "capacity_unavailable")
	default:
		slog.Error("database operation failed", "error_type", fmt.Sprintf("%T", e))
		writeError(c, 503, "database_unavailable")
	}
}
func (s *Server) register(ctx context.Context, c *app.RequestContext) {
	var i Instance
	if _, e := s.body(ctx, c, &i); e != nil || !validID(i.ID) || i.ModelKey == "" || len(i.ModelKey) > 128 || i.Capacity <= 0 || i.Capacity > 65536 {
		writeError(c, 400, "invalid_registration")
		return
	}
	state := requestConfig(ctx)
	if e := validateEndpoint(i.Endpoint, state.networks, state.ports); e != nil {
		writeError(c, 400, "endpoint_not_allowed")
		return
	}
	i.Endpoint = strings.TrimRight(i.Endpoint, "/")
	if i.GPUDevices == nil {
		i.GPUDevices = []int{}
	}
	seen := map[int]bool{}
	for _, gpu := range i.GPUDevices {
		if gpu < 0 || seen[gpu] {
			writeError(c, 400, "invalid_gpu_devices")
			return
		}
		seen[gpu] = true
	}
	if e := state.store.Register(ctx, i); e != nil {
		storeError(c, e)
		return
	}
	writeJSON(c, 200, map[string]bool{"registered": true})
}
func (s *Server) heartbeat(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Ready *bool `json:"ready"`
	}
	if _, e := s.body(ctx, c, &body); e != nil || body.Ready == nil {
		writeError(c, 400, "ready_required")
		return
	}
	if e := requestConfig(ctx).store.Heartbeat(ctx, c.Param("id"), *body.Ready); e != nil {
		storeError(c, e)
		return
	}
	writeJSON(c, 200, map[string]bool{"renewed": true})
}
func (s *Server) drain(ctx context.Context, c *app.RequestContext) {
	var body struct {
		Draining *bool `json:"draining"`
	}
	if _, e := s.body(ctx, c, &body); e != nil || body.Draining == nil {
		writeError(c, 400, "draining_required")
		return
	}
	if e := requestConfig(ctx).store.Drain(ctx, c.Param("id"), *body.Draining); e != nil {
		storeError(c, e)
		return
	}
	writeJSON(c, 200, map[string]bool{"updated": true})
}
func (s *Server) submit(ctx context.Context, c *app.RequestContext) {
	var in Submission
	raw, e := s.body(ctx, c, &in)
	if e != nil || !validID(in.ID) || in.ModelKey == "" || len(in.ModelKey) > 128 || in.Units < 1 || in.Units > 65536 || len(in.Input) == 0 {
		writeError(c, 400, "invalid_request")
		return
	}
	var request Request
	var endpoint string
	var created bool
	state := requestConfig(ctx)
	for attempt := 0; attempt < 3; attempt++ {
		request, endpoint, created, e = state.store.Reserve(ctx, in, raw)
		if !errors.Is(e, ErrBusy) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	if e != nil {
		storeError(c, e)
		return
	}
	if created {
		// 派发不绑定客户端连接；上游超时断开后仍须保存执行结果、释放容量。
		state.db.users.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer state.db.users.Done()
			s.dispatch(state, request.ID, endpoint, raw)
		}()
	}
	status := http.StatusAccepted
	if terminal(request.Status) {
		status = 200
	}
	writeJSON(c, status, request)
}
func (s *Server) get(ctx context.Context, c *app.RequestContext) {
	if !validID(c.Param("id")) {
		writeError(c, 400, "invalid_request_id")
		return
	}
	request, e := requestConfig(ctx).store.Get(ctx, c.Param("id"))
	if e != nil {
		storeError(c, e)
		return
	}
	writeJSON(c, 200, request)
}

func (s *Server) call(ctx context.Context, state *runtimeConfig, method, endpoint string, body []byte) (BackendResponse, error) {
	request, e := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if e != nil {
		return BackendResponse{}, e
	}
	request.Header.Set("Content-Type", "application/json")
	if state.BackendToken != "" {
		request.Header.Set("Authorization", "Bearer "+state.BackendToken)
	}
	response, e := s.client.Do(request)
	if e != nil {
		return BackendResponse{}, e
	}
	defer response.Body.Close()
	// 非 2xx、畸形响应和断连都不能证明后端未执行，不自动重试 POST。
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return BackendResponse{}, fmt.Errorf("backend HTTP %d", response.StatusCode)
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if e != nil || len(raw) > 1<<20 {
		return BackendResponse{}, errors.New("invalid backend response size")
	}
	var result BackendResponse
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&result); e != nil {
		return result, e
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return result, errors.New("trailing backend data")
	}
	return result, result.validate()
}
func (s *Server) save(state *runtimeConfig, id string, response BackendResponse) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := state.store.Apply(ctx, id, response); e != nil {
		slog.Error("request update failed", "request_id", id, "error_type", fmt.Sprintf("%T", e))
	}
}
func (s *Server) dispatch(state *runtimeConfig, id, endpoint string, payload []byte) {
	ctx, cancel := context.WithTimeout(s.ctx, state.BackendTimeout)
	defer cancel()
	started, e := state.store.Start(ctx, id)
	if e != nil || !started {
		return
	}
	result, e := s.call(ctx, state, "POST", endpoint+"/requests", payload)
	if e != nil {
		status, code := "unknown", "backend_outcome_unknown"
		// 仅连接建立前失败可确认没有发送；读取超时不能当失败释放。
		var u *url.Error
		var op *net.OpError
		if errors.As(e, &u) && errors.As(u.Err, &op) && op.Op == "dial" {
			status, code = "failed", "backend_not_connected"
		}
		s.save(state, id, BackendResponse{Status: status, Result: json.RawMessage(`{"error_code":"` + code + `"}`)})
		return
	}
	s.save(state, id, result)
}
func (s *Server) StartReconciler() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			state, release := s.acquire()
			interval := state.PollInterval
			release()
			timer := time.NewTimer(interval)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return
			case <-s.configChanged:
				// 轮询间隔变更立即重设等待时间，不等旧间隔走完。
				timer.Stop()
			case <-timer.C:
				s.ReconcileOnce(s.ctx)
			}
		}
	}()
}
func (s *Server) ReconcileOnce(ctx context.Context) {
	state, release := s.acquire()
	defer release()
	batchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pending, e := state.store.Reconcile(batchCtx, state.PollInterval, state.BackendTimeout+5*time.Second)
	if e != nil {
		if ctx.Err() == nil {
			slog.Error("reconciliation database unavailable")
		}
		return
	}
	var wg sync.WaitGroup
	for _, p := range pending {
		wg.Add(1)
		go func(p Pending) {
			defer wg.Done()
			queryCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			result, e := s.call(queryCtx, state, "GET", p.Endpoint+"/requests/"+p.ID, nil)
			if e != nil {
				result = BackendResponse{Status: "unknown", Result: json.RawMessage(`{"error_code":"backend_outcome_unknown"}`)}
			}
			s.save(state, p.ID, result)
		}(p)
	}
	wg.Wait()
}

// 调用方先停止 HTTP 接入再关闭后台任务；无法确认的执行保持 unknown，其他副本继续核对。
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
	s.retired.Wait()
	s.current.db.users.Wait()
	_ = s.current.db.pool.Close()
	s.client.CloseIdleConnections()
}
