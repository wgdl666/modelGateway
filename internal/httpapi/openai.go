package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

const (
	openAIGenerationsPath = "/v1/images/generations"
	openAIEditsPath       = "/v1/images/edits"
	openAIVideosPath      = "/v1/videos"
)

// Both JSON generation endpoints select a registered model directly. The edits
// endpoint is only a multipart transport for the same image generation protocol.
func (s *Server) openAIGenerations(ctx context.Context, c *app.RequestContext) {
	id, ok := optionalRequestID(c)
	if !ok || !s.acquireUpload(ctx) {
		return
	}
	var body protocol.ImageGenerationRequest
	if _, ok := readBody(c, requestConfig(ctx).MaxBodyBytes, &body, id); !ok {
		<-s.uploads
		return
	}
	raw, _ := json.Marshal(body.ImageGenerationInput)
	s.acceptGeneration(ctx, c, id, body.Model, protocol.ImageGenerationProtocol, raw, body.User)
}

func (s *Server) openAIEdits(ctx context.Context, c *app.RequestContext) {
	id, ok := optionalRequestID(c)
	if !ok || !s.acquireUpload(ctx) {
		return
	}
	form, err := readOpenAIForm(c, requestConfig(ctx).MaxBodyBytes, "image", []string{
		"model", "prompt", "n", "size", "response_format", "user", "image", "image[]", "parameters",
	})
	if err != nil {
		<-s.uploads
		writeUploadError(c, err, id)
		return
	}
	if len(form.Images) == 0 {
		<-s.uploads
		writeError(c, 400, "invalid_input", "image edits require reference images", id)
		return
	}
	raw, _ := json.Marshal(protocol.ImageGenerationInput{Prompt: form.Prompt, Images: form.Images,
		N: form.N, Size: form.Size, ResponseFormat: form.ResponseFormat, Parameters: form.Parameters})
	s.acceptGeneration(ctx, c, id, form.Model, protocol.ImageGenerationProtocol, raw, form.User)
}

func (s *Server) openAIVideos(ctx context.Context, c *app.RequestContext) {
	id, ok := optionalRequestID(c)
	if !ok || !s.acquireUpload(ctx) {
		return
	}
	var body protocol.VideoGenerationRequest
	mediaType, _, _ := mime.ParseMediaType(string(c.GetHeader("Content-Type")))
	if mediaType == "multipart/form-data" {
		form, err := readOpenAIForm(c, requestConfig(ctx).MaxBodyBytes, "input_reference", []string{
			"model", "prompt", "size", "seconds", "user", "input_reference", "parameters",
		})
		if err != nil {
			<-s.uploads
			writeUploadError(c, err, id)
			return
		}
		body = protocol.VideoGenerationRequest{Model: form.Model, User: form.User,
			VideoGenerationInput: protocol.VideoGenerationInput{Prompt: form.Prompt, Images: form.Images,
				Size: form.Size, Seconds: form.Seconds, Parameters: form.Parameters}}
	} else if _, ok := readBody(c, requestConfig(ctx).MaxBodyBytes, &body, id); !ok {
		<-s.uploads
		return
	}
	raw, _ := json.Marshal(body.VideoGenerationInput)
	s.acceptGeneration(ctx, c, id, body.Model, protocol.VideoGenerationProtocol, raw, body.User)
}

func (s *Server) acceptGeneration(ctx context.Context, c *app.RequestContext, id, model, kind string, input json.RawMessage, user string) {
	if !protocol.ValidModelKey(model) {
		<-s.uploads
		writeError(c, 400, "invalid_input", "model must be a registered model key", id)
		return
	}
	if err := protocol.ValidateGenerationInput(kind, input); err != nil {
		<-s.uploads
		writeError(c, 400, "invalid_input", err.Error(), id)
		return
	}
	trace := string(c.GetHeader("X-Trace-ID"))
	for _, value := range []string{user, trace} {
		if len(value) > 256 || strings.ContainsFunc(value, func(r rune) bool { return r < 32 || r == 127 }) {
			<-s.uploads
			writeError(c, 400, "invalid_input", "invalid metadata", id)
			return
		}
	}
	registered, ok := s.resolveModelProtocol(ctx, c, model)
	if !ok {
		<-s.uploads
		return
	}
	if registered != kind {
		<-s.uploads
		writeError(c, 400, "invalid_protocol", "registered model does not support this generation interface", id)
		return
	}
	task, ok := s.admitMedia(ctx, c, id, model, kind, protocol.Submission{Input: input,
		Metadata: protocol.Metadata{UserID: user, TraceID: trace}})
	if !ok {
		return
	}
	if kind == protocol.VideoGenerationProtocol {
		writeJSON(c, 200, openAIVideoObject(task))
		return
	}
	s.writeOpenAIImage(ctx, c, task.ID)
}

func (s *Server) openAIVideo(ctx context.Context, c *app.RequestContext) {
	task, ok := s.videoTask(ctx, c)
	if !ok {
		return
	}
	writeJSON(c, 200, openAIVideoObject(task))
}

func (s *Server) openAIVideoContent(ctx context.Context, c *app.RequestContext) {
	task, ok := s.videoTask(ctx, c)
	if !ok {
		return
	}
	if task.State != request.StateSucceeded {
		writeError(c, 409, "video_not_ready", "video is not ready", task.ID)
		return
	}
	view := taskResponse(task)
	if view.Result == nil || len(view.Result.Files) != 1 {
		writeError(c, 404, "media_unavailable", "output is unavailable", task.ID)
		return
	}
	s.writeStoredObject(ctx, c, task.ID, 0, view.Result.Files[0])
}

func (s *Server) videoTask(ctx context.Context, c *app.RequestContext) (*ent.Request, bool) {
	id := c.Param("id")
	if !protocol.ValidID(id) || len(c.Request.URI().QueryString()) != 0 {
		writeError(c, 400, "invalid_request", "video id must be a UUID", "")
		return nil, false
	}
	task, err := s.store.Get(ctx, id)
	if err != nil {
		storeError(c, err, id)
		return nil, false
	}
	if task.Protocol != protocol.VideoGenerationProtocol {
		writeError(c, 404, "video_not_found", "video not found", id)
		return nil, false
	}
	return task, true
}

func (s *Server) writeOpenAIImage(ctx context.Context, c *app.RequestContext, id string) {
	task, ok := s.awaitTask(ctx, c, id)
	if !ok {
		return
	}
	view := taskResponse(task)
	if view.Result == nil || len(view.Result.Files) == 0 {
		writeError(c, 502, "result_unavailable", "cannot publish image result", id)
		return
	}
	data := make([]map[string]string, 0, len(view.Result.Files))
	var total int64
	for index, file := range view.Result.Files {
		mimeType := protocol.OutputMIME(file.Key)
		if mimeType == "" || mimeType == "video/mp4" || !protocol.MatchesOutputKey(id, index, file.Key) {
			writeError(c, 502, "result_unavailable", "cannot publish image result", id)
			return
		}
		body, size, err := s.openObject(ctx, file, mimeType)
		if err != nil {
			writeError(c, 404, "media_unavailable", "output is unavailable", id)
			return
		}
		total += size
		if total > maxImageBytes {
			_ = body.Close()
			writeError(c, 502, "result_unavailable", "image results exceed the response size limit", id)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(body, size))
		_ = body.Close()
		if err != nil || int64(len(raw)) != size {
			writeError(c, 502, "result_unavailable", "cannot publish image result", id)
			return
		}
		data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(raw)})
	}
	writeJSON(c, 200, map[string]any{"created": task.CreatedAt.Unix(), "data": data})
}

func (s *Server) writeStoredObject(ctx context.Context, c *app.RequestContext, id string, index int, file protocol.Object) {
	mimeType := protocol.OutputMIME(file.Key)
	if mimeType == "" || !protocol.MatchesOutputKey(id, index, file.Key) {
		writeError(c, 404, "media_unavailable", "output is unavailable", id)
		return
	}
	body, size, err := s.openObject(ctx, file, mimeType)
	if err != nil {
		writeError(c, 404, "media_unavailable", "output is unavailable", id)
		return
	}
	writeMedia(c, mimeType, body, size)
}

func (s *Server) openObject(ctx context.Context, file protocol.Object, mimeType string) (io.ReadCloser, int64, error) {
	reader, err := s.objectReader(requestConfig(ctx))
	if err != nil {
		return nil, 0, err
	}
	body, size, err := reader.Get(ctx, file)
	if err != nil || size <= 0 || size > mediaLimit(mimeType) {
		if body != nil {
			_ = body.Close()
		}
		return nil, 0, errors.New("output is unavailable")
	}
	return body, size, nil
}

func (s *Server) acquireUpload(ctx context.Context) bool {
	select {
	case s.uploads <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

type openAIVideoJob struct {
	ID        string          `json:"id"`
	Object    string          `json:"object"`
	Model     string          `json:"model"`
	Status    string          `json:"status"`
	CreatedAt int64           `json:"created_at"`
	Error     *openAIAPIError `json:"error,omitempty"`
}

type openAIAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func openAIVideoObject(task *ent.Request) openAIVideoJob {
	job := openAIVideoJob{
		ID: task.ID, Object: "video", Model: task.ModelKey,
		Status: openAIVideoStatus(string(task.State)), CreatedAt: task.CreatedAt.Unix(),
	}
	if task.State == request.StateFailed {
		job.Error = &openAIAPIError{Code: task.ErrorCode, Message: task.ErrorMessage}
	}
	return job
}

func openAIVideoStatus(state string) string {
	switch state {
	case string(request.StateProcessing):
		return "in_progress"
	case string(request.StateSucceeded):
		return "completed"
	default:
		return state
	}
}

func writeUploadError(c *app.RequestContext, err error, id string) {
	status, code := 400, "invalid_input"
	switch err.Error() {
	case "request must use multipart/form-data":
		status, code = 415, "unsupported_media_type"
	case "request exceeds its size limit":
		status, code = 413, "request_too_large"
	case "could not read multipart request":
		status, code = 400, "invalid_multipart"
	}
	writeError(c, status, code, err.Error(), id)
}

type generationForm struct {
	Model, Prompt, Size, ResponseFormat, User string
	N                                         *int
	Seconds                                   *float64
	Images                                    []protocol.InputImage
	Parameters                                json.RawMessage
}

func readOpenAIForm(c *app.RequestContext, limit int64, fileField string, allowed []string) (generationForm, error) {
	mediaType, params, err := mime.ParseMediaType(string(c.GetHeader("Content-Type")))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		c.SetConnectionClose()
		return generationForm{}, errors.New("request must use multipart/form-data")
	}
	raw, err := readPayload(c, limit)
	if int64(len(raw)) > limit {
		c.SetConnectionClose()
		return generationForm{}, errors.New("request exceeds its size limit")
	}
	if err != nil {
		c.SetConnectionClose()
		return generationForm{}, errors.New("could not read multipart request")
	}
	parts := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	var req generationForm
	seen := map[string]bool{}
	count := 0
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return generationForm{}, errors.New("could not read multipart request")
		}
		count++
		if count > 16 {
			return generationForm{}, errors.New("too many form fields")
		}
		name := part.FormName()
		if !slices.Contains(allowed, name) {
			return generationForm{}, errors.New("unsupported field")
		}
		if name == fileField || (fileField == "image" && name == "image[]") {
			if part.FileName() == "" {
				return generationForm{}, errors.New("image file is required")
			}
			image, err := readFormImage(part)
			if err != nil {
				return generationForm{}, err
			}
			req.Images = append(req.Images, image)
			continue
		}
		if part.FileName() != "" || seen[name] {
			return generationForm{}, errors.New("duplicate field")
		}
		seen[name] = true
		value, err := io.ReadAll(io.LimitReader(part, 4097))
		if err != nil || len(value) > 4096 {
			return generationForm{}, errors.New("invalid field")
		}
		text := string(value)
		switch name {
		case "model":
			req.Model = text
		case "prompt":
			req.Prompt = text
		case "size":
			req.Size = text
		case "response_format":
			req.ResponseFormat = text
		case "user":
			req.User = text
		case "parameters":
			if !json.Valid(value) {
				return generationForm{}, errors.New("parameters must be a JSON object")
			}
			req.Parameters = append(json.RawMessage(nil), value...)
		case "seconds":
			number, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
				return generationForm{}, errors.New("seconds must be positive")
			}
			req.Seconds = &number
		case "n":
			number, err := strconv.Atoi(text)
			if err != nil {
				return generationForm{}, errors.New("n must be positive")
			}
			req.N = &number
		default:
			return generationForm{}, errors.New("unsupported field")
		}
	}
	return req, nil
}

func readFormImage(part *multipart.Part) (protocol.InputImage, error) {
	data, err := io.ReadAll(io.LimitReader(part, maxImageBytes+1))
	if err != nil {
		return protocol.InputImage{}, errors.New("could not read image")
	}
	if len(data) == 0 || len(data) > maxImageBytes {
		return protocol.InputImage{}, errors.New("invalid image")
	}
	mimeType := part.Header.Get("Content-Type")
	if mimeType == "image/jpg" {
		mimeType = "image/jpeg"
	}
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	switch mimeType {
	case "image/png", "image/jpeg", "image/webp":
		return protocol.InputImage{MIMEType: mimeType, Data: data}, nil
	default:
		return protocol.InputImage{}, errors.New("image must be png, jpeg, or webp")
	}
}
