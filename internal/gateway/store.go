package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	DB    *pgxpool.Pool
	Lease time.Duration
}

const columns = `request_id,model_key,request_payload,instance_id,backend_task_id,reserved_units,status,result,created_at,updated_at`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.ModelKey, &r.Payload, &r.InstanceID, &r.BackendTaskID, &r.Units, &r.Status, &r.Result, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}
func (s *Store) Get(ctx context.Context, id string) (Request, error) {
	return scanRequest(s.DB.QueryRow(ctx, `SELECT `+columns+` FROM model_gateway.requests WHERE request_id=$1`, id))
}

func (s *Store) Register(ctx context.Context, i Instance) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// 同地址的注册串行检查，防止换一个实例 ID 就绕过旧请求仍持有的容量。
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, i.ModelKey+"|"+i.Endpoint)
	if e != nil {
		return e
	}
	var conflict bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_gateway.model_instances WHERE model_key=$1 AND endpoint=$2 AND instance_id<>$3 AND (lease_expires_at>now() OR used_units>0))`, i.ModelKey, i.Endpoint, i.ID).Scan(&conflict)
	if e != nil {
		return e
	}
	if conflict {
		return ErrConflict
	}
	tag, e := tx.Exec(ctx, `INSERT INTO model_gateway.model_instances(instance_id,model_key,endpoint,machine_id,gpu_devices,capacity_units,ready,lease_expires_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,now()+$8*interval '1 second')
 ON CONFLICT(instance_id) DO UPDATE SET capacity_units=EXCLUDED.capacity_units,ready=EXCLUDED.ready,lease_expires_at=EXCLUDED.lease_expires_at,updated_at=now()
 WHERE model_instances.model_key=EXCLUDED.model_key AND model_instances.endpoint=EXCLUDED.endpoint
 AND model_instances.machine_id=EXCLUDED.machine_id AND model_instances.gpu_devices=EXCLUDED.gpu_devices
 AND model_instances.used_units<=EXCLUDED.capacity_units`, i.ID, i.ModelKey, i.Endpoint, i.MachineID, i.GPUDevices, i.Capacity, i.Ready, s.Lease.Seconds())
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}
func (s *Store) Heartbeat(ctx context.Context, id string, ready bool) error {
	var model, endpoint string
	e := s.DB.QueryRow(ctx, `SELECT model_key,endpoint FROM model_gateway.model_instances WHERE instance_id=$1`, id).Scan(&model, &endpoint)
	if e != nil {
		return e
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// 心跳只续租与报告就绪，不能用先前读取的配置覆盖管理员或注册更新。
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, model+"|"+endpoint)
	if e != nil {
		return e
	}
	var conflict bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_gateway.model_instances WHERE model_key=$1 AND endpoint=$2 AND instance_id<>$3 AND (lease_expires_at>now() OR used_units>0))`, model, endpoint, id).Scan(&conflict)
	if e != nil {
		return e
	}
	if conflict {
		return ErrConflict
	}
	_, e = tx.Exec(ctx, `UPDATE model_gateway.model_instances SET ready=$2,lease_expires_at=now()+$3*interval '1 second',updated_at=now() WHERE instance_id=$1`, id, ready, s.Lease.Seconds())
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s *Store) Drain(ctx context.Context, id string, draining bool) error {
	tag, e := s.DB.Exec(ctx, `UPDATE model_gateway.model_instances SET draining=$2,updated_at=now() WHERE instance_id=$1`, id, draining)
	if e == nil && tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return e
}

func (s *Store) Reserve(ctx context.Context, in Submission, payload json.RawMessage) (Request, string, bool, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return Request{}, "", false, e
	}
	defer tx.Rollback(ctx)
	// 仅锁同一个请求 ID；多网关副本对重复提交只创建一份请求和一份容量占用。
	_, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, in.ID)
	if e != nil {
		return Request{}, "", false, e
	}
	old, e := scanRequest(tx.QueryRow(ctx, `SELECT `+columns+` FROM model_gateway.requests WHERE request_id=$1`, in.ID))
	if e == nil {
		return old, "", false, tx.Commit(ctx)
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return Request{}, "", false, e
	}
	var instance, endpoint string
	e = tx.QueryRow(ctx, `SELECT instance_id,endpoint FROM model_gateway.model_instances
 WHERE model_key=$1 AND ready AND NOT draining AND lease_expires_at>now() AND $2<=capacity_units-used_units
 ORDER BY used_units::double precision/capacity_units,instance_id LIMIT 1 FOR UPDATE SKIP LOCKED`, in.ModelKey, in.Units).Scan(&instance, &endpoint)
	if errors.Is(e, pgx.ErrNoRows) {
		return Request{}, "", false, ErrBusy
	}
	if e != nil {
		return Request{}, "", false, e
	}
	_, e = tx.Exec(ctx, `UPDATE model_gateway.model_instances SET used_units=used_units+$2,updated_at=now() WHERE instance_id=$1`, instance, in.Units)
	if e != nil {
		return Request{}, "", false, e
	}
	r, e := scanRequest(tx.QueryRow(ctx, `INSERT INTO model_gateway.requests(request_id,model_key,request_payload,instance_id,reserved_units,status)
 VALUES($1,$2,$3,$4,$5,'reserved') RETURNING `+columns, in.ID, in.ModelKey, payload, instance, in.Units))
	if e != nil {
		return Request{}, "", false, e
	}
	return r, endpoint, true, tx.Commit(ctx)
}
func (s *Store) Start(ctx context.Context, id string) (bool, error) {
	tag, e := s.DB.Exec(ctx, `UPDATE model_gateway.requests SET status='dispatching',updated_at=now() WHERE request_id=$1 AND status='reserved'`, id)
	return tag.RowsAffected() == 1, e
}

func terminal(state string) bool { return state == "succeeded" || state == "failed" }
func (s *Store) Apply(ctx context.Context, id string, response BackendResponse) error {
	return s.apply(ctx, id, response, false)
}
func (s *Store) apply(ctx context.Context, id string, response BackendResponse, expireReserved bool) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	r, e := scanRequest(tx.QueryRow(ctx, `SELECT `+columns+` FROM model_gateway.requests WHERE request_id=$1 FOR UPDATE`, id))
	if e != nil {
		return e
	}
	if terminal(r.Status) {
		return nil
	}
	if expireReserved && r.Status != "reserved" {
		return nil
	}
	if !expireReserved && r.Status == "reserved" {
		return nil
	}
	if terminal(response.Status) {
		// 请求终态和减 N 同事务，重复完成回执不会重复释放；计数异常时整笔回滚。
		tag, err := tx.Exec(ctx, `UPDATE model_gateway.model_instances SET used_units=used_units-$2,updated_at=now() WHERE instance_id=$1 AND used_units >= $2`, r.InstanceID, r.Units)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("capacity accounting mismatch")
		}
	}
	_, e = tx.Exec(ctx, `UPDATE model_gateway.requests SET status=$2,backend_task_id=COALESCE($3,backend_task_id),result=$4,updated_at=now() WHERE request_id=$1`, id, response.Status, response.TaskID, response.Result)
	if e != nil {
		return e
	}
	return tx.Commit(ctx)
}

type Pending struct{ ID, Endpoint string }

func (s *Store) Reconcile(ctx context.Context, interval, dispatchTimeout time.Duration) ([]Pending, error) {
	rows, e := s.DB.Query(ctx, `SELECT request_id FROM model_gateway.requests WHERE status='reserved' AND updated_at<now()-interval '30 seconds' LIMIT 100`)
	if e != nil {
		return nil, e
	}
	var stale []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		stale = append(stale, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for _, id := range stale {
		if e = s.apply(ctx, id, BackendResponse{Status: "failed", Result: json.RawMessage(`{"error_code":"not_dispatched"}`)}, true); e != nil {
			return nil, e
		}
	}
	// 用更新时间短暂认领本批状态查询；HTTP 超时短于查询间隔。查询重复仍由终态事务去重。
	rows, e = s.DB.Query(ctx, `WITH selected AS (
 SELECT request_id FROM model_gateway.requests
 WHERE (status IN ('running','unknown') AND updated_at<now()-$1*interval '1 second')
 OR (status='dispatching' AND updated_at<now()-$2*interval '1 second')
 ORDER BY updated_at LIMIT 20 FOR UPDATE SKIP LOCKED), claimed AS (
 UPDATE model_gateway.requests r SET updated_at=now() FROM selected s WHERE r.request_id=s.request_id RETURNING r.request_id,r.instance_id)
 SELECT c.request_id,i.endpoint FROM claimed c JOIN model_gateway.model_instances i ON i.instance_id=c.instance_id`, interval.Seconds(), dispatchTimeout.Seconds())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Pending{}
	for rows.Next() {
		var p Pending
		if e = rows.Scan(&p.ID, &p.Endpoint); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
