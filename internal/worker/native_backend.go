package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

const (
	nativeImageLimit = 64 << 20
	nativeVideoLimit = 200 << 20
	fluxEditsPath    = "/v1/images/edits"
)

var errNativeRouteUnavailable = errors.New("native model route is unavailable")

type nativeAuth struct {
	username, password, token, bearer string
}

func readNativeAuth(prefix string) (nativeAuth, error) {
	auth := nativeAuth{username: os.Getenv(prefix + "_USERNAME"), password: os.Getenv(prefix + "_PASSWORD"),
		token: os.Getenv(prefix + "_TOKEN")}
	if (auth.username == "") != (auth.password == "") {
		return nativeAuth{}, errors.New("native backend Basic Auth requires username and password")
	}
	if strings.ContainsFunc(auth.token, func(r rune) bool { return r < 32 || r == 127 }) {
		return nativeAuth{}, errors.New("invalid native backend token")
	}
	return auth, nil
}

func (w *Reconciler) nativeAuth(backend config.NativeBackend) nativeAuth {
	if backend.Video {
		return w.nativeVideoAuth
	}
	return w.nativeImageAuth
}

// 原生接口路径和文件字段由登记协议决定，商品图和拆衣各走自己的入口。
var nativeRoutes = map[string]struct {
	path   string
	images []string
}{
	protocol.ClothGenProtocol: {"/v2/images/product_generation", []string{"image"}},
	protocol.WardrobeProtocol: {"/v2/images/garment_extraction", []string{"image", "parse_mask"}},
	protocol.OutfitProtocol:   {"/v2/images/vton", []string{"person_image", "garment_image"}},
	protocol.BodyProtocol:     {"/v2/images/body_model", []string{"image"}},
	protocol.VideoProtocol:    {"/vton", []string{"image"}},
}

// nativeMultipartBody 按顺序读取小型表单头和已有图片字节，不复制整包媒体。
type nativeMultipartBody struct {
	io.Reader
	length int64
	chunks [][]byte
}

// replay 为 HTTP 重放重建读取游标，仍共享不可变的图片切片。
func (b *nativeMultipartBody) replay() io.ReadCloser {
	readers := make([]io.Reader, len(b.chunks))
	for i, chunk := range b.chunks {
		readers[i] = bytes.NewReader(chunk)
	}
	return io.NopCloser(io.MultiReader(readers...))
}

// 视频 worker 由执行地址的单卡路径选定，表单只转发业务输入。
func nativeMultipart(task *ent.Request, input protocol.Submission, backend config.NativeBackend) (string, *nativeMultipartBody, http.Header, error) {
	route, ok := nativeRoutes[task.Protocol]
	if !ok || !backend.Matches(task.Protocol) {
		return "", nil, nil, errors.New("registered protocol does not match native backend")
	}
	fields, images, err := protocol.DecodeInput(input.Input)
	if err != nil {
		return "", nil, nil, err
	}
	minImages := len(route.images)
	if task.Protocol == protocol.WardrobeProtocol {
		minImages = 1
	}
	if len(images) < minImages || len(images) > len(route.images) {
		return "", nil, nil, errors.New("wrong number of input images")
	}
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	var chunks [][]byte
	var length int64
	form := make(map[string]string, len(fields))
	for key, raw := range fields {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			value = string(raw)
		}
		form[key] = value
	}
	fluxEdits := backend.VTONEdits && task.Protocol == protocol.OutfitProtocol
	if fluxEdits {
		// 国内 FLUX 由 vton LoRA 选择试衣，两个文件按人像、衣物顺序使用同一个字段名。
		route.path = fluxEditsPath
		form["model"] = "FLUX.2-klein-9B"
		form["lora_name"] = "vton"
		form["lora_strength"] = "1.0"
		form["n"] = "1"
		form["size"] = "768x1024"
		form["response_format"] = "b64_json"
	}
	headers := make(http.Header)
	headers.Set("X-Gateway-Request-ID", task.ID)
	setDeploymentHeaders(headers, task)
	headers.Set("X-Gateway-Dispatch", "immediate")
	if trace := input.Metadata.TraceID; trace != "" {
		headers.Set("X-Trace-ID", trace)
		form["trace_id"] = trace
	}
	if user := input.Metadata.UserID; user != "" {
		headers.Set("X-User-ID", user)
	}
	for key, value := range form {
		if err := writer.WriteField(key, value); err != nil {
			return "", nil, nil, err
		}
	}
	for i, image := range images {
		name := route.images[i]
		filename := name
		if fluxEdits {
			filename += "." + strings.TrimPrefix(image.MIMEType, "image/")
			name = "image"
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": name, "filename": filename}))
		header.Set("Content-Type", image.MIMEType)
		_, err := writer.CreatePart(header)
		if err != nil {
			return "", nil, nil, err
		}
		// 只复制小型边界和字段；图片直接从解码后的切片读取。
		headerBytes := bytes.Clone(body.Bytes())
		chunks = append(chunks, headerBytes, image.Data)
		length += int64(len(headerBytes) + len(image.Data))
		body.Reset()
	}
	if err := writer.Close(); err != nil {
		return "", nil, nil, err
	}
	headers.Set("Content-Type", writer.FormDataContentType())
	chunks = append(chunks, body.Bytes())
	length += int64(body.Len())
	stream := &nativeMultipartBody{length: length, chunks: chunks}
	stream.Reader = stream.replay()
	return route.path, stream, headers, nil
}

// 调用方先校验地址；原生请求只带该模型的凭据，复用已有的禁代理和禁重定向客户端。
func (w *Reconciler) nativeHTTP(ctx context.Context, auth nativeAuth, method, url string, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if multipartBody, ok := body.(*nativeMultipartBody); ok {
		req.ContentLength = multipartBody.length
		req.GetBody = func() (io.ReadCloser, error) { return multipartBody.replay(), nil }
	}
	if headers != nil {
		req.Header = headers
	}
	if auth.username != "" {
		req.SetBasicAuth(auth.username, auth.password)
	}
	if auth.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+auth.bearer)
	}
	if auth.token != "" {
		req.Header.Set("X-Token", auth.token)
	}
	return w.modelHTTP(req, method == http.MethodPost)
}

func nativeRouteNotFound(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "text/plain" && mediaType != "application/json") {
		return false
	}
	raw, err := readModelBody(resp.Body, 64)
	if err != nil {
		return false
	}
	if mediaType == "text/plain" {
		return strings.EqualFold(strings.TrimSpace(string(raw)), "not found")
	}
	var body map[string]string
	return json.Unmarshal(raw, &body) == nil && len(body) == 1 && body["detail"] == "Not Found"
}

func (w *Reconciler) nativeJSON(ctx context.Context, auth nativeAuth, url string, out any) error {
	resp, err := w.nativeHTTP(ctx, auth, http.MethodGet, url, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := readModelBody(resp.Body, workerResponseLimit)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (w *Reconciler) nativeHealth(ctx context.Context, cfg *config.Snapshot, target protocol.InstanceTarget, backend config.NativeBackend) (protocol.InstanceHealth, error) {
	if err := cfg.ValidateEndpoint(target.HealthURL, true); err != nil {
		return protocol.InstanceHealth{}, err
	}
	type workerHealth struct {
		Ready   *bool `json:"ready"`
		Healthy bool  `json:"healthy"`
	}
	var health struct {
		healthReport
		Status    string `json:"status"`
		Instances []struct {
			Name string `json:"name"`
			GPUs []int  `json:"gpus"`
		} `json:"instances"`
		Workers []workerHealth `json:"workers"`
	}
	if err := w.nativeJSON(ctx, w.nativeAuth(backend), target.HealthURL, &health); err != nil {
		return protocol.InstanceHealth{}, err
	}
	ready := health.Status == "ok"
	if health.Ready != nil {
		ready = *health.Ready
	}
	if backend.Video {
		// 单卡探针只有命中目标 GPU 才能提供该登记的健康和制品身份。
		if len(health.Instances) != 1 {
			return protocol.InstanceHealth{}, nil
		}
		instance := health.Instances[0]
		if instance.Name == "" || len(instance.GPUs) != 1 {
			return protocol.InstanceHealth{}, nil
		}
		if len(target.GPUDevices) != 0 {
			if !slices.Equal(instance.GPUs, target.GPUDevices) {
				return protocol.InstanceHealth{}, nil
			}
		}
		if !cfg.AcceptsInstanceGPU(target.ModelKey, target.Endpoint, instance.GPUs) {
			return protocol.InstanceHealth{}, nil
		}
		return health.result(ready), nil
	}
	// 国内 FLUX 用 ready 表示可接单，旧实例的同一信号由 healthy 提供。
	ready = ready && (health.Workers == nil || slices.ContainsFunc(health.Workers, func(w workerHealth) bool {
		if w.Ready != nil {
			return *w.Ready
		}
		return w.Healthy
	}))
	return health.result(ready), nil
}

var nativeJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type nativeJob struct {
	Generation *protocol.Generation `json:"generation"`
	ID         string               `json:"job_id"`
	Status     string               `json:"status"`
}

func (w *Reconciler) executeNative(ctx context.Context, cfg *config.Snapshot, task *ent.Request, input protocol.Submission, backend config.NativeBackend) error {
	path, body, headers, err := nativeMultipart(task, input, backend)
	if err != nil {
		return w.failDispatch(task, 500, "invalid_saved_request", "saved request cannot be dispatched to the native model", false)
	}
	// 表单准备也可能消耗排队预算，真正发出前再检查上游截止时间。
	if !time.Now().Before(task.DeadlineAt) || ctx.Err() != nil {
		return w.failDispatch(task, 504, "queue_timeout", "request expired before dispatch", false)
	}
	resp, err := w.nativeHTTP(ctx, w.nativeAuth(backend), http.MethodPost, task.Endpoint+path, body, headers)
	if err != nil {
		if errors.Is(err, errNativeRouteUnavailable) {
			return w.failDispatch(task, 502, "native_route_unavailable", err.Error(), false)
		}
		// 带明确未执行声明的忙响应交给通用派发逻辑，普通拒绝沿用原生接口的失败路径。
		var status modelStatusError
		if errors.As(err, &status) && (status == 422 || status == 429) {
			return w.failDispatch(task, 502, "model_rejected", modelValidationMessage(err, "native model rejected the request before execution"), false)
		}
		return w.dispatchError(task, err)
	}
	limit := int64(nativeImageLimit)
	if task.Protocol == protocol.VideoProtocol {
		limit = workerResponseLimit
	}
	raw, err := readModelBody(resp.Body, limit)
	_ = resp.Body.Close()
	if err != nil {
		return w.dispatchError(task, err)
	}
	if !deploymentResponseMatches(raw, task) {
		return w.failDispatch(task, 502, "deployment_identity_mismatch", "worker response did not match the assigned deployment", false)
	}
	if task.Protocol == protocol.VideoProtocol {
		var job nativeJob
		if json.Unmarshal(raw, &job) != nil || !nativeJobID.MatchString(job.ID) {
			return w.requeueDispatch(task)
		}
		// 记录后端分配的 ID，再交给已有轮询；同一请求始终查原 LTX 任务。
		response := protocol.ModelResponse{Response: protocol.Response{RequestID: task.ID, State: "processing"}, BackendTaskID: job.ID}
		return w.saveModelResponse(cfg, task, response, false)
	}

	result, err := w.results.publishImages(ctx, task.ID, raw)
	if err != nil {
		return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "result_unavailable", "cannot publish image result"), false)
	}
	response := nativeSuccess(task.ID, result.Files)
	response.Result.Generation = result.Generation
	return w.saveModelResponse(cfg, task, response, false)
}

func (u *resultUploader) publishImages(ctx context.Context, id string, raw []byte) (*protocol.Result, error) {
	var images struct {
		Generation *protocol.Generation `json:"generation"`
		Data       []struct {
			Bytes []byte `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &images); err != nil || len(images.Data) == 0 {
		return nil, errors.New("invalid native image result")
	}
	files := make([]protocol.Object, 0, len(images.Data))
	for i, image := range images.Data {
		contentType := http.DetectContentType(image.Bytes)
		var ext string
		switch contentType {
		case "image/jpeg":
			ext = "jpg"
		case "image/png", "image/webp":
			ext = strings.TrimPrefix(contentType, "image/")
		default:
			return nil, errors.New("unknown output image format")
		}
		key := fmt.Sprintf("outputs/%s/%d.%s", id, i, ext)
		file, err := u.upload(ctx, key, contentType, bytes.NewReader(image.Bytes), int64(len(image.Bytes)))
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return &protocol.Result{Files: files, Generation: images.Generation}, nil
}

func (w *Reconciler) pollNative(cfg *config.Snapshot, task *ent.Request, backend config.NativeBackend) error {
	if task.DeploymentID != "" && task.ErrorCode == "deployment_execution_unknown" {
		return w.pollUnavailable(cfg, task)
	}
	if !backend.Matches(task.Protocol) {
		return w.pollUnavailable(cfg, task)
	}
	if !backend.Video || task.BackendTaskID == "" {
		// 领取窗口已过且没有可回查的任务，按同一请求回队；历史失败记录只归还占用。
		if task.State == "failed" {
			return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "backend_outcome_unknown", "model call ended without a saved result"), true)
		}
		return w.requeueDispatch(task)
	}
	if !nativeJobID.MatchString(task.BackendTaskID) {
		return w.pollUnavailable(cfg, task)
	}
	auth := w.nativeAuth(backend)
	jobURL := task.Endpoint + "/jobs/" + task.BackendTaskID
	ctx, cancel := context.WithTimeout(w.taskContext(w.ctx, task), 3*time.Second)
	var job nativeJob
	err := w.nativeJSON(ctx, auth, jobURL, &job)
	cancel()
	if err != nil || (job.ID != "" && job.ID != task.BackendTaskID) {
		return w.pollUnavailable(cfg, task)
	}
	switch job.Status {
	case "done":
		publish := func() error {
			return w.completeNativeVideo(cfg, task, auth, jobURL+"/video", job.Generation)
		}
		// 发布协程忙时按原间隔回查重试，避免轮询阻塞或把在途领取堆在内存里。
		select {
		case w.videoPublications <- publish:
			return nil
		default:
		}
	case "error", "cancelled", "interrupted":
		return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "model_failed", "native video execution failed"), true)
	}
	return w.saveModelResponse(cfg, task, protocol.ModelResponse{Response: protocol.Response{RequestID: task.ID, State: "processing"}}, true)
}

func (w *Reconciler) completeNativeVideo(cfg *config.Snapshot, task *ent.Request, auth nativeAuth, url string, generation *protocol.Generation) error {
	ctx, cancel := context.WithTimeout(w.taskContext(w.ctx, task), cfg.BackendTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	// 领取仍有效才开始传输，并给发布后的终态写入留足时间。
	claimed, err := w.store.ExtendPoll(ctx, task.ID, task.PollClaim, deadline.Add(store.ExecutionWriteTimeout))
	if err != nil || !claimed {
		return err
	}
	file, err := w.publishNativeVideo(ctx, w.results, auth, task.ID, url)
	if err != nil {
		// 本副本关停时保留任务，领取到期后可以重新发布同一成品。
		if w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "result_unavailable", "cannot publish video result"), true)
	}
	response := nativeSuccess(task.ID, []protocol.Object{file})
	response.Result.Generation = generation
	return w.saveModelResponse(cfg, task, response, true)
}

func (w *Reconciler) publishNativeVideo(ctx context.Context, results *resultUploader, auth nativeAuth, id, url string) (protocol.Object, error) {
	resp, err := w.nativeHTTP(ctx, auth, http.MethodGet, url, nil, nil)
	if err != nil {
		return protocol.Object{}, err
	}
	defer resp.Body.Close()
	// 视频先落有大小上限的临时文件，SDK 可以重读，网关内存不随视频大小增长。
	body, err := os.CreateTemp("", "modelgateway-video-*")
	if err != nil {
		return protocol.Object{}, err
	}
	defer os.Remove(body.Name())
	defer body.Close()
	size, err := io.Copy(body, io.LimitReader(resp.Body, nativeVideoLimit+1))
	if err != nil {
		return protocol.Object{}, err
	}
	if size == 0 || size > nativeVideoLimit {
		return protocol.Object{}, errors.New("invalid native video size")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return protocol.Object{}, err
	}
	return results.upload(ctx, "outputs/"+id+"/0.mp4", "video/mp4", body, size)
}

func nativeSuccess(id string, files []protocol.Object) protocol.ModelResponse {
	return protocol.ModelResponse{Response: protocol.Response{RequestID: id, State: "succeeded", Result: &protocol.Result{Files: files}}}
}

func nativeFailure(id, code, message string) protocol.ModelResponse {
	return protocol.ModelResponse{Response: protocol.Response{RequestID: id, State: "failed", Error: &protocol.RequestError{Code: code, Message: message}}}
}
