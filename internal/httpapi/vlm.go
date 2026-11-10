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
	// 对话服务返回文本。保留 OpenAI 请求体和响应，避免转入只保存媒体文件的任务队列。
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

	forwardDirect(ctx, c, s.direct, "VLM", "http://"+cfg.VLMAddr+chatCompletionsPath, "application/json", body)
}
