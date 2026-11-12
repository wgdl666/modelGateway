package store

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/ent/resourcegroup"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// Admission 携带本次受理的队列限制，任务入库后沿用当时计算的截止时间。
type Admission struct {
	ID, ModelKey, Protocol string
	TraceID                string
	Payload                json.RawMessage
	Input                  json.RawMessage
	InputMedia             json.RawMessage
	Units                  int
	MaxWaiting             int
	BackendTimeout         time.Duration
}

func sameInput(payload, input json.RawMessage) bool {
	var stored protocol.Submission
	var left, right any
	return json.Unmarshal(payload, &stored) == nil &&
		json.Unmarshal(stored.Input, &left) == nil &&
		json.Unmarshal(input, &right) == nil && reflect.DeepEqual(left, right)
}

func (s *Store) Admit(ctx context.Context, in Admission) (*ent.Request, error) {
	var task *ent.Request
	err := s.serialize(ctx, func(tx *ent.Tx) error {
		now := time.Now().UTC()
		old, err := tx.Request.Get(ctx, in.ID)
		if err == nil {
			// 同步流无法重放。重复键明确冲突，不允许另一连接接管或再次执行。
			if protocol.SynchronousKind(in.Protocol) != "" {
				return ErrConflict
			}
			if old.ModelKey != in.ModelKey || old.Protocol != in.Protocol || !sameInput(old.RequestPayload, in.Input) {
				return ErrConflict
			}
			task = old
			return nil
		}
		if !ent.IsNotFound(err) {
			return err
		}
		// 登记与受理共用串行事务，写队列前再次核对模型及协议，避免校验与落库之间变化。
		registered := tx.ModelInstance.Query().Where(modelinstance.ModelKeyEQ(in.ModelKey))
		if protocol.SynchronousKind(in.Protocol) != "" {
			registered.Where(modelinstance.DeletedAtIsNil())
		}
		model, err := registered.Select(modelinstance.FieldProtocol).First(ctx)
		if ent.IsNotFound(err) {
			return ErrModelUnregistered
		}
		if err != nil {
			return err
		}
		if model.Protocol != in.Protocol {
			return ErrConflict
		}
		// 实例短时下线时仍可接单。派发再查健康和可用容量，超过原截止时间的任务按排队超时结束。
		// 同模型只按等待条数限流，可串行化事务避免多个副本同时接单超额。
		waiting, err := tx.Request.Query().Where(
			request.ModelKeyEQ(in.ModelKey), request.StateEQ(request.StateQueued), request.DeadlineAtGT(now),
		).Count(ctx)
		if err != nil {
			return err
		}
		if waiting >= in.MaxWaiting {
			return ErrFull
		}
		create := tx.Request.Create().SetID(in.ID).SetModelKey(in.ModelKey).SetProtocol(in.Protocol).
			SetTraceID(in.TraceID).SetRequestPayload(in.Payload).SetPayloadBytes(int64(len(in.Payload))).SetUnits(in.Units).
			SetCreatedAt(now).SetDeadlineAt(now.Add(in.BackendTimeout))
		if in.InputMedia != nil {
			create.SetInputMedia(in.InputMedia)
		}
		// 同步响应由原连接读取，容量仍在 PG 竞争；不能再投 MQ 产生第二次模型执行。
		if protocol.SynchronousKind(in.Protocol) == "" {
			create.SetQueuePublishAt(now)
		}
		task, err = create.Save(ctx)
		return err
	})
	return task, err
}

func (s *Store) PendingPublications(ctx context.Context) ([]*ent.Request, error) {
	now := time.Now().UTC()
	return s.db.Request.Query().Where(request.StateEQ(request.StateQueued), request.QueuePublishAtLTE(now), request.DeadlineAtGT(now)).
		Order(ent.Asc(request.FieldQueuePublishAt, request.FieldID)).Limit(32).
		Select(request.FieldID, request.FieldQueuePublishAt, request.FieldDeadlineAt).All(ctx)
}

func (s *Store) MarkPublished(ctx context.Context, id string, due time.Time) error {
	// 消费者可能已把同一请求改为稍后再投，迟到的发布确认不能清掉新的投递时间。
	_, err := s.db.Request.Update().Where(request.IDEQ(id), request.QueuePublishAtEQ(due)).ClearQueuePublishAt().Save(ctx)
	return err
}

type reservation struct {
	InstanceID     string
	ResourceGroup  string
	ResourceGroups []string
	Endpoint       string
	Units          int
	Requests       int
}

// 派发与 Ops 查询共用容量占用口径，等待结果确认的任务继续占用槽位。
func reserved(ctx context.Context, client *ent.RequestClient) ([]reservation, error) {
	rows, err := client.Query().Where(request.SlotReservedEQ(true)).Select(request.FieldInstanceID,
		request.FieldResourceGroup, request.FieldResourceGroups, request.FieldEndpoint, request.FieldUnits).All(ctx)
	if err != nil {
		return nil, err
	}
	occupied := make([]reservation, 0, len(rows))
	for _, row := range rows {
		groups := row.ResourceGroups
		if len(groups) == 0 {
			// 滚动发布中的旧网关只会写单卡请求，沿用其冻结的主组直到旧副本退出。
			groups = []string{row.ResourceGroup}
		}
		occupied = append(occupied, reservation{InstanceID: row.InstanceID, ResourceGroup: row.ResourceGroup,
			ResourceGroups: groups, Endpoint: row.Endpoint, Units: row.Units, Requests: 1})
	}
	return occupied, nil
}

func (s *Store) Claim(ctx context.Context, cfg config.Config, id string) (*ent.Request, error) {
	var task *ent.Request
	err := s.serialize(ctx, func(tx *ent.Tx) error {
		task = nil
		now := time.Now().UTC()
		// 同一消息可能重投。按 ID 等待行锁，锁竞争不能被误当成已处理而确认消息。
		next, err := tx.Request.Query().Where(request.IDEQ(id), request.StateEQ(request.StateQueued), request.DeadlineAtGT(now)).
			Select(request.FieldID, request.FieldModelKey, request.FieldProtocol, request.FieldUnits, request.FieldDeadlineAt, request.FieldQueuePublishAt).
			ForUpdate().Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		// 旧消息可能在回队后重投。沿用延期写入新的时间标记，防止迟到的发布确认清掉下次投递。
		if next.QueuePublishAt != nil && next.QueuePublishAt.After(now) {
			return deferPublication(ctx, tx, next)
		}
		// 同键不同业务的旧登记在迁移时已归档，派发直接按请求的模型键选实例。
		instances, err := tx.ModelInstance.Query().Where(
			modelinstance.DeletedAtIsNil(), modelinstance.ModelKeyEQ(next.ModelKey),
			modelinstance.ReadyEQ(true), modelinstance.DrainingEQ(false),
		).Order(ent.Asc(modelinstance.FieldID)).All(ctx)
		if err != nil {
			return err
		}
		if len(instances) == 0 {
			if protocol.SynchronousKind(next.Protocol) != "" {
				return nil
			}
			return deferPublication(ctx, tx, next)
		}
		// 同一事务锁定物理卡的容量，多卡实例必须一次拿齐全部额度。
		groups, err := tx.ResourceGroup.Query().Order(resourcegroup.ByID()).ForShare().All(ctx)
		if err != nil {
			return err
		}
		limits := make(map[string]int, len(groups))
		for _, group := range groups {
			limits[group.ID] = group.CapacityUnits
		}
		gpuGroups := indexGPUResourceGroups(groups)
		occupied, err := reserved(ctx, tx.Request)
		if err != nil {
			return err
		}
		byExecution, byGroup := map[string]int{}, map[string]int{}
		for _, hold := range occupied {
			// 人物/主体抠图等不同模型键共用执行端，不能各自获得一份独立并发。
			byExecution[hold.ResourceGroup+"\x00"+hold.Endpoint] += hold.Units
			for _, group := range hold.ResourceGroups {
				byGroup[group] += hold.Units
			}
		}
		for _, i := range instances {
			// 地址退出原生配置后，旧探活快照不能让它按 Worker 协议接单。
			if !instanceHealthy(i, now) || !cfg.AcceptsInstanceAddress(i.ModelKey, i.Protocol, i.Endpoint, i.HealthURL) {
				continue
			}
			memberships := []string{i.ResourceGroup}
			if len(i.GpuDevices) > 1 {
				memberships = gpuGroups.instanceGroups(i)
				if len(memberships) != len(i.GpuDevices) {
					continue
				}
			}
			free := i.CapacityUnits - byExecution[i.ResourceGroup+"\x00"+i.Endpoint]
			for _, group := range memberships {
				free = min(free, limits[group]-byGroup[group])
			}
			if free < next.Units {
				continue
			}
			// 与排空串行化，提交后请求身份和物理槽位才对其他副本可见。
			if _, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(i.ID)).ForUpdate().Only(ctx); err != nil {
				return err
			}
			nextPoll := next.DeadlineAt.Add(2 * time.Second)
			if _, direct := cfg.NativeBackendFor(next.ModelKey, i.Endpoint); direct || protocol.SynchronousKind(i.Protocol) != "" {
				// 原生调用可晚于上游返回，给原派发者留足执行和写终态的时间。
				nextPoll = now.Add(cfg.NativeExecutionTimeout).Add(ExecutionWriteTimeout)
			}
			// 回队后会生成新的领取标识，旧一轮的响应不能写入后续重试。
			task, err = tx.Request.UpdateOneID(id).SetState(request.StateProcessing).SetSlotReserved(true).ClearQueuePublishAt().
				SetInstanceID(i.ID).SetResourceGroup(i.ResourceGroup).SetResourceGroups(memberships).SetEndpoint(i.Endpoint).SetArtifactDigest(i.ArtifactDigest).
				AddDispatchAttempts(1).SetStartedAt(now).SetNextPollAt(nextPoll).SetPollClaim(uuid.NewString()).Save(ctx)
			return err
		}
		if protocol.SynchronousKind(next.Protocol) != "" {
			return nil
		}
		return deferPublication(ctx, tx, next)
	})
	return task, err
}

func deferPublication(ctx context.Context, tx *ent.Tx, task *ent.Request) error {
	// 槽位暂满时先持久化下次投递再确认消息，消费者可以继续处理其他模型。
	due := time.Now().UTC().Add(500 * time.Millisecond).Truncate(time.Microsecond)
	// 时间也用来识别投递轮次。副本时钟有偏差时，延期仍须写入一个更晚的值。
	if task.QueuePublishAt != nil && !due.After(*task.QueuePublishAt) {
		due = task.QueuePublishAt.Add(time.Microsecond)
	}
	_, err := tx.Request.Update().Where(request.IDEQ(task.ID)).SetQueuePublishAt(due).Save(ctx)
	return err
}

func (s *Store) RequeueDispatch(ctx context.Context, id string, startedAt time.Time, claim string, reasons ...string) error {
	reason := "dispatch_outcome_unknown"
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	// 忙、断线和调用超时统一延期回队。只撤销本轮领取，不重置总截止时间或重派已登记的异步任务。
	due := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	_, err := s.db.Request.Update().Where(request.IDEQ(id), request.StateEQ(request.StateProcessing),
		request.SlotReservedEQ(true), request.StartedAtEQ(startedAt), request.PollClaimEQ(claim), request.BackendTaskIDEQ("")).
		SetState(request.StateQueued).SetSlotReserved(false).SetQueuePublishAt(due).SetDispatchRetryReason(reason).
		SetInstanceID("").SetResourceGroup("").SetResourceGroups([]string{}).SetEndpoint("").SetArtifactDigest("").
		ClearStartedAt().ClearNextPollAt().SetPollClaim("").Save(ctx)
	return err
}

func (s *Store) Cancel(ctx context.Context, id string) error {
	n, err := s.db.Request.Update().Where(request.IDEQ(id), request.StateEQ(request.StateQueued)).
		SetState(request.StateFailed).SetErrorCode("task_cancelled").SetErrorMessage("request cancelled before dispatch").
		SetErrorStatus(409).SetFinishedAt(time.Now().UTC()).Save(ctx)
	if err != nil || n > 0 {
		return err
	}
	found, err := s.db.Request.Query().Where(request.IDEQ(id)).Exist(ctx)
	if err != nil {
		return err
	}
	if !found {
		return &ent.NotFoundError{}
	}
	return ErrConflict
}

func (s *Store) Expire(ctx context.Context) error {
	now := time.Now().UTC()
	_, err := s.db.Request.Update().Where(request.StateEQ(request.StateQueued), request.DeadlineAtLTE(now)).
		SetState(request.StateFailed).SetErrorCode("queue_timeout").SetErrorMessage("request expired before dispatch").
		SetErrorStatus(504).SetFinishedAt(now).Save(ctx)
	if err != nil {
		return err
	}
	// 同步执行的进程消失后无法回查上游任务，明确标记结果未知并保留容量，禁止静默重复执行。
	_, err = s.db.Request.Update().Where(request.ProtocolIn(protocol.SynchronousProtocols()...),
		request.StateEQ(request.StateProcessing), request.SlotReservedEQ(true), request.NextPollAtLTE(now)).
		SetState(request.StateFailed).SetErrorCode("backend_outcome_unknown").SetErrorMessage("synchronous model outcome is unknown").
		SetErrorStatus(504).SetFinishedAt(now).ClearNextPollAt().Save(ctx)
	return err
}
