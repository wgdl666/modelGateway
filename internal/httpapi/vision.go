package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"slices"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

type visionRoute struct {
	path, mediaType    string
	fields             []string
	modelKey, protocol string
}

const mattePath = "/segment-person/api/matte"

// 只开放固定推理路径和内联入图字段，避免调用方借用 G6 的网络和存储权限拉取图片。
var visionRoutes = []visionRoute{
	{
		path: mattePath, mediaType: "application/json",
		fields: []string{"image", "method", "person_method", "subject_method", "output", "trace_id"},
	},
	{
		path: "/human-parser/human_parser/predict", modelKey: protocol.HumanParseModel, protocol: protocol.HumanParseProtocol,
		mediaType: "application/json", fields: []string{"image"},
	},
	{
		path: "/human-yolo/predict", modelKey: protocol.HumanYOLOModel, protocol: protocol.HumanYOLOProtocol,
		mediaType: "multipart/form-data", fields: []string{"file", "image", "conf", "iou"},
	},
	{
		path: "/facesim/compare", modelKey: protocol.FaceSimModel, protocol: protocol.FaceSimProtocol,
		mediaType: "multipart/form-data", fields: []string{"file_a", "file_b", "image_a", "image_b", "face_index_a", "face_index_b"},
	},
	{
		path: "/segment-person/api/segment", modelKey: protocol.SegmentPersonModel, protocol: protocol.SegmentPersonProtocol,
		mediaType: "multipart/form-data", fields: []string{"file", "image", "method", "preprocess", "output", "trace_id"},
	},
	{
		path: "/segment-person/api/segment_subject", modelKey: protocol.SegmentSubjectModel, protocol: protocol.SegmentSubjectProtocol,
		mediaType: "multipart/form-data", fields: []string{"file", "image", "method", "output", "trace_id"},
	},
	{
		path: "/lens_distortion_correction/correct", modelKey: protocol.LensCorrectionModel, protocol: protocol.LensCorrectionProtocol,
		mediaType: "application/json", fields: []string{"base64", "mirror_lens", "crop", "sampler", "preset"},
	},
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
		raw, err := readPayload(c, cfg.VisionMaxBodyBytes)
		tooLarge := int64(len(raw)) > cfg.VisionMaxBodyBytes
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
		client := s.direct
		if strings.HasPrefix(route.path, "/segment-person/") {
			// 人像与主体抠图沿用 ModelHub 直连时的 180 秒上游等待预算。
			segment := *s.direct
			segment.Timeout = 180 * time.Second
			client = &segment
		}
		// 兼容现有固定路径调用，带模型名时按登记校验，避免分人请求误走分主体协议。
		model := string(c.GetHeader("X-Model-Key"))
		if model == "" {
			model = route.modelKey
		}
		kind, ok := s.resolveModelProtocol(ctx, c, model)
		if !ok {
			return
		}
		targetPath := route.path
		if route.path == mattePath {
			// 一次抠图提交只携带模型名，人物/商品入口及算法在网关内按登记选择。
			if kind != protocol.SegmentPersonProtocol && kind != protocol.SegmentSubjectProtocol {
				writeError(c, 400, "invalid_protocol", "model does not use a segment protocol", "")
				return
			}
			body, contentType = prepareMatteBody(body, kind)
			if body == nil {
				writeError(c, 400, "invalid_vision_input", "segment input requires an inline image and method", "")
				return
			}
			targetPath = "/segment-person/api/segment"
			if kind == protocol.SegmentSubjectProtocol {
				targetPath += "_subject"
			}
		} else if kind != route.protocol {
			writeError(c, 400, "invalid_protocol", "model protocol does not match this vision route", "")
			return
		}
		s.forwardSynchronous(ctx, c, client, model, kind, targetPath, contentType, body)
	}
}

func prepareMatteBody(raw []byte, kind string) ([]byte, string) {
	var fields map[string]string
	if json.Unmarshal(raw, &fields) != nil || fields["image"] == "" || fields["output"] != "b64" {
		return nil, ""
	}
	methodKey := "subject_method"
	if kind == protocol.SegmentPersonProtocol {
		methodKey = "person_method"
		fields["preprocess"] = "false"
	}
	if fields[methodKey] != "" {
		fields["method"] = fields[methodKey]
	}
	if fields["method"] == "" {
		return nil, ""
	}
	delete(fields, "person_method")
	delete(fields, "subject_method")
	var body bytes.Buffer
	out := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := out.WriteField(key, value); err != nil {
			return nil, ""
		}
	}
	if out.Close() != nil {
		return nil, ""
	}
	return body.Bytes(), out.FormDataContentType()
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
