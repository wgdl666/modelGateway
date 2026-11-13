package httpapi

import (
	"context"
	"github.com/cloudwego/hertz/pkg/app"
)

func (s *Server) deploymentDomain(ctx context.Context, c *app.RequestContext) {
	region, err := s.store.DeploymentDomain(ctx)
	if err != nil {
		storeError(c, err, "")
		return
	}
	writeJSON(c, 200, map[string]any{"region": region, "lifecycle_protocol": 1})
}
