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
	// 网关用上游的 served-model-name 校验请求，调用方使用同一个发布标识。
	if err := json.Unmarshal(fields["model"], &model); err != nil || model != protocol.DreamModel {
		writeError(c, 404, "model_not_found", "model is not configured", "")
		return
	}

	s.forwardSynchronous(ctx, c, s.direct, protocol.DreamModel, chatCompletionsPath, "application/json", body)
}
