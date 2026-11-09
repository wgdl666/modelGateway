package gateway

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/predicate"
	"github.com/wgdl666/modelGateway/ent/request"
)

type Store struct{ DB *ent.Client }

// 同组容量、等待预算和幂等都涉及多行。PG 用可串行化事务阻止两个副本按同一旧快照提交。
func (s *Store) serialize(ctx context.Context, apply func(*ent.Tx) error) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		tx, beginErr := s.DB.BeginTx(ctx, &stdsql.TxOptions{Isolation: stdsql.LevelSerializable})
		if beginErr != nil {
			return beginErr
		}
		err = apply(tx)
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		var conflict *pgconn.PgError
		if !errors.As(err, &conflict) || (conflict.Code != "40001" && conflict.Code != "40P01" && conflict.Code != "23505") {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 4 * time.Millisecond):
		}
	}
	return err
}

func (s *Store) Register(ctx context.Context, spec Instance) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		old, err := tx.ModelInstance.Get(ctx, spec.ID)
		if err == nil {
			// 相同部署重复注册是幂等操作，不改排空状态、健康快照或任务账本。
			if old.ModelKey == spec.ModelKey && old.Capability == spec.Capability && old.Endpoint == spec.Endpoint &&
				old.HealthURL == spec.HealthURL && old.ArtifactDigest == spec.Artifact && old.ResourceGroup == spec.ResourceGroup &&
				old.GroupCapacityUnits == spec.GroupCapacity && old.CapacityUnits == spec.Capacity &&
				old.MachineID == spec.MachineID && slices.Equal(old.GpuDevices, spec.GPUDevices) {
				return nil
			}
			return ErrConflict
		}
		if !ent.IsNotFound(err) {
			return err
		}
		conflict, err := tx.ModelInstance.Query().Where(
			modelinstance.ResourceGroupEQ(spec.ResourceGroup),
			modelinstance.GroupCapacityUnitsNEQ(spec.GroupCapacity),
		).Exist(ctx)
		if err != nil {
			return err
		}
		if conflict {
			return ErrConflict
		}
		// 同一执行地址的新实例不能绕过仍在执行的旧实例及其资源组。
		sameEndpoint, err := tx.ModelInstance.Query().Where(
			modelinstance.ModelKeyEQ(spec.ModelKey), modelinstance.CapabilityEQ(spec.Capability),
			modelinstance.EndpointEQ(spec.Endpoint),
		).All(ctx)
		if err != nil {
			return err
		}
		for _, instance := range sameEndpoint {
			held, err := tx.Request.Query().Where(request.InstanceIDEQ(instance.ID), request.SlotReservedEQ(true)).Exist(ctx)
			if err != nil {
				return err
			}
			if !instance.Draining || held {
				return ErrConflict
			}
		}
		return tx.ModelInstance.Create().
			SetID(spec.ID).SetModelKey(spec.ModelKey).SetCapability(spec.Capability).
			SetEndpoint(spec.Endpoint).SetHealthURL(spec.HealthURL).SetArtifactDigest(spec.Artifact).
			SetResourceGroup(spec.ResourceGroup).SetGroupCapacityUnits(spec.GroupCapacity).
			SetCapacityUnits(spec.Capacity).SetMachineID(spec.MachineID).SetGpuDevices(spec.GPUDevices).Exec(ctx)
	})
}

func (s *Store) Drain(ctx context.Context, id string, draining bool) error {
	return s.serialize(ctx, func(tx *ent.Tx) error {
		instance, err := tx.ModelInstance.Get(ctx, id)
		if err != nil {
			return err
		}
		if !draining {
			conflict, err := tx.ModelInstance.Query().Where(
				modelinstance.IDNEQ(id), modelinstance.ModelKeyEQ(instance.ModelKey),
				modelinstance.CapabilityEQ(instance.Capability), modelinstance.EndpointEQ(instance.Endpoint),
				modelinstance.DrainingEQ(false),
			).Exist(ctx)
			if err != nil {
				return err
			}
			if conflict {
				return ErrConflict
			}
		}
		return tx.ModelInstance.UpdateOneID(id).SetDraining(draining).Exec(ctx)
	})
}

func (s *Store) RecordProbe(ctx context.Context, id string, checked time.Time, ready bool) error {
	// 并行探针的响应可能乱序，较早样本不能覆盖较新的可用性判断。
	_, err := s.DB.ModelInstance.Update().Where(
		modelinstance.IDEQ(id), modelinstance.HealthCheckedAtLT(checked),
	).SetHealthCheckedAt(checked).SetReady(ready).Save(ctx)
	return err
}

type admission struct {
	id, model, capability string
	traceID               string
	raw                   json.RawMessage
	input                 json.RawMessage
	units                 int
	config                Config
}

func sameInput(payload, input json.RawMessage) bool {
	var stored Submission
	var left, right any
	return json.Unmarshal(payload, &stored) == nil &&
		json.Unmarshal(stored.Input, &left) == nil &&
		json.Unmarshal(input, &right) == nil && reflect.DeepEqual(left, right)
}

func (s *Store) Admit(ctx context.Context, in admission) (*ent.Request, error) {
	var task *ent.Request
	err := s.serialize(ctx, func(tx *ent.Tx) error {
		now := time.Now().UTC()
		old, err := tx.Request.Get(ctx, in.id)
		if err == nil {
			if old.ModelKey != in.model || !sameInput(old.RequestPayload, in.input) {
				return ErrConflict
			}
			task = old
			return nil
		}
		if !ent.IsNotFound(err) {
			return err
		}
		healthy, err := tx.ModelInstance.Query().Where(
			modelinstance.ModelKeyEQ(in.model), modelinstance.CapabilityEQ(in.capability),
			modelinstance.ReadyEQ(true), modelinstance.DrainingEQ(false),
			modelinstance.HealthCheckedAtGT(now.Add(-probeMaxAge)),
			modelinstance.CapacityUnitsGTE(in.units), modelinstance.GroupCapacityUnitsGTE(in.units),
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
			request.ModelKeyEQ(in.model), request.StateEQ(request.StateQueued), request.DeadlineAtGT(now),
		).Aggregate(ent.As(ent.Count(), "count"), ent.As(ent.Sum(request.FieldPayloadBytes), "bytes")).Scan(ctx, &totals); err != nil {
			return err
		}
		var waitingBytes int64
		if totals[0].Bytes != nil {
			waitingBytes = *totals[0].Bytes
		}
		if totals[0].Count >= in.config.MaxWaiting || int64(len(in.raw)) > in.config.MaxWaitingBytes-waitingBytes {
			return ErrFull
		}
		task, err = tx.Request.Create().SetID(in.id).SetModelKey(in.model).SetCapability(in.capability).
			SetTraceID(in.traceID).SetRequestPayload(in.raw).SetPayloadBytes(int64(len(in.raw))).SetUnits(in.units).
			SetCreatedAt(now).SetDeadlineAt(now.Add(in.config.BackendTimeout)).Save(ctx)
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

func (s *Store) Get(ctx context.Context, id string) (*ent.Request, error) {
	return s.DB.Request.Query().Where(request.IDEQ(id)).Select(
		request.FieldID, request.FieldModelKey, request.FieldState, request.FieldInstanceID,
		request.FieldDeadlineAt, request.FieldResult, request.FieldErrorCode, request.FieldErrorMessage, request.FieldErrorStatus,
	).Only(ctx)
}

func (s *Store) Cancel(ctx context.Context, id string) error {
	n, err := s.DB.Request.Update().Where(request.IDEQ(id), request.StateEQ(request.StateQueued)).
		SetState(request.StateFailed).SetErrorCode("task_cancelled").SetErrorMessage("request cancelled before dispatch").
		SetErrorStatus(409).SetFinishedAt(time.Now().UTC()).Save(ctx)
	if err != nil || n > 0 {
		return err
	}
	found, err := s.DB.Request.Query().Where(request.IDEQ(id)).Exist(ctx)
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
	_, err := s.DB.Request.Update().Where(request.StateEQ(request.StateQueued), request.DeadlineAtLTE(now)).
		SetState(request.StateFailed).SetErrorCode("queue_timeout").SetErrorMessage("request expired before dispatch").
		SetErrorStatus(504).SetFinishedAt(now).Save(ctx)
	return err
}

func (s *Store) ClaimPoll(ctx context.Context) (*ent.Request, error) {
	tx, err := s.DB.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	task, err := tx.Request.Query().Where(request.SlotReservedEQ(true), request.NextPollAtLTE(now)).
		Order(ent.Asc(request.FieldNextPollAt, request.FieldID)).ForUpdate(sql.WithLockAction(sql.SkipLocked)).
		Select(request.FieldID, request.FieldEndpoint).First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 只认领一次短查询。副本退出后时间到即可接管，迟到的查询不能覆盖新的领取结果。
	task.PollClaim = uuid.NewString()
	if err := tx.Request.UpdateOneID(task.ID).SetPollClaim(task.PollClaim).SetNextPollAt(now.Add(8 * time.Second)).Exec(ctx); err != nil {
		return nil, err
	}
	return task, tx.Commit()
}

func (s *Store) updateExecution(ctx context.Context, id, claim string, apply func(*ent.Request, *ent.RequestUpdateOne) error) error {
	tx, err := s.DB.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := tx.Request.Query().Where(request.IDEQ(id)).ForUpdate().Select(
		request.FieldID, request.FieldSlotReserved, request.FieldState,
		request.FieldPollClaim, request.FieldBackendTaskID, request.FieldDeadlineAt,
	).Only(ctx)
	if err != nil {
		return err
	}
	if !task.SlotReserved || (claim != "" && claim != task.PollClaim) {
		return nil
	}
	update := tx.Request.UpdateOneID(id).SetPollClaim("")
	if err := apply(task, update); err != nil {
		return err
	}
	if err := update.Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordResponse(ctx context.Context, id, claim string, response ModelResponse, interval time.Duration) error {
	return s.updateExecution(ctx, id, claim, func(task *ent.Request, update *ent.RequestUpdateOne) error {
		now := time.Now().UTC()
		if response.BackendTaskID != "" {
			update.SetBackendTaskID(response.BackendTaskID)
		}
		if response.State == "processing" {
			if task.BackendTaskID == "" && response.BackendTaskID == "" {
				update.SetBackendTaskID(id)
			}
			update.SetNextPollAt(now.Add(interval))
			return nil
		}
		// 终态和容量同一事务提交，已报失败的任务只释放，不会被迟到的成功改写。
		update.SetSlotReserved(false).ClearNextPollAt()
		if task.State == request.StateFailed {
			return nil
		}
		update.SetState(request.State(response.State)).SetFinishedAt(now)
		if response.State == "failed" {
			update.SetErrorStatus(502).SetErrorCode(response.Error.Code).SetErrorMessage(response.Error.Message)
			return nil
		}
		raw, err := json.Marshal(response.Result)
		if err == nil {
			update.SetResult(raw)
		}
		return err
	})
}

func (s *Store) DispatchFailure(ctx context.Context, id string, status int, code, message string, hold bool) error {
	return s.updateExecution(ctx, id, "", func(task *ent.Request, update *ent.RequestUpdateOne) error {
		now := time.Now().UTC()
		if task.State == request.StateProcessing {
			update.SetState(request.StateFailed).SetErrorStatus(status).SetErrorCode(code).SetErrorMessage(message).SetFinishedAt(now)
		}
		if hold {
			update.SetNextPollAt(now)
		} else {
			update.SetSlotReserved(false).ClearNextPollAt()
		}
		return nil
	})
}

func (s *Store) PollUnavailable(ctx context.Context, id, claim string, interval time.Duration) error {
	return s.updateExecution(ctx, id, claim, func(task *ent.Request, update *ent.RequestUpdateOne) error {
		now := time.Now().UTC()
		// 领取副本中断时可能连受理响应也未保存。查不到终态仍保留槽位，不重新提交生成。
		if task.State == request.StateProcessing && task.BackendTaskID == "" && !now.Before(task.DeadlineAt) {
			update.SetState(request.StateFailed).SetErrorStatus(504).SetErrorCode("backend_outcome_unknown").
				SetErrorMessage("model outcome is unknown").SetFinishedAt(now)
		}
		update.SetNextPollAt(now.Add(interval))
		return nil
	})
}
