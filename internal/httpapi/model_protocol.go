package httpapi

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

// 在受理边界解析登记协议；实例健康与空闲容量仍在派发时判断，不能混同模型不存在。
func (s *Server) resolveModelProtocol(ctx context.Context, c *app.RequestContext, model string) (string, bool) {
	if !protocol.ValidModelKey(model) {
		writeError(c, 404, "model_not_found", "invalid model key", "")
		return "", false
	}
	kind, err := s.store.ModelProtocol(ctx, model)
	if errors.Is(err, store.ErrModelUnregistered) {
		writeError(c, 404, "model_not_found", "model has no registration", "")
		return "", false
	}
	if err != nil {
		storeError(c, err, "")
		return "", false
	}
	return kind, true
}

func (s *Server) modelProtocol(ctx context.Context, c *app.RequestContext) {
	definition, ok := s.resolveModelDefinition(ctx, c, c.Param("model_key"))
	if !ok {
		return
	}
	view, err := modelView(definition, c.GetBool(experimentalCredential), true)
	if err != nil {
		writeError(c, 503, "model_schema_invalid", "invalid model definition", "")
		return
	}
	c.Header("Cache-Control", "no-store")
	writeJSON(c, 200, view)
}
