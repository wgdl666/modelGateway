package httpapi

import (
	"context"
	"mime"

	"github.com/cloudwego/hertz/pkg/app"
)

const decisionPredictPath = "/v1/decision/predict"

func (s *Server) decisionPredict(ctx context.Context, c *app.RequestContext) {
	cfg := requestConfig(ctx)
	if cfg.DecisionAddr == "" {
		writeError(c, 404, "model_not_found", "model is not configured", "")
		return
	}
	contentType := string(c.GetHeader("Content-Type"))
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		c.SetConnectionClose()
		writeError(c, 415, "unsupported_media_type", "request must use multipart/form-data with a boundary", "")
		return
	}
	// 与其他上传共用限额，保留图片和任务字段原样交给 Decision 校验。
	select {
	case s.uploads <- struct{}{}:
	case <-ctx.Done():
		return
	}
	body, err := readPayload(c, cfg.MaxBodyBytes)
	<-s.uploads
	if int64(len(body)) > cfg.MaxBodyBytes {
		c.SetConnectionClose()
		writeError(c, 413, "request_too_large", "request exceeds its size limit", "")
		return
	}
	if err != nil {
		c.SetConnectionClose()
		writeError(c, 400, "invalid_multipart", "could not read multipart request", "")
		return
	}
	s.forwardDirect(ctx, c, "Decision", "http://"+cfg.DecisionAddr+"/v1/predict", contentType, body)
}
