package store

import (
	"context"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
)

// 等待响应时只取响应字段和截止时间，避免反复读取入队保存的大图。
func (s *Store) Get(ctx context.Context, id string) (*ent.Request, error) {
	return s.db.Request.Query().Where(request.IDEQ(id)).Select(
		request.FieldID, request.FieldModelKey, request.FieldState, request.FieldInstanceID,
		request.FieldDeadlineAt, request.FieldResult, request.FieldErrorCode, request.FieldErrorMessage, request.FieldErrorStatus,
	).Only(ctx)
}

type InstanceView struct {
	*ent.ModelInstance
	ConfigRevision   string `json:"config_revision"`
	ReservedUnits    int    `json:"reserved_units"`
	ReservedRequests int    `json:"reserved_requests"`
	Healthy          bool   `json:"healthy"`
	Eligible         bool   `json:"eligible"`
	Clearable        bool   `json:"clearable,omitempty"`
}

func (s *Store) ListInstances(ctx context.Context) ([]InstanceView, error) {
	rows, err := s.db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, err
	}
	limits, err := groupCapacities(ctx, s.db.ResourceGroup.Query())
	if err != nil {
		return nil, err
	}
	holds, err := reserved(ctx, s.db.Request)
	if err != nil {
		return nil, err
	}
	units := make(map[string]int)
	requests := make(map[string]int)
	for _, hold := range holds {
		units[hold.InstanceID] += hold.Units
		requests[hold.InstanceID] += hold.Requests
	}
	result := make([]InstanceView, 0, len(rows))
	now := time.Now()
	for _, row := range rows {
		// 保持实例查询的字段契约，组容量显示调度当前使用的值。
		row.GroupCapacityUnits = limits[row.ResourceGroup]
		// 预留单位和请求数分别返回，避免把加权容量显示为在途任务数。
		healthy := instanceHealthy(row, now)
		result = append(result, InstanceView{
			ModelInstance: row, ReservedUnits: units[row.ID], ReservedRequests: requests[row.ID],
			// 暂停后不再写探活，更新时间可识别同一份编辑草稿。
			ConfigRevision: row.UpdatedAt.UTC().Format(time.RFC3339Nano),
			Healthy:        healthy, Eligible: healthy && !row.Draining,
			Clearable: instanceClearable(row, healthy) && requests[row.ID] == 0,
		})
	}
	return result, nil
}

type RequestFilter struct {
	ID, TraceID, Model, InstanceID, Artifact string
	State                                    request.State
	From, To                                 *time.Time
	Offset, Limit                            int
}

// 运维查询只读取执行归属与状态，原始媒体仍留在队列账本里。
var requestSummaryFields = []string{
	request.FieldID, request.FieldModelKey, request.FieldTraceID,
	request.FieldState, request.FieldCreatedAt, request.FieldStartedAt, request.FieldFinishedAt,
	request.FieldInstanceID, request.FieldResourceGroup, request.FieldArtifactDigest,
	request.FieldBackendTaskID, request.FieldSlotReserved, request.FieldErrorCode,
}

func (s *Store) ListRequests(ctx context.Context, filter RequestFilter) ([]*ent.Request, int, error) {
	query := s.db.Request.Query()
	if filter.ID != "" {
		query.Where(request.IDEQ(filter.ID))
	}
	if filter.TraceID != "" {
		query.Where(request.TraceIDEQ(filter.TraceID))
	}
	if filter.Model != "" {
		// 使用调用时的完整模型键，避免不同版本的请求被归到同一组。
		query.Where(request.ModelKeyEQ(filter.Model))
	}
	if filter.InstanceID != "" {
		query.Where(request.InstanceIDEQ(filter.InstanceID))
	}
	if filter.Artifact != "" {
		query.Where(request.ArtifactDigestEQ(filter.Artifact))
	}
	if filter.State != "" {
		query.Where(request.StateEQ(filter.State))
	}
	if filter.From != nil {
		query.Where(request.CreatedAtGTE(*filter.From))
	}
	if filter.To != nil {
		query.Where(request.CreatedAtLTE(*filter.To))
	}
	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := query.Order(request.ByCreatedAt(entsql.OrderDesc()), request.ByID(entsql.OrderDesc())).
		Offset(filter.Offset).Limit(filter.Limit).Select(requestSummaryFields...).All(ctx)
	return rows, total, err
}

func (s *Store) RequestDetail(ctx context.Context, id string) (*ent.Request, error) {
	// 详情只补对象引用和终态错误，禁止将 Base64 输入带到 Ops。
	fields := append(append([]string(nil), requestSummaryFields...), request.FieldResult, request.FieldErrorMessage)
	return s.db.Request.Query().Where(request.IDEQ(id)).Select(fields...).Only(ctx)
}
