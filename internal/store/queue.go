package store

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/predicate"
	"github.com/wgdl666/modelGateway/ent/request"
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
	MaxWaitingBytes          int64
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
		healthy, err := tx.ModelInstance.Query().Where(
			modelinstance.ModelKeyEQ(in.ModelKey), modelinstance.CapabilityEQ(in.Capability),
			modelinstance.ReadyEQ(true), modelinstance.DrainingEQ(false),
			modelinstance.HealthCheckedAtGT(now.Add(-probeMaxAge)),
			modelinstance.CapacityUnitsGTE(in.Units), modelinstance.GroupCapacityUnitsGTE(in.Units),
		).Exist(ctx)
		if err != nil {
			return err
		}
		if !healthy {
			return ErrBusy
		}
		var totals []struct {
			Count int
			Bytes *int64
		}
		if err := tx.Request.Query().Where(
			request.ModelKeyEQ(in.ModelKey), request.StateEQ(request.StateQueued), request.DeadlineAtGT(now),
		).Aggregate(ent.As(ent.Count(), "count"), ent.As(ent.Sum(request.FieldPayloadBytes), "bytes")).Scan(ctx, &totals); err != nil {
			return err
		}
		var waitingBytes int64
		if totals[0].Bytes != nil {
			waitingBytes = *totals[0].Bytes
		}
		if totals[0].Count >= in.MaxWaiting || int64(len(in.Payload)) > in.MaxWaitingBytes-waitingBytes {
			return ErrFull
		}
		task, err = tx.Request.Create().SetID(in.ID).SetModelKey(in.ModelKey).SetCapability(in.Capability).
			SetTraceID(in.TraceID).SetRequestPayload(in.Payload).SetPayloadBytes(int64(len(in.Payload))).SetUnits(in.Units).
			SetCreatedAt(now).SetDeadlineAt(now.Add(in.BackendTimeout)).Save(ctx)
		return err
	})
	return task, err
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

func (s *Store) Claim(ctx context.Context) (*ent.Request, error) {
	var task *ent.Request
	err := s.serialize(ctx, func(tx *ent.Tx) error {
		task = nil
		now := time.Now().UTC()
		instances, err := tx.ModelInstance.Query().Where(modelinstance.ReadyEQ(true), modelinstance.DrainingEQ(false),
			modelinstance.HealthCheckedAtGT(now.Add(-probeMaxAge))).Order(ent.Asc(modelinstance.FieldID)).All(ctx)
		if err != nil || len(instances) == 0 {
			return err
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
		// 过滤当前无法派发的能力，避免同卡队首阻塞别的资源组。
		var runnable []predicate.Request
		available := make(map[string]int, len(instances))
		for _, i := range instances {
			free := min(i.CapacityUnits-byInstance[i.ID], i.GroupCapacityUnits-byGroup[i.ResourceGroup])
			if free > 0 {
				available[i.ID] = free
				runnable = append(runnable, request.And(request.ModelKeyEQ(i.ModelKey), request.CapabilityEQ(i.Capability), request.UnitsLTE(free)))
			}
		}
		if len(runnable) == 0 {
			return nil
		}
		next, err := tx.Request.Query().Where(request.StateEQ(request.StateQueued), request.DeadlineAtGT(now),
			request.Or(runnable...)).Order(ent.Asc(request.FieldCreatedAt, request.FieldID)).
			ForUpdate(sql.WithLockAction(sql.SkipLocked)).First(ctx)
		if ent.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, i := range instances {
			if i.ModelKey != next.ModelKey || i.Capability != next.Capability || available[i.ID] < next.Units {
				continue
			}
			// 与排空串行化，提交后请求身份和物理槽位才对其他副本可见。
			if _, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(i.ID)).ForUpdate().Only(ctx); err != nil {
				return err
			}
			task, err = tx.Request.UpdateOneID(next.ID).SetState(request.StateProcessing).SetSlotReserved(true).
				SetInstanceID(i.ID).SetResourceGroup(i.ResourceGroup).SetEndpoint(i.Endpoint).SetArtifactDigest(i.ArtifactDigest).
				SetStartedAt(now).SetNextPollAt(next.DeadlineAt.Add(2 * time.Second)).Save(ctx)
			return err
		}
		return nil
	})
	return task, err
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
