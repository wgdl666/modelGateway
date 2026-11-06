package gateway

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lib/pq"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
)

type Store struct {
	DB    *ent.Client
	Lease time.Duration
}

// Ent 实体只留在持久层，对外继续使用原请求记录结构，保持 JSON 字段契约。
func requestRecord(r *ent.Request) Request {
	return Request{ID: r.ID, ModelKey: r.ModelKey, Payload: r.RequestPayload, InstanceID: r.InstanceID,
		BackendTaskID: r.BackendTaskID, Units: r.ReservedUnits, Status: string(r.Status), Result: r.Result,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func (s *Store) Get(ctx context.Context, id string) (Request, error) {
	r, err := s.DB.Request.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	return requestRecord(r), nil
}

// 可串行化事务保护尚不存在的同地址实例；冲突时重读，避免两个副本同时注册绕过容量。
func (s *Store) identityTx(ctx context.Context, apply func(*ent.Tx) error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
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
		// 同实例的并发首次注册也可能命中唯一键；回滚后重读，不把它误判为业务失败。
		var conflict *pgconn.PgError
		if !errors.As(err, &conflict) || (conflict.Code != "40001" && conflict.Code != "40P01" && conflict.Code != "23505") {
			return err
		}
	}
	return err
}

func instanceConflict(ctx context.Context, tx *ent.Tx, model, endpoint, id string) (bool, error) {
	return tx.ModelInstance.Query().Where(modelinstance.ModelKeyEQ(model), modelinstance.EndpointEQ(endpoint),
		modelinstance.IDNEQ(id), modelinstance.Or(modelinstance.LeaseExpiresAtGT(time.Now()), modelinstance.UsedUnitsGT(0))).Exist(ctx)
}

func (s *Store) Register(ctx context.Context, i Instance) error {
	devices := make(pq.Int64Array, len(i.GPUDevices))
	for n, device := range i.GPUDevices {
		devices[n] = int64(device)
	}
	return s.identityTx(ctx, func(tx *ent.Tx) error {
		conflict, err := instanceConflict(ctx, tx, i.ModelKey, i.Endpoint, i.ID)
		if err != nil {
			return err
		}
		if conflict {
			return ErrConflict
		}
		old, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(i.ID)).ForUpdate().Only(ctx)
		if ent.IsNotFound(err) {
			return tx.ModelInstance.Create().SetID(i.ID).SetModelKey(i.ModelKey).SetEndpoint(i.Endpoint).
				SetMachineID(i.MachineID).SetGpuDevices(devices).SetCapacityUnits(i.Capacity).
				SetReady(i.Ready).SetLeaseExpiresAt(time.Now().Add(s.Lease)).Exec(ctx)
		}
		if err != nil {
			return err
		}
		if old.ModelKey != i.ModelKey || old.Endpoint != i.Endpoint || old.MachineID != i.MachineID ||
			!slices.Equal(old.GpuDevices, devices) || old.UsedUnits > i.Capacity {
			return ErrConflict
		}
		// 重注册只改声明容量与租约，保留已占用容量和管理员的排空状态。
		return tx.ModelInstance.UpdateOneID(i.ID).SetCapacityUnits(i.Capacity).SetReady(i.Ready).
			SetLeaseExpiresAt(time.Now().Add(s.Lease)).Exec(ctx)
	})
}

func (s *Store) Heartbeat(ctx context.Context, id string, ready bool) error {
	return s.identityTx(ctx, func(tx *ent.Tx) error {
		i, err := tx.ModelInstance.Query().Where(modelinstance.IDEQ(id)).ForUpdate().Only(ctx)
		if err != nil {
			return err
		}
		conflict, err := instanceConflict(ctx, tx, i.ModelKey, i.Endpoint, id)
		if err != nil {
			return err
		}
		if conflict {
			return ErrConflict
		}
		// 心跳只续租和报告就绪，不覆盖注册容量或管理员的排空设置。
		return tx.ModelInstance.UpdateOneID(id).SetReady(ready).SetLeaseExpiresAt(time.Now().Add(s.Lease)).Exec(ctx)
	})
}

func (s *Store) Drain(ctx context.Context, id string, draining bool) error {
	return s.DB.ModelInstance.UpdateOneID(id).SetDraining(draining).Exec(ctx)
}

func (s *Store) Reserve(ctx context.Context, in Submission, payload json.RawMessage) (Request, string, bool, error) {
	tx, err := s.DB.Tx(ctx)
	if err != nil {
		return Request{}, "", false, err
	}
	defer tx.Rollback()
	// 先插入请求占据唯一键，使跨副本重复提交等待首单提交；实例归属只在同一事务内暂缺。
	r, err := tx.Request.Create().SetID(in.ID).SetModelKey(in.ModelKey).SetRequestPayload(payload).
		SetInstanceID("").SetReservedUnits(in.Units).Save(ctx)
	if err != nil {
		_ = tx.Rollback()
		if ent.IsConstraintError(err) {
			if old, readErr := s.Get(ctx, in.ID); readErr == nil {
				return old, "", false, nil
			}
		}
		return Request{}, "", false, err
	}
	// 容量比较和占用率排序使用 Ent 查询构造器；只锁选中的实例，不跨模型 HTTP 调用持锁。
	i, err := tx.ModelInstance.Query().Where(modelinstance.ModelKeyEQ(in.ModelKey), modelinstance.ReadyEQ(true),
		modelinstance.DrainingEQ(false), modelinstance.LeaseExpiresAtGT(time.Now()), func(q *sql.Selector) {
			q.Where(sql.P(func(b *sql.Builder) {
				b.Ident(q.C(modelinstance.FieldCapacityUnits)).WriteOp(sql.OpSub).
					Ident(q.C(modelinstance.FieldUsedUnits)).WriteOp(sql.OpGTE).Arg(in.Units)
			}))
		}).Order(func(q *sql.Selector) {
		q.OrderExpr(sql.ExprFunc(func(b *sql.Builder) {
			b.Ident(q.C(modelinstance.FieldUsedUnits)).WriteOp(sql.OpMul).WriteString("1.0").
				WriteOp(sql.OpDiv).Ident(q.C(modelinstance.FieldCapacityUnits))
		})).OrderBy(q.C(modelinstance.FieldID))
	}).ForUpdate(sql.WithLockAction(sql.SkipLocked)).First(ctx)
	if ent.IsNotFound(err) {
		return Request{}, "", false, ErrBusy
	}
	if err != nil {
		return Request{}, "", false, err
	}
	if err = tx.ModelInstance.UpdateOneID(i.ID).AddUsedUnits(in.Units).Exec(ctx); err != nil {
		return Request{}, "", false, err
	}
	r, err = tx.Request.UpdateOneID(r.ID).SetInstanceID(i.ID).Save(ctx)
	if err != nil {
		return Request{}, "", false, err
	}
	return requestRecord(r), i.Endpoint, true, tx.Commit()
}

func (s *Store) Start(ctx context.Context, id string) (bool, error) {
	n, err := s.DB.Request.Update().Where(request.IDEQ(id), request.StatusEQ(request.StatusReserved)).SetStatus(request.StatusDispatching).Save(ctx)
	return n == 1, err
}

func terminal(state string) bool { return state == "succeeded" || state == "failed" }

func (s *Store) Apply(ctx context.Context, id string, response BackendResponse) error {
	return s.apply(ctx, id, response, false)
}

func (s *Store) apply(ctx context.Context, id string, response BackendResponse, expireReserved bool) error {
	tx, err := s.DB.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.Request.Query().Where(request.IDEQ(id)).ForUpdate().Only(ctx)
	if err != nil {
		return err
	}
	if terminal(string(r.Status)) || (expireReserved && r.Status != request.StatusReserved) || (!expireReserved && r.Status == request.StatusReserved) {
		return nil
	}
	if terminal(response.Status) {
		// 终态与容量释放同事务提交，重复完成只读终态，计数异常则整笔回滚。
		n, err := tx.ModelInstance.Update().Where(modelinstance.IDEQ(r.InstanceID), modelinstance.UsedUnitsGTE(r.ReservedUnits)).AddUsedUnits(-r.ReservedUnits).Save(ctx)
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("capacity accounting mismatch")
		}
	}
	update := tx.Request.UpdateOneID(id).SetStatus(request.Status(response.Status)).SetNillableBackendTaskID(response.TaskID)
	if len(response.Result) == 0 {
		update.ClearResult()
	} else {
		update.SetResult(response.Result)
	}
	if err := update.Exec(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

type Pending struct{ ID, Endpoint string }

func (s *Store) Reconcile(ctx context.Context, interval, dispatchTimeout time.Duration) ([]Pending, error) {
	now := time.Now()
	stale, err := s.DB.Request.Query().Where(request.StatusEQ(request.StatusReserved), request.UpdatedAtLT(now.Add(-30*time.Second))).Limit(100).IDs(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range stale {
		if err := s.apply(ctx, id, BackendResponse{Status: "failed", Result: json.RawMessage(`{"error_code":"not_dispatched"}`)}, true); err != nil {
			return nil, err
		}
	}
	tx, err := s.DB.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// 在行锁事务内认领一批查询，随后批量读取实例地址；实际 HTTP 查询在提交后执行。
	selected, err := tx.Request.Query().Where(request.Or(
		request.And(request.StatusIn(request.StatusRunning, request.StatusUnknown), request.UpdatedAtLT(now.Add(-interval))),
		request.And(request.StatusEQ(request.StatusDispatching), request.UpdatedAtLT(now.Add(-dispatchTimeout))),
	)).Order(request.ByUpdatedAt()).Limit(20).ForUpdate(sql.WithLockAction(sql.SkipLocked)).All(ctx)
	if err != nil || len(selected) == 0 {
		return nil, err
	}
	ids, instances := make([]string, 0, len(selected)), make([]string, 0, len(selected))
	for _, r := range selected {
		ids, instances = append(ids, r.ID), append(instances, r.InstanceID)
	}
	if err := tx.Request.Update().Where(request.IDIn(ids...)).SetUpdatedAt(now).Exec(ctx); err != nil {
		return nil, err
	}
	backends, err := tx.ModelInstance.Query().Where(modelinstance.IDIn(instances...)).All(ctx)
	if err != nil {
		return nil, err
	}
	endpoints := make(map[string]string, len(backends))
	for _, i := range backends {
		endpoints[i.ID] = i.Endpoint
	}
	out := make([]Pending, 0, len(selected))
	for _, r := range selected {
		if endpoint, ok := endpoints[r.InstanceID]; ok {
			out = append(out, Pending{r.ID, endpoint})
		}
	}
	return out, tx.Commit()
}
