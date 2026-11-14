package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/rabbitmq/amqp091-go"
	"github.com/wgdl666/kangaroo/callmeta"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
	"github.com/wgdl666/modelGateway/internal/telemetry"
	"go.opentelemetry.io/otel"
)

const (
	workerResponseLimit       = 1 << 20
	probeInterval             = 2 * time.Second
	nativeVideoPublishWorkers = 2
)

// Reconciler 按 PG 账本派发与核对结果，入站连接断开后仍可处理已受理的任务。
type Reconciler struct {
	store   *store.Store
	runtime *config.Runtime
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	nativeImageAuth   nativeAuth
	nativeVideoAuth   nativeAuth
	results           *resultUploader
	videoPublications chan func() error
}

func New(tasks *store.Store, runtime *config.Runtime) (*Reconciler, error) {
	w := &Reconciler{store: tasks, runtime: runtime, videoPublications: make(chan func() error)}
	cfg := runtime.Current()
	// 连接暂时失败由投递循环重试，缺失启动配置会使已受理任务一直留在账本。
	if cfg.RabbitMQURL == "" || cfg.RabbitMQQueue == "" {
		return nil, errors.New("MODEL_GATEWAY_RABBITMQ_URL and rabbitmq_queue are required")
	}
	if _, err := amqp091.ParseURI(cfg.RabbitMQURL); err != nil {
		return nil, errors.New("invalid RabbitMQ connection configuration")
	}
	// 原生路由和结果桶属于部署配置，客户端与执行器共享同一份固定配置。
	if len(cfg.NativeBackends) != 0 || len(cfg.NativeModels) != 0 {
		var err error
		w.nativeImageAuth, err = readNativeAuth("MODEL_GATEWAY_NATIVE_IMAGE")
		if err != nil {
			return nil, err
		}
		w.nativeVideoAuth, err = readNativeAuth("MODEL_GATEWAY_NATIVE_VIDEO")
		if err != nil {
			return nil, err
		}
	}
	if len(cfg.NativeBackends) != 0 || len(cfg.NativeModels) != 0 || cfg.NativeResults.Provider != "" {
		var err error
		w.results, err = newResultUploader(cfg.NativeResults)
		if err != nil {
			return nil, err
		}
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = config.SubjectPoolDialer(backendDialer(net.DefaultResolver.LookupNetIP, dialer.DialContext))
	w.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return w, nil
}

func (w *Reconciler) Close() {
	w.cancel()
	w.wg.Wait()
	w.client.CloseIdleConnections()
}

type modelStatusError int

func (status modelStatusError) Error() string { return fmt.Sprintf("model HTTP %d", status) }

var errWorkerBusyBeforeExecution = errors.New("model worker busy before execution")

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
	resp, err := w.modelHTTP(req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readModelBody(resp.Body, workerResponseLimit)
	if err != nil {
		return nil, errors.New("invalid model response body")
	}
	return raw, nil
}

func (w *Reconciler) modelHTTP(req *http.Request, nativeSubmission bool) (*http.Response, error) {
	// Validate each request, including health, polling and artifact downloads.
	// The transport pins DNS answers at dial time; pooled connections have already
	// passed that check. Synchronous upstreams retain their separate fixed policy.
	cfg := w.runtime.Current()
	if err := cfg.ValidateEndpoint(req.URL.String(), true); err != nil {
		return nil, err
	}
	req = req.WithContext(context.WithValue(req.Context(), backendPolicyKey{}, cfg))
	// 所有模型执行/回查 HTTP 共用来源传播；探活没有任务身份。
	callmeta.Propagator{}.Inject(req.Context(), callmeta.HTTPHeaders(req.Header))
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		// 只有原生提交阶段的标准路由拒绝能确认未受理，回查 404 无法确认执行终态。
		if nativeSubmission && resp.StatusCode == http.StatusNotFound && nativeRouteNotFound(resp) {
			return nil, errNativeRouteUnavailable
		}
		// 只相信首次派发响应里的明确未执行声明，避免用探活、回查或代理的 429 重派生成。
		if req.Method == http.MethodPost && resp.StatusCode == http.StatusTooManyRequests && workerBusyBeforeExecution(resp.Body) {
			return nil, errWorkerBusyBeforeExecution
		}
		if detail := readModelValidationError(resp); detail != nil {
			return nil, detail
		}
		return nil, modelStatusError(resp.StatusCode)
	}
	return resp, nil
}

func workerBusyBeforeExecution(body io.Reader) bool {
	var rejection struct {
		Error struct {
			Code             string `json:"code"`
			ExecutionStarted *bool  `json:"execution_started"`
		} `json:"error"`
	}
	raw, err := readModelBody(body, 4<<10)
	return err == nil && json.Unmarshal(raw, &rejection) == nil &&
		rejection.Error.Code == "worker_busy" &&
		rejection.Error.ExecutionStarted != nil && !*rejection.Error.ExecutionStarted
}

func readModelBody(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("invalid model response body")
	}
	return raw, nil
}

func (w *Reconciler) execute(cfg *config.Snapshot, task *ent.Request) error {
	var payload protocol.Submission
	if err := json.Unmarshal(task.RequestPayload, &payload); err != nil {
		return w.failDispatch(task, 500, "invalid_saved_request", "saved request is invalid", false)
	}
	backend, direct := cfg.NativeBackendFor(task.ModelKey, task.Endpoint)
	generic := protocol.GenerationProtocol(task.Protocol)
	var raw []byte
	if !direct && !generic {
		var err error
		raw, err = json.Marshal(protocol.ModelRequest{RequestID: task.ID, ModelKey: task.ModelKey, Submission: payload, DeploymentID: task.DeploymentID, ArtifactDigest: task.ArtifactDigest})
		if err != nil {
			return w.failDispatch(task, 500, "invalid_saved_request", "saved request is invalid", false)
		}
	}
	if !time.Now().Before(task.DeadlineAt) {
		return w.failDispatch(task, 504, "queue_timeout", "request expired before dispatch", false)
	}
	if cfg.ValidateEndpoint(task.Endpoint, generic) != nil {
		return w.failDispatch(task, 503, "backend_unavailable", "selected model address is unavailable", false)
	}
	deadline := task.DeadlineAt
	if direct || generic {
		// 上游先等超时也要继续接原生响应，拿到真实终态后才能释放 GPU。
		deadline = task.StartedAt.Add(cfg.NativeExecutionTimeout)
	}
	ctx, cancel := context.WithDeadline(payload.Metadata.Restore(w.ctx), deadline)
	defer cancel()
	ctx, span := otel.Tracer("modelGateway.worker").Start(ctx, "gateway.dispatch")
	defer span.End()
	callmeta.AnnotateSpan(ctx)
	if generic {
		return w.executeGeneration(ctx, cfg, task, payload)
	}
	if direct {
		return w.executeNative(ctx, cfg, task, payload, backend)
	}
	response, err := w.modelCall(ctx, cfg, http.MethodPost, task.Endpoint, "/requests", raw)
	if err != nil {
		return w.dispatchError(task, err)
	}
	if !deploymentResponseMatches(response, task) {
		return w.failDispatch(task, 502, "deployment_identity_mismatch", "worker response did not match assigned deployment", false)
	}
	return w.recordModelResponse(cfg, task, response, false)
}

func (w *Reconciler) dispatchError(task *ent.Request, err error) error {
	if errors.Is(err, errWorkerBusyBeforeExecution) {
		return w.requeueDispatch(task, "worker_busy_before_execution")
	}
	var status modelStatusError
	if errors.As(err, &status) {
		// 参数、鉴权和其他明确的 4xx 拒绝不靠重复派发恢复。
		switch status {
		case 400, 413, 415:
			return w.failDispatch(task, 400, "model_rejected", modelValidationMessage(err, "model rejected the request before admission"), false)
		case 401, 403:
			return w.failDispatch(task, 502, "model_authentication_failed", "model rejected the gateway credential", false)
		}
		if status >= 400 && status < 500 {
			return w.failDispatch(task, 502, "model_rejected", modelValidationMessage(err, "model rejected the request"), false)
		}
	}
	// Managed native business APIs cannot prove idempotence or query a lost
	// execution by request ID. Preserve the reservation until it is reconciled.
	if task.DeploymentID != "" && !protocol.GenerationProtocol(task.Protocol) {
		return w.failDispatch(task, 502, "deployment_execution_unknown", "backend execution outcome is unknown; reconcile before retiring the deployment", true)
	}
	// Legacy behavior and durable generic backends retain their retry contract.
	return w.requeueDispatch(task)
}

func (w *Reconciler) requeueDispatch(task *ent.Request, reasons ...string) error {
	ctx, cancel := context.WithTimeout(w.taskContext(context.Background(), task), store.ExecutionWriteTimeout)
	defer cancel()
	// 待投递记录与释放同次写入，成功后消费者才 ACK；MQ 短暂不可用也不会丢掉重试。
	return w.store.RequeueDispatch(ctx, task.ID, *task.StartedAt, task.PollClaim, reasons...)
}

func (w *Reconciler) failDispatch(task *ent.Request, status int, code, message string, hold bool) error {
	ctx, cancel := context.WithTimeout(w.taskContext(context.Background(), task), store.ExecutionWriteTimeout)
	defer cancel()
	return w.store.DispatchFailure(ctx, task.ID, task.PollClaim, status, code, message, hold)
}

func (w *Reconciler) recordModelResponse(cfg *config.Snapshot, task *ent.Request, raw []byte, polling bool) error {
	var response protocol.ModelResponse
	if err := protocol.DecodeJSON(raw, &response); err != nil {
		response = protocol.ModelResponse{}
	}
	return w.saveModelResponse(cfg, task, response, polling)
}

func (w *Reconciler) saveModelResponse(cfg *config.Snapshot, task *ent.Request, response protocol.ModelResponse, polling bool) error {
	ctx, cancel := context.WithTimeout(w.taskContext(context.Background(), task), store.ExecutionWriteTimeout)
	defer cancel()
	if response.Validate(task.ID) != nil {
		if polling {
			return w.store.PollUnavailable(ctx, task.ID, task.PollClaim, cfg.PollInterval)
		}
		return w.requeueDispatch(task)
	}
	return w.store.RecordResponse(ctx, task.ID, task.PollClaim, response, cfg.PollInterval)
}

func (w *Reconciler) pollModel(cfg *config.Snapshot, task *ent.Request) error {
	if protocol.GenerationProtocol(task.Protocol) {
		return w.pollGeneration(cfg, task)
	}
	if backend, direct := cfg.NativeBackendFor(task.ModelKey, task.Endpoint); direct {
		return w.pollNative(cfg, task, backend)
	}
	ctx, cancel := context.WithTimeout(w.taskContext(w.ctx, task), 3*time.Second)
	defer cancel()
	raw, err := w.modelCall(ctx, cfg, http.MethodGet, task.Endpoint, "/requests/"+task.ID, nil)
	if err == nil {
		return w.recordModelResponse(cfg, task, raw, true)
	}
	return w.pollUnavailable(cfg, task)
}

func (w *Reconciler) pollUnavailable(cfg *config.Snapshot, task *ent.Request) error {
	ctx, cancel := context.WithTimeout(w.taskContext(context.Background(), task), 3*time.Second)
	defer cancel()
	return w.store.PollUnavailable(ctx, task.ID, task.PollClaim, cfg.PollInterval)
}

func (w *Reconciler) probe(cfg *config.Snapshot) error {
	// 在读取登记前开始计时，避免读到旧地址后才产生新时间戳。
	checked := time.Now().UTC()
	rows, err := w.store.ProbeInstances(w.ctx)
	if err != nil {
		return err
	}
	// 同轮并行探测，单个模型超时不会阻断其他资源组的健康更新。
	var probes sync.WaitGroup
	for _, row := range rows {
		if protocol.SynchronousKind(row.Protocol) != "" {
			continue
		}
		probes.Go(func() {
			var ready bool
			if cfg.AcceptsInstanceAddress(row.ModelKey, row.Protocol, row.Endpoint, row.HealthURL) {
				target := protocol.InstanceTarget{ModelKey: row.ModelKey, Protocol: row.Protocol, Endpoint: row.Endpoint, HealthURL: row.HealthURL, GPUDevices: row.GpuDevices, DeploymentID: row.DeploymentID, ArtifactDigest: row.ArtifactDigest, BootID: row.BootID, WorkerID: row.WorkerID}
				result, err := w.ProbeInstance(w.ctx, cfg, target)
				ready = err == nil && result.Ready
			}
			if err := w.store.RecordProbe(w.ctx, row.ID, checked, ready); err != nil && w.ctx.Err() == nil {
				telemetry.LogError(w.ctx, "model health update failed", err, "instance_id", row.ID)
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
				telemetry.LogError(w.ctx, label, err)
			}
			select {
			case <-w.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (w *Reconciler) runVideoPublisher() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case publish := <-w.videoPublications:
			if err := publish(); err != nil && w.ctx.Err() == nil {
				telemetry.LogError(w.ctx, "native video publication failed", err)
			}
		}
	}
}

// 派发、回查和视频发布各用独立协程，慢传输不会占用状态查询的并发。
func (w *Reconciler) Start() {
	w.runWorker("model health probe failed", probeInterval, func() error { return w.probe(w.runtime.Current()) })
	// 同步上游有独立探活预算，不阻塞图像和视频派单使用的探针。
	w.runWorker("synchronous model health probe failed", 5*time.Second, func() error { return w.probeSynchronous(w.runtime.Current()) })
	// 截止时间仍由账本保存；后台每 2 秒收敛过期状态，避免空闲高频更新。
	w.runWorker("queue expiry failed", 2*time.Second, func() error { return w.store.Expire(w.ctx) })
	w.runRabbit("queue publication failed", w.publishQueued)
	for range 2 {
		w.runRabbit("queue dispatch failed", w.consumeQueued)
	}
	for range nativeVideoPublishWorkers {
		w.wg.Go(w.runVideoPublisher)
	}
	// 单条回查管线有任务时沿用 500ms 节奏，空闲时等待 2 秒，取消可立即唤醒。
	w.runWorker("model reconciliation failed", 500*time.Millisecond, func() error {
		task, err := w.store.ClaimPoll(w.ctx)
		if err != nil {
			return err
		}
		if task == nil {
			select {
			case <-w.ctx.Done():
			case <-time.After(2 * time.Second):
			}
			return nil
		}
		return w.pollModel(w.runtime.Current(), task)
	})
}

// taskContext 供轮询、重新发布和终态写入恢复受理快照；不读取当前查询者身份。
func (w *Reconciler) taskContext(parent context.Context, task *ent.Request) context.Context {
	var saved protocol.Submission
	if err := json.Unmarshal(task.RequestPayload, &saved); err != nil {
		telemetry.LogError(parent, "saved request metadata unavailable", err, "request_id", task.ID)
	}
	return saved.Metadata.Restore(parent)
}
