package store

import (
	"context"
	stdsql "database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/modelinstance"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/ent/resourcegroup"
)

var (
	ErrFull                 = errors.New("waiting queue is full")
	ErrConflict             = errors.New("request or instance conflict")
	ErrModelUnregistered    = errors.New("model has no registered instance")
	ErrInstanceNotClearable = errors.New("instance must be paused, not ready and have no in-flight requests")
)

// Store 管理 PG 账本，同一事务协调多个网关副本的受理和容量分配。
type Store struct{ db *ent.Client }

func New(db *ent.Client) *Store {
	return &Store{db: db}
}

func (s *Store) CheckReady(ctx context.Context) error {
	// 组表先迁移，网关才能按统一容量接单。
	if _, err := s.db.ResourceGroup.Query().Select(resourcegroup.FieldCapacityUnits).Limit(1).All(ctx); err != nil {
		return err
	}
	// 就绪检查覆盖软删除字段，迁移完成后才允许新副本接流量。
	if _, err := s.db.ModelInstance.Query().Where(modelinstance.DeletedAtIsNil()).Exist(ctx); err != nil {
		return err
	}
	// 同步、多卡请求都依赖这份占用快照，迁移缺失时不能接流量。
	_, err := s.db.Request.Query().Select(request.FieldQueuePublishAt, request.FieldResourceGroups).Limit(1).All(ctx)
	return err
}

// 同组容量、等待预算和幂等都涉及多行。PG 用可串行化事务阻止两个副本按同一旧快照提交。
func (s *Store) serialize(ctx context.Context, apply func(*ent.Tx) error) error {
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		tx, beginErr := s.db.BeginTx(ctx, &stdsql.TxOptions{Isolation: stdsql.LevelSerializable})
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
