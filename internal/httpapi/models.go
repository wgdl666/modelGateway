package httpapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/inputschema"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func (s *Server) resolveModelDefinition(ctx context.Context, c *app.RequestContext, model string) (store.ModelDefinition, bool) {
	if !protocol.ValidModelKey(model) {
		writeError(c, 404, "model_not_found", "invalid model key", "")
		return store.ModelDefinition{}, false
	}
	definition, err := s.store.ModelDefinition(ctx, model)
	if errors.Is(err, store.ErrModelUnregistered) {
		writeError(c, 404, "model_not_found", "model has no registration", "")
		return definition, false
	}
	if errors.Is(err, store.ErrInputSchemaMissing) {
		writeError(c, 503, "model_schema_unavailable", err.Error(), "")
		return definition, false
	}
	if err != nil {
		storeError(c, err, "")
		return definition, false
	}
	if err := inputschema.CheckDefinition(definition.InputSchema); err != nil {
		writeError(c, 503, "model_schema_invalid", "model input_schema is invalid", "")
		return definition, false
	}
	return definition, true
}

func modelView(definition store.ModelDefinition, experimental, detail bool) (map[string]any, error) {
	raw, err := inputschema.Effective(definition.InputSchema, experimental)
	if err != nil {
		return nil, err
	}
	var meta struct {
		Description string `json:"description"`
		Category    string `json:"x-category"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	responseMode := "direct"
	if protocol.MediaProtocol(definition.Protocol) {
		responseMode = "task"
	}
	view := map[string]any{"model_key": definition.ModelKey, "protocol": definition.Protocol, "description": meta.Description, "category": meta.Category, "response_mode": responseMode}
	if detail {
		view["input_schema"] = raw
	}
	return view, nil
}

// 模型目录按登记去重，实例健康与容量不属于调用方契约。
func (s *Server) models(ctx context.Context, c *app.RequestContext) {
	definitions, err := s.store.ListModelDefinitions(ctx)
	if err != nil {
		storeError(c, err, "")
		return
	}
	items := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		view, err := modelView(definition, c.GetBool(experimentalCredential), false)
		if err != nil {
			writeError(c, 503, "model_schema_invalid", "invalid model definition", "")
			return
		}
		if category := c.Query("category"); category == "" || view["category"] == category {
			items = append(items, view)
		}
	}
	c.Header("Cache-Control", "no-store")
	writeJSON(c, 200, map[string]any{"models": items})
}

func (s *Server) setModelSchema(ctx context.Context, c *app.RequestContext) {
	model := c.Param("model_key")
	if !protocol.ValidModelKey(model) {
		writeError(c, 400, "invalid_model", "invalid model key", "")
		return
	}
	var body struct {
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if _, ok := readBody(c, 256<<10, &body, ""); !ok {
		return
	}
	if err := inputschema.CheckDefinition(body.InputSchema); err != nil {
		writeError(c, 400, "invalid_schema", err.Error(), "")
		return
	}
	if err := s.store.SetModelSchema(ctx, model, body.InputSchema); err != nil {
		if errors.Is(err, store.ErrModelUnregistered) {
			writeError(c, 404, "model_not_found", err.Error(), "")
			return
		}
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]bool{"updated": true})
}
