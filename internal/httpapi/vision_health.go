package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
)

type visionServiceHealth struct {
	Ready *bool `json:"ready"`
}

type visionModelHealth struct {
	ModelKey string `json:"model_key"`
	Ready    *bool  `json:"ready"`
}

func (s *Server) visionHealth(ctx context.Context, c *app.RequestContext) {
	addr := requestConfig(ctx).VisionAddr
	models := make([]visionModelHealth, 0, len(visionRoutes))
	if addr != "" {
		services := s.readVisionHealth(ctx, addr)
		for _, route := range visionRoutes {
			// 只展示有 ModelHub 模型标识的推理路由。两条分割路由共用同一个上游探活结果。
			if route.modelKey == "" {
				continue
			}
			service, _, _ := strings.Cut(strings.TrimPrefix(route.path, "/"), "/")
			models = append(models, visionModelHealth{ModelKey: route.modelKey, Ready: services[service].Ready})
		}
	}
	c.Header("Cache-Control", "no-store")
	writeJSON(c, http.StatusOK, models)
}

func (s *Server) readVisionHealth(ctx context.Context, addr string) map[string]visionServiceHealth {
	// G6 的聚合探活分三批执行，最慢的一轮约需 20 秒。
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return nil
	}
	resp, err := s.direct.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	// 聚合状态为 503 时，G6 仍返回每个服务的独立结果。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return nil
	}
	var health struct {
		Services map[string]visionServiceHealth `json:"services"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&health); err != nil {
		return nil
	}
	return health.Services
}
