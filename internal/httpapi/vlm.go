package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
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
	if err := json.Unmarshal(fields["model"], &model); err != nil || model != "Vmind_dream_v1" {
		writeError(c, 404, "model_not_found", "model is not configured", "")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cfg.VLMAddr+chatCompletionsPath, bytes.NewReader(body))
	if err != nil {
		writeError(c, 502, "upstream_unavailable", "VLM request could not be sent", "")
		return
	}
	// 业务凭据只在网关校验，不转发给模型服务。
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.vlm.Do(req)
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(c, 504, "upstream_timeout", "VLM response timed out", "")
		return
	}
	if err != nil {
		writeError(c, 502, "upstream_unavailable", "VLM request could not be sent", "")
		return
	}
	c.SetStatusCode(resp.StatusCode)
	c.Header("Content-Type", resp.Header.Get("Content-Type"))
	c.Response.SetBodyStream(resp.Body, int(resp.ContentLength))
}
