package gateway

import (
	"context"
	"strconv"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
)

// 运维查询只读取执行归属与状态，原始媒体仍留在队列账本里。
var requestSummaryFields = []string{
	request.FieldID, request.FieldModelKey, request.FieldCapability, request.FieldTraceID,
	request.FieldState, request.FieldCreatedAt, request.FieldStartedAt, request.FieldFinishedAt,
	request.FieldInstanceID, request.FieldResourceGroup, request.FieldArtifactDigest,
	request.FieldBackendTaskID, request.FieldSlotReserved, request.FieldErrorCode,
}

type requestView struct {
	Response
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
	query := s.store.DB.Request.Query()
	if id := c.Query("request_id"); id != "" {
		if !validID(id) {
			writeError(c, 400, "invalid_request_id", "request_id must be a UUID", "")
			return
		}
		query.Where(request.IDEQ(id))
	}
	if value := c.Query("trace_id"); value != "" {
		query.Where(request.TraceIDEQ(value))
	}
	if value := c.Query("model"); value != "" {
		query.Where(request.Or(request.ModelKeyEQ(value), request.CapabilityEQ(value)))
	}
	if value := c.Query("instance_id"); value != "" {
		query.Where(request.InstanceIDEQ(value))
	}
	if value := c.Query("artifact_digest"); value != "" {
		query.Where(request.ArtifactDigestEQ(value))
	}
	if value := c.Query("state"); value != "" {
		state := request.State(value)
		if err := request.StateValidator(state); err != nil {
			writeError(c, 400, "invalid_state", "invalid request state", "")
			return
		}
		query.Where(request.StateEQ(state))
	}
	for _, key := range []string{"from", "to"} {
		if value := c.Query(key); value != "" {
			bound, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(c, 400, "invalid_time", "request time range must use RFC3339", "")
				return
			}
			if key == "from" {
				query.Where(request.CreatedAtGTE(bound))
			} else {
				query.Where(request.CreatedAtLTE(bound))
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	total, err := query.Clone().Count(ctx)
	if err != nil {
		storeError(c, err, "")
		return
	}
	rows, err := query.Order(request.ByCreatedAt(entsql.OrderDesc()), request.ByID(entsql.OrderDesc())).
		Offset(offset).Limit(limit).Select(requestSummaryFields...).All(ctx)
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
	if !validID(id) {
		writeError(c, 400, "invalid_request_id", "request_id must be a UUID", "")
		return
	}
	// 详情只补对象引用和终态错误，禁止将 Base64 输入带到 Ops。
	fields := append(append([]string(nil), requestSummaryFields...), request.FieldResult, request.FieldErrorMessage)
	row, err := s.store.DB.Request.Query().Where(request.IDEQ(id)).Select(fields...).Only(ctx)
	if err != nil {
		storeError(c, err, id)
		return
	}
	writeJSON(c, 200, viewRequest(row))
}
