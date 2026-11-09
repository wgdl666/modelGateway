package store

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// Admission 携带本次受理的队列限制，任务入库后沿用当时计算的截止时间。
type Admission struct {
	ID, ModelKey, Capability string
	TraceID                  string
	Payload                  json.RawMessage
	Input                    json.RawMessage
	Units                    int
	MaxWaiting               int
	BackendTimeout           time.Duration
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
			if old.ModelKey != in.ModelKey || !sameInput(old.RequestPayload, in.Input) {
				return ErrConflict
			}
			task = old
			return nil
		}
		if !ent.IsNotFound(err) {
			return err
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
		task, err = tx.Request.Create().SetID(in.ID).SetModelKey(in.ModelKey).SetCapability(in.Capability).
			SetTraceID(in.TraceID).SetRequestPayload(in.Payload).SetPayloadBytes(int64(len(in.Payload))).SetUnits(in.Units).
			SetCreatedAt(now).SetDeadlineAt(now.Add(in.BackendTimeout)).SetQueuePublishAt(now).Save(ctx)
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
	InstanceID    string `json:"instance_id"`
	ResourceGroup string `json:"resource_group"`
	Units         int    `json:"units"`
	Requests      int    `json:"requests"`
}

// 派发与 Ops 查询共用容量占用口径，等待结果确认的任务继续占用槽位。
func reserved(ctx context.Context, client *ent.RequestClient) ([]reservation, error) {
	var occupied []reservation
	err := client.Query().Where(request.SlotReservedEQ(true)).
		GroupBy(request.FieldInstanceID, request.FieldResourceGroup).
		Aggregate(ent.As(ent.Sum(request.FieldUnits), "units"), ent.As(ent.Count(), "requests")).Scan(ctx, &occupied)
	return occupied, err
}

func (s *Store) Claim(ctx context.Context, cfg config.Config, id string) (*ent.Request, *ent.ModelInstance, error) {
	var task *ent.Request
	var instance *ent.ModelInstance
	err := s.serialize(ctx, func(tx *ent.Tx) error {
		task, instance = nil, nil
		now := time.Now().UTC()
		// 同一消息可能重投。按 ID 等待行锁，锁竞争不能被误当成已处理而确认消息。
		next, err := tx.Request.Query().Where(request.IDEQ(id), request.StateEQ(request.StateQueued), request.DeadlineAtGT(now)).
			Select(request.FieldID, request.FieldModelKey, request.FieldCapability, request.FieldUnits, request.FieldDeadlineAt, request.FieldQueuePublishAt).
			ForUpdate().Only(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		instances, err := tx.ModelInstance.Query().Where(
			modelinstance.DeletedAtIsNil(), modelinstance.ModelKeyEQ(next.ModelKey), modelinstance.CapabilityEQ(next.Capability),
			modelinstance.ReadyEQ(true), modelinstance.DrainingEQ(false), modelinstance.HealthCheckedAtGT(now.Add(-probeMaxAge)),
		).Order(ent.Asc(modelinstance.FieldID)).All(ctx)
		if err != nil {
			return err
		}
		if len(instances) == 0 {
			return deferPublication(ctx, tx, next)
		}
		occupied, err := reserved(ctx, tx.Request)
		if err != nil {
			return err
		}
		byInstance, byGroup := map[string]int{}, map[string]int{}
		for _, hold := range occupied {
			byInstance[hold.InstanceID] += hold.Units
			byGroup[hold.ResourceGroup] += hold.Units
		}
		for _, i := range instances {
			// 旧登记仍保留执行账本，历史组合不再参与新派发。
			if !protocol.MatchesCapability(i.ModelKey, i.Capability) {
				continue
			}
			// 旧视频登记即使还留有就绪快照，也要先补齐目标才能预留 GPU。
			backend, direct := cfg.NativeBackends[i.Endpoint]
			if direct && !backend.Accepts(i.Capability, i.Endpoint, i.HealthURL, i.TargetInstance) {
				continue
			}
			free := min(i.CapacityUnits-byInstance[i.ID], i.GroupCapacityUnits-byGroup[i.ResourceGroup])
			if free < next.Units {
				continue
			}
			// 与排空串行化，提交后请求身份和物理槽位才对其他副本可见。
			if _, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(i.ID)).ForUpdate().Only(ctx); err != nil {
				return err
			}
			nextPoll := next.DeadlineAt.Add(2 * time.Second)
			if direct {
				// 原生调用可晚于上游返回，给原派发者留足执行和写终态的时间。
				nextPoll = now.Add(cfg.NativeExecutionTimeout).Add(ExecutionWriteTimeout)
			}
			task, err = tx.Request.UpdateOneID(id).SetState(request.StateProcessing).SetSlotReserved(true).ClearQueuePublishAt().
				SetInstanceID(i.ID).SetResourceGroup(i.ResourceGroup).SetEndpoint(i.Endpoint).SetArtifactDigest(i.ArtifactDigest).
				SetStartedAt(now).SetNextPollAt(nextPoll).Save(ctx)
			if err != nil {
				return err
			}
			// 将预留容量时选中的实例交给派发，LTX 目标与 GPU 分组来自同一条登记。
			instance = i
			return nil
		}
		return deferPublication(ctx, tx, next)
	})
	return task, instance, err
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
	return err
}
