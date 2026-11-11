package httpapi

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

const chatCompletionsPath = "/v1/chat/completions"

func (s *Server) chatCompletions(ctx context.Context, c *app.RequestContext) {
	cfg := requestConfig(ctx)
	if cfg.VLMAddr == "" {
		writeError(c, 404, "model_not_found", "model is not configured", "")
		return
	}
	// 对话响应仍保留 OpenAI 格式和流式传输，登记与容量由统一账本决定。
	var fields map[string]json.RawMessage
	select {
	case s.uploads <- struct{}{}:
	case <-ctx.Done():
		return
	}
	body, ok := readBody(c, cfg.MaxBodyBytes, &fields, "")
	<-s.uploads
	if !ok {
		return
	}
	var model string
	// model 保持真实上游标识，是否使用对话协议由登记决定。
	if err := json.Unmarshal(fields["model"], &model); err != nil {
		writeError(c, 404, "model_not_found", "model is not configured", "")
		return
	}

	kind, ok := s.resolveModelProtocol(ctx, c, model)
	if !ok {
		return
	}
	if kind != protocol.ChatProtocol {
		writeError(c, 400, "invalid_protocol", "model does not use chat completions", "")
		return
	}
	s.forwardSynchronous(ctx, c, s.direct, model, kind, chatCompletionsPath, "application/json", body)
}
