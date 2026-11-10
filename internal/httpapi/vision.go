package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"slices"

	"github.com/cloudwego/hertz/pkg/app"
)

type visionRoute struct {
	path, mediaType string
	fields          []string
}

// 只开放固定推理路径和内联入图字段，避免调用方借用 G6 的网络和存储权限拉取图片。
var visionRoutes = []visionRoute{
	{"/human-parser/human_parser/predict", "application/json", []string{"image"}},
	{"/human-yolo/predict", "multipart/form-data", []string{"file", "image", "conf", "iou"}},
	{"/facesim/compare", "multipart/form-data", []string{"file_a", "file_b", "image_a", "image_b", "face_index_a", "face_index_b"}},
	{"/segment-person/api/segment", "multipart/form-data", []string{"file", "image", "method", "preprocess", "output", "trace_id"}},
	{"/segment-person/api/segment_subject", "multipart/form-data", []string{"file", "image", "method", "output", "trace_id"}},
	{"/lens_distortion_correction/correct", "application/json", []string{"base64", "mirror_lens", "crop", "sampler", "preset"}},
}

func (s *Server) visionPredict(route visionRoute) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		cfg := requestConfig(ctx)
		if cfg.VisionAddr == "" {
			writeError(c, 404, "model_not_found", "G6 vision is not configured", "")
			return
		}
		mediaType, params, err := mime.ParseMediaType(string(c.GetHeader("Content-Type")))
		boundary := params["boundary"]
		if err != nil || mediaType != route.mediaType || (mediaType == "multipart/form-data" && boundary == "") {
			c.SetConnectionClose()
			writeError(c, 415, "unsupported_media_type", "request must use "+route.mediaType, "")
			return
		}
		select {
		case s.uploads <- struct{}{}:
		case <-ctx.Done():
			return
		}
		raw, err := readPayload(c, cfg.MaxBodyBytes)
		tooLarge := int64(len(raw)) > cfg.MaxBodyBytes
		var body []byte
		var contentType string
		if err == nil && !tooLarge {
			body, contentType = route.prepareBody(raw, boundary)
		}
		<-s.uploads
		if tooLarge {
			c.SetConnectionClose()
			writeError(c, 413, "request_too_large", "request exceeds its size limit", "")
			return
		}
		if body == nil {
			c.SetConnectionClose()
			writeError(c, 400, "invalid_vision_input", "use inline images and supported fields for this route", "")
			return
		}
		// G6 自己负责分卡和模型参数校验，网关同步转发 JSON 或图片结果。
		s.forwardDirect(ctx, c, "G6 vision", "http://"+cfg.VisionAddr+route.path, contentType, body)
	}
}

// 只校验字段名，避免再复制一份 Base64 图片。
type visionJSONValue struct{}

func (*visionJSONValue) UnmarshalJSON([]byte) error { return nil }

func (r visionRoute) prepareBody(raw []byte, boundary string) ([]byte, string) {
	if r.mediaType == "application/json" {
		var fields map[string]visionJSONValue
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return nil, ""
		}
		for name := range fields {
			if !slices.Contains(r.fields, name) {
				return nil, ""
			}
		}
		return raw, r.mediaType
	}
	parts := multipart.NewReader(bytes.NewReader(raw), boundary)
	var body bytes.Buffer
	out := multipart.NewWriter(&body)
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			if out.Close() != nil {
				return nil, ""
			}
			return body.Bytes(), out.FormDataContentType()
		}
		if err != nil || !slices.Contains(r.fields, part.FormName()) {
			return nil, ""
		}
		// 重新生成标准表单头，避免 Go 和 G6 对来路表单头的解析差异绕过字段限制。
		var dst io.Writer
		if part.FileName() == "" {
			dst, err = out.CreateFormField(part.FormName())
		} else {
			dst, err = out.CreateFormFile(part.FormName(), "image")
		}
		if err != nil {
			return nil, ""
		}
		if _, err := io.Copy(dst, part); err != nil {
			return nil, ""
		}
	}
}
