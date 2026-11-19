package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/inputschema"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func (s *Server) readSynchronousSubmission(ctx context.Context, c *app.RequestContext, definition store.ModelDefinition, limit int64, id string) (protocol.Submission, map[string]any, bool) {
	defer func() { <-s.uploads }()
	raw, ok := readJSONPayload(c, limit, id)
	if !ok {
		return protocol.Submission{}, nil, false
	}
	input, value, err := decodeSynchronousSubmission(raw)
	if err != nil {
		writeError(c, 400, "invalid_json", "invalid JSON request body", id)
		return protocol.Submission{}, nil, false
	}
	if input.Output != "" {
		writeError(c, 400, "invalid_protocol", "model protocol does not match the requested output", id)
		return protocol.Submission{}, nil, false
	}
	fields, err := inputschema.ValidateValue(definition.InputSchema, value, c.GetBool(experimentalCredential))
	if err == nil {
		input.Metadata, err = freezeMetadata(ctx, c, input.Metadata)
	}
	if err != nil {
		writeError(c, 400, "invalid_input", err.Error(), id)
		return protocol.Submission{}, nil, false
	}
	return input, fields, true
}

// Decode the input once for schema validation. The original value is a slice of
// the request bytes so JSON vision executors and their request digests stay identical.
func decodeSynchronousSubmission(raw []byte) (protocol.Submission, any, error) {
	var input protocol.Submission
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	first, err := decoder.Token()
	if err != nil {
		return input, nil, err
	}
	switch first {
	case json.Delim('{'):
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return input, nil, err
			}
			key := token.(string)
			switch {
			case strings.EqualFold(key, "input"):
				start := decoder.InputOffset()
				if err := decoder.Decode(&value); err != nil {
					return input, nil, err
				}
				// Decode has consumed the colon and checked the value's JSON syntax.
				_, data, _ := bytes.Cut(raw[start:decoder.InputOffset()], []byte(":"))
				input.Input = bytes.TrimSpace(data)
			case strings.EqualFold(key, "output"):
				if err := decoder.Decode(&input.Output); err != nil {
					return input, nil, err
				}
			case strings.EqualFold(key, "metadata"):
				if err := decoder.Decode(&input.Metadata); err != nil {
					return input, nil, err
				}
			default:
				return input, nil, fmt.Errorf("unknown request field %q", key)
			}
		}
		if _, err := decoder.Token(); err != nil {
			return input, nil, err
		}
	case nil:
		// An absent input is rejected by the schema.
	default:
		return input, nil, errors.New("expected a JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return input, nil, errors.New("expected one JSON value")
	}
	return input, value, nil
}

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
