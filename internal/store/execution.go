package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

// ExecutionWriteTimeout 给执行终态写入预留时间，避免结果回查提前接管。
const ExecutionWriteTimeout = 3 * time.Second

// CompleteSynchronous 以读完上游响应为释放依据；连接中断只能证明结果未知，不能虚构 GPU 已空闲。
func (s *Store) CompleteSynchronous(ctx context.Context, id, claim string, result protocol.HTTPResult, known bool) error {
	return s.updateExecution(ctx, id, claim, func(task *ent.Request, update *ent.RequestUpdateOne) error {
		update.ClearNextPollAt()
		if !known {
			if task.State == request.StateProcessing {
				update.SetState(request.StateFailed).SetFinishedAt(time.Now().UTC()).SetErrorStatus(504).
					SetErrorCode("backend_outcome_unknown").SetErrorMessage("synchronous model execution outcome is unknown")
			}
			return nil
		}
		update.SetSlotReserved(false)
		if task.State == request.StateFailed {
			return nil
		}
		update.SetFinishedAt(time.Now().UTC())
		if result.StatusCode < 200 || result.StatusCode >= 300 {
			update.SetState(request.StateFailed).SetErrorStatus(result.StatusCode).
				SetErrorCode("upstream_error").SetErrorMessage(fmt.Sprintf("model returned HTTP %d", result.StatusCode))
			return nil
		}
		raw, err := json.Marshal(protocol.Result{HTTP: &result})
		if err == nil {
			update.SetState(request.StateSucceeded).SetResult(raw)
		}
		return err
	})
}

// ClaimPoll 领取结果回查，派发与容量归还仍由请求账本里的状态决定。
func (s *Store) ClaimPoll(ctx context.Context) (*ent.Request, error) {
	tx, err := s.db.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	// 回查沿用派发账本里的原地址、任务 ID 和协议，后续登记不改变在途任务。
	task, err := tx.Request.Query().Where(request.SlotReservedEQ(true), request.NextPollAtLTE(now),
		request.ProtocolNotIn(protocol.SynchronousProtocols()...)).
		Order(ent.Asc(request.FieldNextPollAt, request.FieldID)).ForUpdate(sql.WithLockAction(sql.SkipLocked)).
		Select(request.FieldID, request.FieldEndpoint, request.FieldModelKey, request.FieldProtocol, request.FieldBackendTaskID, request.FieldStartedAt, request.FieldState).First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 首次派发和回查共用领取标识，旧一轮调用或查询的迟到结果不能覆盖重试中的请求。
	task.PollClaim = uuid.NewString()
	if err := tx.Request.UpdateOneID(task.ID).SetPollClaim(task.PollClaim).SetNextPollAt(now.Add(8 * time.Second)).Exec(ctx); err != nil {
		return nil, err
	}
	return task, tx.Commit()
}

// ExtendPoll 给视频发布延长领取期限，仅当前持有者能开始下载和上传。
func (s *Store) ExtendPoll(ctx context.Context, id, claim string, until time.Time) (bool, error) {
	count, err := s.db.Request.Update().Where(request.IDEQ(id), request.SlotReservedEQ(true), request.PollClaimEQ(claim)).
		SetNextPollAt(until).Save(ctx)
	return count == 1, err
}

func (s *Store) updateExecution(ctx context.Context, id, claim string, apply func(*ent.Request, *ent.RequestUpdateOne) error) error {
	tx, err := s.db.Tx(ctx)
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

func (s *Store) RecordResponse(ctx context.Context, id, claim string, response protocol.ModelResponse, interval time.Duration) error {
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

func (s *Store) DispatchFailure(ctx context.Context, id, claim string, status int, code, message string, hold bool) error {
	return s.updateExecution(ctx, id, claim, func(task *ent.Request, update *ent.RequestUpdateOne) error {
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
