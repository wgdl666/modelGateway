package httpapi

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func (s *Server) editInstance(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	var settings protocol.InstanceSettings
	if _, ok := readBody(c, 16<<10, &settings, ""); !ok {
		return
	}
	settings.Endpoint = strings.TrimRight(strings.TrimSpace(settings.Endpoint), "/")
	settings.HealthURL = strings.TrimSpace(settings.HealthURL)
	cfg := requestConfig(ctx)
	// 新地址沿用登记的私网白名单；模型专属协议由锁内当前登记决定。
	if !protocol.ValidInstanceID(id) || !protocol.ValidResourceGroupID(settings.ResourceGroup) ||
		settings.Capacity < 1 || settings.Capacity > protocol.MaxCapacityUnits || settings.ConfigRevision == "" ||
		cfg.ValidateEndpoint(settings.Endpoint, false) != nil || cfg.ValidateEndpoint(settings.HealthURL, true) != nil {
		writeError(c, 400, "invalid_instance", "invalid instance settings", "")
		return
	}
	err := s.store.EditInstance(ctx, cfg.Config, id, settings)
	switch {
	case err == nil:
		writeJSON(c, 200, map[string]string{"instance_id": id})
	case errors.Is(err, store.ErrInstanceAddress):
		writeError(c, 400, "invalid_instance_address", err.Error(), "")
	case errors.Is(err, store.ErrInstanceNotEditable):
		writeError(c, 409, "instance_not_editable", err.Error(), "")
	case errors.Is(err, store.ErrInstanceChanged):
		writeError(c, 409, "instance_changed", err.Error(), "")
	case errors.Is(err, store.ErrInstanceDestination):
		writeError(c, 409, "instance_destination_conflict", err.Error(), "")
	default:
		storeError(c, err, "")
	}
}
