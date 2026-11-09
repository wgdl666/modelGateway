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

var errNativeRouteUnavailable = errors.New("native image route is unavailable")

type nativeAuth struct {
	username, password, token string
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

// 原生接口路径和文件字段随模型键固定，商品图和拆衣各走自己的入口。
var nativeRoutes = map[string]struct {
	path   string
	images []string
}{
	protocol.ClothGenModel: {"/v2/images/product_generation", []string{"image"}},
	protocol.WardrobeModel: {"/v2/images/garment_extraction", []string{"image", "parse_mask"}},
	protocol.OutfitModel:   {"/v2/images/vton", []string{"person_image", "garment_image"}},
	protocol.BodyModel:     {"/v2/images/body_model", []string{"image"}},
	protocol.VideoModel:    {"/vton", []string{"image"}},
}

// 视频目标来自本次占槽的实例登记，原生服务据此使用对应的 GPU。
func nativeMultipart(task *ent.Request, input protocol.Submission, backend config.NativeBackend, targetInstance string) (string, *bytes.Buffer, http.Header, error) {
	route, ok := nativeRoutes[task.ModelKey]
	if !ok || !backend.Matches(task.ModelKey) {
		return "", nil, nil, errors.New("model key does not match native backend")
	}
	fields, images, err := protocol.DecodeInput(input.Input)
	if err != nil {
		return "", nil, nil, err
	}
	minImages := len(route.images)
	if task.ModelKey == protocol.WardrobeModel {
		minImages = 1
	}
	if len(images) < minImages || len(images) > len(route.images) {
		return "", nil, nil, errors.New("wrong number of input images")
	}
	body := new(bytes.Buffer)
	writer := multipart.NewWriter(body)
	form := make(map[string]string, len(fields))
	for key, raw := range fields {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			value = string(raw)
		}
		form[key] = value
	}
	fluxEdits := backend.VTONEdits && task.ModelKey == protocol.OutfitModel
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
	headers.Set("X-Gateway-Dispatch", "immediate")
	if trace := input.Metadata.TraceID; trace != "" {
		headers.Set("X-Trace-ID", trace)
		form["trace_id"] = trace
	}
	if user := input.Metadata.UserID; user != "" {
		headers.Set("X-User-ID", user)
	}
	if backend.Video {
		form["target_instance"] = targetInstance
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
		part, err := writer.CreatePart(header)
		if err != nil {
			return "", nil, nil, err
		}
		if _, err := part.Write(image.Data); err != nil {
			return "", nil, nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return "", nil, nil, err
	}
	headers.Set("Content-Type", writer.FormDataContentType())
	return route.path, body, headers, nil
}

// 调用方先校验地址；原生请求只带该模型的凭据，复用已有的禁代理和禁重定向客户端。
func (w *Reconciler) nativeHTTP(ctx context.Context, auth nativeAuth, method, url string, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		req.Header = headers
	}
	if auth.username != "" {
		req.SetBasicAuth(auth.username, auth.password)
	}
	if auth.token != "" {
		req.Header.Set("X-Token", auth.token)
	}
	return w.modelHTTP(req)
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

func (w *Reconciler) nativeReady(ctx context.Context, cfg *config.Snapshot, row *ent.ModelInstance, backend config.NativeBackend) bool {
	if !backend.Accepts(row.ModelKey, row.Endpoint, row.HealthURL, row.TargetInstance) ||
		cfg.ValidateEndpoint(row.HealthURL, true) != nil {
		return false
	}
	type workerHealth struct {
		Ready   *bool `json:"ready"`
		Healthy bool  `json:"healthy"`
	}
	var health struct {
		Ready     *bool  `json:"ready"`
		Status    string `json:"status"`
		Instances []struct {
			Name string `json:"name"`
		} `json:"instances"`
		Workers []workerHealth `json:"workers"`
	}
	if w.nativeJSON(ctx, w.nativeAuth(backend), row.HealthURL, &health) != nil {
		return false
	}
	ready := health.Status == "ok"
	if health.Ready != nil {
		ready = *health.Ready
	}
	if !ready {
		return false
	}
	if backend.Video && !slices.ContainsFunc(health.Instances, func(i struct {
		Name string `json:"name"`
	}) bool {
		return i.Name == row.TargetInstance
	}) {
		return false
	}
	// 国内 FLUX 用 ready 表示可接单，旧实例的同一信号由 healthy 提供。
	return health.Workers == nil || slices.ContainsFunc(health.Workers, func(w workerHealth) bool {
		if w.Ready != nil {
			return *w.Ready
		}
		return w.Healthy
	})
}

var nativeJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type nativeJob struct {
	ID     string `json:"job_id"`
	Status string `json:"status"`
}

func (w *Reconciler) executeNative(ctx context.Context, cfg *config.Snapshot, task *ent.Request, input protocol.Submission, backend config.NativeBackend, targetInstance string) error {
	path, body, headers, err := nativeMultipart(task, input, backend, targetInstance)
	if err != nil {
		return w.failDispatch(task.ID, 500, "invalid_saved_request", "saved request cannot be dispatched to the native model", false)
	}
	// 表单准备也可能消耗排队预算，真正发出前再检查上游截止时间。
	if !time.Now().Before(task.DeadlineAt) || ctx.Err() != nil {
		return w.failDispatch(task.ID, 504, "queue_timeout", "request expired before dispatch", false)
	}
	resp, err := w.nativeHTTP(ctx, w.nativeAuth(backend), http.MethodPost, task.Endpoint+path, body, headers)
	if err != nil {
		if errors.Is(err, errNativeRouteUnavailable) {
			return w.failDispatch(task.ID, 502, "native_route_unavailable", err.Error(), false)
		}
		// 原生接口用 422 拒绝无效表单，429 拒绝无空闲槽位的请求。
		var status modelStatusError
		if errors.As(err, &status) && (status == 422 || status == 429) {
			return w.failDispatch(task.ID, 502, "model_rejected", "native model rejected the request before execution", false)
		}
		return w.dispatchError(task.ID, err)
	}
	limit := int64(nativeImageLimit)
	if task.ModelKey == protocol.VideoModel {
		limit = workerResponseLimit
	}
	raw, err := readModelBody(resp.Body, limit)
	_ = resp.Body.Close()
	if err != nil {
		return w.dispatchError(task.ID, err)
	}
	if task.ModelKey == protocol.VideoModel {
		var job nativeJob
		if json.Unmarshal(raw, &job) != nil || !nativeJobID.MatchString(job.ID) {
			return w.failDispatch(task.ID, 502, "invalid_model_response", "video admission outcome is unknown", true)
		}
		// 记录后端分配的 ID，再交给已有轮询；同一请求始终查原 LTX 任务。
		response := protocol.ModelResponse{Response: protocol.Response{RequestID: task.ID, State: "processing"}, BackendTaskID: job.ID}
		return w.saveModelResponse(cfg, task, response, false)
	}

	files, err := w.results.publishImages(ctx, task.ID, raw)
	if err != nil {
		return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "result_unavailable", "cannot publish image result"), false)
	}
	return w.saveModelResponse(cfg, task, nativeSuccess(task.ID, files), false)
}

func (u *resultUploader) publishImages(ctx context.Context, id string, raw []byte) ([]protocol.Object, error) {
	var images struct {
		Data []struct {
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
	return files, nil
}

func (w *Reconciler) pollNative(cfg *config.Snapshot, task *ent.Request, backend config.NativeBackend) error {
	// 图片同步返回成品。连接中断或 LTX 受理响应丢失时，继续保留槽位等待核对。
	if task.ModelKey != protocol.VideoModel || !backend.Matches(task.ModelKey) || !nativeJobID.MatchString(task.BackendTaskID) {
		return w.pollUnavailable(cfg, task)
	}
	auth := w.nativeAuth(backend)
	jobURL := task.Endpoint + "/jobs/" + task.BackendTaskID
	ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
	var job nativeJob
	err := w.nativeJSON(ctx, auth, jobURL, &job)
	cancel()
	if err != nil || (job.ID != "" && job.ID != task.BackendTaskID) {
		return w.pollUnavailable(cfg, task)
	}
	switch job.Status {
	case "done":
		ctx, cancel := context.WithTimeout(w.ctx, cfg.BackendTimeout)
		defer cancel()
		deadline, _ := ctx.Deadline()
		claimed, err := w.store.ExtendPoll(ctx, task.ID, task.PollClaim, deadline.Add(store.ExecutionWriteTimeout))
		if err != nil || !claimed {
			return err
		}
		// shortcut: 当前副本在轮询中完成成品发布，有并发视频查询延迟时再拆分发布。
		file, err := w.publishNativeVideo(ctx, w.results, auth, task.ID, jobURL+"/video")
		if err != nil {
			return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "result_unavailable", "cannot publish video result"), true)
		}
		return w.saveModelResponse(cfg, task, nativeSuccess(task.ID, []protocol.Object{file}), true)
	case "error", "cancelled", "interrupted":
		return w.saveModelResponse(cfg, task, nativeFailure(task.ID, "model_failed", "native video execution failed"), true)
	default:
		return w.saveModelResponse(cfg, task, protocol.ModelResponse{Response: protocol.Response{RequestID: task.ID, State: "processing"}}, true)
	}
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
