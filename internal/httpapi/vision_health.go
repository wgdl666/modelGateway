package httpapi

import (
	"context"
	"net/http"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/upstreamhealth"
)

type visionModelHealth struct {
	ModelKey string `json:"model_key"`
	Ready    *bool  `json:"ready"`
}

func (s *Server) visionHealth(ctx context.Context, c *app.RequestContext) {
	addr := requestConfig(ctx).VisionAddr
	models := make([]visionModelHealth, 0, len(visionRoutes))
	if addr != "" {
		services := upstreamhealth.ReadVision(ctx, s.direct, "http://"+addr+"/health")
		for _, route := range visionRoutes {
			// 只展示有 ModelHub 模型标识的推理路由。两条分割路由共用同一个上游探活结果。
			if route.modelKey == "" {
				continue
			}
			service := protocol.VisionService(route.modelKey)
			models = append(models, visionModelHealth{ModelKey: route.modelKey, Ready: services[service].Ready})
		}
	}
	c.Header("Cache-Control", "no-store")
	writeJSON(c, http.StatusOK, models)
}
