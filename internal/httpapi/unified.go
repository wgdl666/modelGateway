package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// 统一入口只做契约校验与编码分派，受理、占槽及终态仍由原执行路径处理。
func (s *Server) submitSynchronousInput(ctx context.Context, c *app.RequestContext, model, kind string, raw json.RawMessage, fields map[string]any, metadata protocol.Metadata) {
	cfg := requestConfig(ctx)
	// Embedding 原样转发到已占槽的副本，维度和归一化参数由模型契约约束。
	if kind == protocol.EmbeddingProtocol {
		s.forwardSynchronous(ctx, c, s.direct, model, kind, "/v1/embed", "application/json", raw, metadata)
		return
	}
	if kind == protocol.ChatProtocol || kind == protocol.DecisionProtocol {
		if kind == protocol.DecisionProtocol {
			if cfg.DecisionAddr == "" {
				writeError(c, 503, "model_unavailable", "decision execution is not configured", "")
				return
			}
			body, contentType, stream, err := prepareDecisionChat(fields)
			if err != nil {
				writeError(c, 400, "invalid_input", err.Error(), "")
				return
			}
			c.Set(decisionChatStreamKey, stream)
			s.forwardSynchronous(ctx, c, s.direct, model, kind, "/v1/predict", contentType, body, metadata)
			return
		}
		if cfg.VLMAddr == "" {
			writeError(c, 503, "model_unavailable", "chat execution is not configured", "")
			return
		}
		fields["model"] = model
		delete(fields, "model_traits")
		body, err := json.Marshal(fields)
		if err != nil {
			writeError(c, 400, "invalid_input", err.Error(), "")
			return
		}
		s.forwardSynchronous(ctx, c, s.direct, model, kind, "/v1/chat/completions", "application/json", body, metadata)
		return
	}
	if cfg.VisionAddr == "" {
		writeError(c, 503, "model_unavailable", "vision execution is not configured", "")
		return
	}
	for _, route := range visionRoutes {
		if route.protocol != kind {
			continue
		}
		body, contentType, err := unifiedVisionBody(kind, route, raw, fields)
		if err != nil {
			writeError(c, 400, "invalid_input", err.Error(), "")
			return
		}
		client := s.direct
		if strings.HasPrefix(route.path, "/segment-person/") {
			copy := *s.direct
			copy.Timeout = 180 * time.Second
			client = &copy
		}
		s.forwardSynchronous(ctx, c, client, model, kind, route.path, contentType, body, metadata)
		return
	}
	writeError(c, 400, "invalid_protocol", "model has no synchronous executor", "")
}

func unifiedVisionBody(kind string, route visionRoute, raw json.RawMessage, fields map[string]any) ([]byte, string, error) {
	if route.mediaType == "application/json" {
		return raw, route.mediaType, nil
	}
	if kind == protocol.SegmentPersonProtocol || kind == protocol.SegmentSubjectProtocol {
		method := "subject_method"
		if kind == protocol.SegmentPersonProtocol {
			method = "person_method"
		}
		if value, exists := fields[method]; exists {
			fields["method"] = value
			if kind == protocol.SegmentPersonProtocol {
				fields["preprocess"] = "false"
			}
		}
		delete(fields, "person_method")
		delete(fields, "subject_method")
	}
	var body bytes.Buffer
	out := multipart.NewWriter(&body)
	for name, value := range fields {
		text, isString := value.(string)
		if !isString {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, "", err
			}
			text = string(encoded)
		}
		if name == "file" || name == "file_a" || name == "file_b" {
			data, err := base64.StdEncoding.DecodeString(text)
			if err != nil {
				return nil, "", fmt.Errorf("%s must be base64 image bytes", name)
			}
			file, err := out.CreateFormFile(name, "image")
			if err != nil {
				return nil, "", err
			}
			if _, err := file.Write(data); err != nil {
				return nil, "", err
			}
		} else if err := out.WriteField(name, text); err != nil {
			return nil, "", err
		}
	}
	if err := out.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), out.FormDataContentType(), nil
}
