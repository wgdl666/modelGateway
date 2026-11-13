package store

import (
	"context"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// 等待响应时只取响应字段和截止时间，避免反复读取入队保存的大图。
func (s *Store) Get(ctx context.Context, id string) (*ent.Request, error) {
	return s.db.Request.Query().Where(request.IDEQ(id)).Select(
		request.FieldID, request.FieldModelKey, request.FieldProtocol, request.FieldState, request.FieldInstanceID, request.FieldTraceID,
		request.FieldDispatchAttempts, request.FieldDispatchRetryReason, request.FieldDeadlineAt, request.FieldResult, request.FieldErrorCode, request.FieldErrorMessage, request.FieldErrorStatus,
	).Only(ctx)
}

type InstanceView struct {
	*ent.ModelInstance
	ConfigRevision   string   `json:"config_revision"`
	ReservedUnits    int      `json:"reserved_units"`
	ReservedRequests int      `json:"reserved_requests"`
	Healthy          bool     `json:"healthy"`
	Eligible         bool     `json:"eligible"`
	Clearable        bool     `json:"clearable,omitempty"`
	Synchronous      bool     `json:"synchronous,omitempty"`
	ResourceGroups   []string `json:"resource_groups,omitempty"`
}

func (s *Store) ListInstances(ctx context.Context) ([]InstanceView, error) {
	rows, err := s.db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil()).All(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := s.db.ResourceGroup.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	limits := make(map[string]int, len(groups))
	for _, group := range groups {
		limits[group.ID] = group.CapacityUnits
	}
	gpuGroups := indexGPUResourceGroups(groups)
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
		synchronous := protocol.SynchronousKind(row.Protocol) != ""
		var memberships []string
		if synchronous {
			memberships = gpuGroups.instanceGroups(row)
		}
		result = append(result, InstanceView{
			Synchronous: synchronous, ResourceGroups: memberships,
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
	request.FieldID, request.FieldModelKey, request.FieldProtocol, request.FieldTraceID,
	request.FieldState, request.FieldCreatedAt, request.FieldStartedAt, request.FieldFinishedAt,
	request.FieldInstanceID, request.FieldResourceGroup, request.FieldResourceGroups, request.FieldEndpoint, request.FieldArtifactDigest,
	request.FieldBackendTaskID, request.FieldSlotReserved, request.FieldErrorCode, request.FieldDeploymentID, request.FieldBootID,
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
	// 列表带出输入清单与结果引用供 Ops 展示，二者都不含图片字节。
	fields := append(append([]string(nil), requestSummaryFields...), request.FieldExtra, request.FieldResult)
	rows, err := query.Order(request.ByCreatedAt(entsql.OrderDesc()), request.ByID(entsql.OrderDesc())).
		Offset(filter.Offset).Limit(filter.Limit).Select(fields...).All(ctx)
	return rows, total, err
}

func (s *Store) RequestDetail(ctx context.Context, id string) (*ent.Request, error) {
	// 详情读取受理正文，以便返回去掉图片字节的业务参数。列表仍然不读取这列。
	fields := append(append([]string(nil), requestSummaryFields...),
		request.FieldRequestPayload, request.FieldResult, request.FieldErrorMessage, request.FieldDispatchAttempts, request.FieldDispatchRetryReason)
	return s.db.Request.Query().Where(request.IDEQ(id)).Select(fields...).Only(ctx)
}

// RequestOutput 仅加载成品引用，避免输出预览读取完整 Base64 入参。
func (s *Store) RequestOutput(ctx context.Context, id string) (*ent.Request, error) {
	return s.db.Request.Query().Where(request.IDEQ(id)).Select(request.FieldID, request.FieldResult).Only(ctx)
}
