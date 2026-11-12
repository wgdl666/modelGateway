package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

const (
	openAIGenerationsPath = "/v1/images/generations"
	openAIEditsPath       = "/v1/images/edits"
	openAIVideosPath      = "/v1/videos"
)

type openAIGenerationBody struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
}

func (s *Server) openAIGenerations(ctx context.Context, c *app.RequestContext) {
	id, ok := optionalRequestID(c)
	if !ok {
		return
	}
	if !s.acquireUpload(ctx) {
		return
	}
	cfg := requestConfig(ctx)
	var body openAIGenerationBody
	if !readOpenAIJSON(c, cfg.MaxBodyBytes, &body, id) {
		<-s.uploads
		return
	}
	route, err := protocol.TranslateOpenAIImage(protocol.InterfaceImageGeneration, protocol.OpenAIImageRequest{
		Model: body.Model, Prompt: body.Prompt, N: body.N, Size: body.Size, ResponseFormat: body.ResponseFormat,
	}, c.GetBool(experimentalCredential))
	if err != nil {
		<-s.uploads
		writeError(c, 400, "invalid_input", err.Error(), id)
		return
	}
	s.acceptOpenAI(ctx, c, id, route, false)
}

func (s *Server) openAIEdits(ctx context.Context, c *app.RequestContext) {
	id, ok := optionalRequestID(c)
	if !ok {
		return
	}
	if !s.acquireUpload(ctx) {
		return
	}
	req, err := readOpenAIForm(c, requestConfig(ctx).MaxBodyBytes, "image", []string{
		"model", "prompt", "n", "size", "response_format", "user", "image", "garment_name", "bbox", "bbox_format", "gender",
	})
	if err != nil {
		<-s.uploads
		writeUploadError(c, err, id)
		return
	}
	route, err := protocol.TranslateOpenAIImage(protocol.InterfaceImageEdit, req, c.GetBool(experimentalCredential))
	if err != nil {
		<-s.uploads
		writeError(c, 400, "invalid_input", err.Error(), id)
		return
	}
	s.acceptOpenAI(ctx, c, id, route, false)
}

func (s *Server) openAIVideos(ctx context.Context, c *app.RequestContext) {
	id, ok := optionalRequestID(c)
	if !ok {
		return
	}
	if !s.acquireUpload(ctx) {
		return
	}
	req, err := readOpenAIForm(c, requestConfig(ctx).MaxBodyBytes, "input_reference", []string{
		"model", "prompt", "size", "user", "input_reference",
	})
	if err != nil {
		<-s.uploads
		writeUploadError(c, err, id)
		return
	}
	route, err := protocol.TranslateOpenAIVideo(req, c.GetBool(experimentalCredential))
	if err != nil {
		<-s.uploads
		writeError(c, 400, "invalid_input", err.Error(), id)
		return
	}
	s.acceptOpenAI(ctx, c, id, route, true)
}

func (s *Server) acceptOpenAI(ctx context.Context, c *app.RequestContext, id string, route protocol.ExpertRoute, video bool) {
	registered, err := s.store.ModelProtocol(ctx, route.ModelKey)
	if errors.Is(err, store.ErrModelUnregistered) {
		<-s.uploads
		writeError(c, 404, "model_not_found", "model has no registration", id)
		return
	}
	if err != nil {
		<-s.uploads
		storeError(c, err, id)
		return
	}
	if registered != route.Protocol {
		<-s.uploads
		writeError(c, 400, "invalid_protocol", "registered protocol does not match the expert route", id)
		return
	}
	task, ok := s.admitMedia(ctx, c, id, route.ModelKey, route.Protocol, protocol.Submission{Input: route.Input})
	if !ok {
		return
	}
	if video {
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
	if task.Protocol != protocol.VideoProtocol {
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
	if view.Result == nil || len(view.Result.Files) != 1 {
		writeError(c, 502, "result_unavailable", "cannot publish image result", id)
		return
	}
	file := view.Result.Files[0]
	mimeType := protocol.OutputMIME(file.Key)
	if mimeType == "" || mimeType == "video/mp4" || !protocol.MatchesOutputKey(id, 0, file.Key) {
		writeError(c, 502, "result_unavailable", "cannot publish image result", id)
		return
	}
	body, size, err := s.openObject(ctx, file, mimeType)
	if err != nil {
		writeError(c, 404, "media_unavailable", "output is unavailable", id)
		return
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, size))
	if err != nil || int64(len(data)) != size {
		writeError(c, 502, "result_unavailable", "cannot publish image result", id)
		return
	}
	writeJSON(c, 200, map[string]any{
		"created": task.CreatedAt.Unix(),
		"data":    []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(data)}},
	})
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
		ID: task.ID, Object: "video", Model: protocol.ExpertForModel(task.ModelKey),
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

func readOpenAIJSON(c *app.RequestContext, limit int64, body *openAIGenerationBody, id string) bool {
	mediaType, _, err := mime.ParseMediaType(string(c.GetHeader("Content-Type")))
	if err != nil || mediaType != "application/json" {
		c.SetConnectionClose()
		writeError(c, 415, "unsupported_media_type", "request must use application/json", id)
		return false
	}
	raw, err := readPayload(c, limit)
	if int64(len(raw)) > limit {
		c.SetConnectionClose()
		writeError(c, 413, "request_too_large", "request exceeds its size limit", id)
		return false
	}
	if err != nil {
		c.SetConnectionClose()
		writeError(c, 400, "invalid_json", "invalid JSON request body", id)
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		writeError(c, 400, "invalid_json", "invalid JSON request body", id)
		return false
	}
	for key := range fields {
		if !slices.Contains([]string{"model", "prompt", "n", "size", "response_format", "user"}, key) {
			writeError(c, 400, "invalid_input", "unsupported field", id)
			return false
		}
	}
	if json.Unmarshal(raw, body) != nil {
		writeError(c, 400, "invalid_json", "invalid JSON request body", id)
		return false
	}
	return true
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

func readOpenAIForm(c *app.RequestContext, limit int64, fileField string, allowed []string) (protocol.OpenAIImageRequest, error) {
	mediaType, params, err := mime.ParseMediaType(string(c.GetHeader("Content-Type")))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		c.SetConnectionClose()
		return protocol.OpenAIImageRequest{}, errors.New("request must use multipart/form-data")
	}
	raw, err := readPayload(c, limit)
	if int64(len(raw)) > limit {
		c.SetConnectionClose()
		return protocol.OpenAIImageRequest{}, errors.New("request exceeds its size limit")
	}
	if err != nil {
		c.SetConnectionClose()
		return protocol.OpenAIImageRequest{}, errors.New("could not read multipart request")
	}
	parts := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	var req protocol.OpenAIImageRequest
	seen := map[string]bool{}
	count := 0
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return protocol.OpenAIImageRequest{}, errors.New("could not read multipart request")
		}
		count++
		if count > 16 {
			return protocol.OpenAIImageRequest{}, errors.New("too many form fields")
		}
		name := part.FormName()
		if !slices.Contains(allowed, name) {
			return protocol.OpenAIImageRequest{}, errors.New("unsupported field")
		}
		if name == fileField {
			if part.FileName() == "" {
				return protocol.OpenAIImageRequest{}, errors.New("image file is required")
			}
			image, err := readFormImage(part)
			if err != nil {
				return protocol.OpenAIImageRequest{}, err
			}
			req.Images = append(req.Images, image)
			continue
		}
		if part.FileName() != "" || seen[name] {
			return protocol.OpenAIImageRequest{}, errors.New("duplicate field")
		}
		seen[name] = true
		value, err := io.ReadAll(io.LimitReader(part, 4097))
		if err != nil || len(value) > 4096 {
			return protocol.OpenAIImageRequest{}, errors.New("invalid field")
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
		case "garment_name":
			req.GarmentName = text
		case "bbox":
			req.BBox = append(json.RawMessage(nil), value...)
		case "bbox_format":
			req.BBoxFormat = text
		case "gender":
			req.Gender = text
		case "n":
			number, err := strconv.Atoi(text)
			if err != nil {
				return protocol.OpenAIImageRequest{}, errors.New("only n=1 is supported")
			}
			req.N = &number
		default:
			return protocol.OpenAIImageRequest{}, errors.New("unsupported field")
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
