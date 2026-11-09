package httpapi

import (
	"context"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

type requestView struct {
	protocol.Response
	Capability    string     `json:"capability"`
	TraceID       string     `json:"trace_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	InstanceID    string     `json:"instance_id,omitempty"`
	ResourceGroup string     `json:"resource_group,omitempty"`
	Artifact      string     `json:"artifact_digest,omitempty"`
	BackendTaskID string     `json:"backend_task_id,omitempty"`
	SlotReserved  bool       `json:"slot_reserved"`
}

func viewRequest(row *ent.Request) requestView {
	return requestView{
		Response: taskResponse(row), Capability: row.Capability, TraceID: row.TraceID,
		CreatedAt: row.CreatedAt, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt,
		InstanceID: row.InstanceID, ResourceGroup: row.ResourceGroup, Artifact: row.ArtifactDigest,
		BackendTaskID: row.BackendTaskID, SlotReserved: row.SlotReserved,
	}
}

func (s *Server) listRequests(ctx context.Context, c *app.RequestContext) {
	limit, errLimit := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, errOffset := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if errLimit != nil || errOffset != nil || limit < 1 || limit > 100 || offset < 0 {
		writeError(c, 400, "invalid_pagination", "invalid request pagination", "")
		return
	}
	filter := store.RequestFilter{
		ID: c.Query("request_id"), TraceID: c.Query("trace_id"), Model: c.Query("model"),
		InstanceID: c.Query("instance_id"), Artifact: c.Query("artifact_digest"),
		Limit: limit, Offset: offset, State: request.State(c.Query("state")),
	}
	if filter.ID != "" && !protocol.ValidID(filter.ID) {
		writeError(c, 400, "invalid_request_id", "request_id must be a UUID", "")
		return
	}
	if filter.State != "" {
		if err := request.StateValidator(filter.State); err != nil {
			writeError(c, 400, "invalid_state", "invalid request state", "")
			return
		}
	}
	for _, key := range []string{"from", "to"} {
		if value := c.Query(key); value != "" {
			bound, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(c, 400, "invalid_time", "request time range must use RFC3339", "")
				return
			}
			if key == "from" {
				filter.From = &bound
			} else {
				filter.To = &bound
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, total, err := s.store.ListRequests(ctx, filter)
	if err != nil {
		storeError(c, err, "")
		return
	}
	items := make([]requestView, 0, len(rows))
	for _, row := range rows {
		items = append(items, viewRequest(row))
	}
	writeJSON(c, 200, map[string]any{"items": items, "total": total})
}

func (s *Server) requestDetail(ctx context.Context, c *app.RequestContext) {
	id := c.Param("id")
	if !protocol.ValidID(id) {
		writeError(c, 400, "invalid_request_id", "request_id must be a UUID", "")
		return
	}
	row, err := s.store.RequestDetail(ctx, id)
	if err != nil {
		storeError(c, err, id)
		return
	}
	writeJSON(c, 200, viewRequest(row))
}
