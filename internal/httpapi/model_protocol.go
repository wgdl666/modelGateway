package httpapi

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

// 查询已有登记的协议供调用方选择适配器；可用性仍由独立的实例探活与派发判断。
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
	model := c.Param("model_key")
	if kind, ok := s.resolveModelProtocol(ctx, c, model); ok {
		writeJSON(c, 200, map[string]string{"model_key": model, "protocol": kind})
	}
}
