package store

import (
	"context"
	stdsql "database/sql"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wgdl666/modelGateway/ent"
)

var (
	ErrFull     = errors.New("waiting queue is full")
	ErrConflict = errors.New("request or instance conflict")
)

// Store 管理 PG 账本，同一事务协调多个网关副本的受理和容量分配。
type Store struct{ db *ent.Client }

func New(db *ent.Client) *Store {
	return &Store{db: db}
}

func (s *Store) CheckReady(ctx context.Context) error {
	if _, err := s.db.ModelInstance.Query().Exist(ctx); err != nil {
		return err
	}
	_, err := s.db.Request.Query().Exist(ctx)
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
