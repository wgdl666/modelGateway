package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// ExecutionWriteTimeout 给执行终态写入预留时间，避免结果回查提前接管。
const ExecutionWriteTimeout = 3 * time.Second

// SynchronousWriteTimeout 覆盖三次独立的保存尝试和退避，不增加模型执行预算。
// shortcut: 结果仅在进程存活且重试预算内保留；需跨重启或长故障恢复时再引入持久完成凭据。
const SynchronousWriteTimeout = 10 * time.Second

// CompleteSynchronous 以读完上游响应为释放依据；连接中断只能证明结果未知，不能虚构 GPU 已空闲。
func (s *Store) CompleteSynchronous(ctx context.Context, id, claim string, result protocol.HTTPResult, known bool) error {
	apply := func(attemptCtx context.Context) error {
		return s.updateExecution(attemptCtx, id, claim, func(task *ent.Request, update *ent.RequestUpdate) error {
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
	// 只重试已拿到的执行结果。相同领取标识及原事务保护重复提交和提交结果不确定的情况。
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attemptCtx, cancel := context.WithTimeout(ctx, ExecutionWriteTimeout)
		err = apply(attemptCtx)
		timedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt == 2 || (!timedOut && !retryableCompletionError(err)) {
			return err
		}
		telemetry.LogError(ctx, "synchronous outcome persistence retry", err, "request_id", id, "attempt", attempt+1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
	return err
}

// 仅重试瞬时数据库错误；校验、缺表和约束错误不会靠重复写入恢复。
func retryableCompletionError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) || pgconn.SafeToRetry(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "08") || pgErr.Code == "40001" || pgErr.Code == "40P01" || pgErr.Code == "53300" || pgErr.Code == "57P01"
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
	// 仅取更新行数，不读回实体；前面的行锁和领取标识仍负责并发隔离。
	count, err := tx.Request.Update().Where(request.IDEQ(task.ID)).SetPollClaim(task.PollClaim).SetNextPollAt(now.Add(8 * time.Second)).Save(ctx)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, &ent.NotFoundError{}
	}
	return task, tx.Commit()
}

// ExtendPoll 给视频发布延长领取期限，仅当前持有者能开始下载和上传。
func (s *Store) ExtendPoll(ctx context.Context, id, claim string, until time.Time) (bool, error) {
	count, err := s.db.Request.Update().Where(request.IDEQ(id), request.SlotReservedEQ(true), request.PollClaimEQ(claim)).
		SetNextPollAt(until).Save(ctx)
	return count == 1, err
}

func (s *Store) updateExecution(ctx context.Context, id, claim string, apply func(*ent.Request, *ent.RequestUpdate) error) (err error) {
	// 只记录阶段和耗时，避免数据库错误链携带 SQL 参数或媒体正文进入遥测。
	ctx, span := otel.Tracer("github.com/wgdl666/modelGateway").Start(ctx, "gateway.db.update_execution")
	phase := "begin_tx"
	span.SetAttributes(attribute.String("request_id", id))
	defer func() {
		if err != nil {
			span.SetAttributes(attribute.String("db.phase", phase))
			span.SetStatus(codes.Error, phase)
			telemetry.LogError(ctx, "gateway execution persistence failed", err, "request_id", id, "db.phase", phase)
		}
		span.End()
	}()
	started := time.Now()
	tx, err := s.db.Tx(ctx)
	span.SetAttributes(attribute.Float64("db.begin_tx_ms", float64(time.Since(started))/float64(time.Millisecond)))
	if err != nil {
		return err
	}
	defer tx.Rollback()
	phase = "lock_request"
	started = time.Now()
	task, err := tx.Request.Query().Where(request.IDEQ(id)).ForUpdate().Select(
		request.FieldID, request.FieldSlotReserved, request.FieldState,
		request.FieldPollClaim, request.FieldBackendTaskID, request.FieldDeadlineAt,
		request.FieldCreatedAt, request.FieldStartedAt, request.FieldDispatchAttempts, request.FieldDispatchRetryReason,
	).Only(ctx)
	span.SetAttributes(attribute.Float64("db.lock_request_ms", float64(time.Since(started))/float64(time.Millisecond)))
	if err != nil {
		return err
	}
	if !task.SlotReserved || (claim != "" && claim != task.PollClaim) {
		return nil
	}
	phase = "update"
	// 已持有目标行锁，直接按 ID 更新；用影响行数判断不存在，不再查询更新后的实体。
	update := tx.Request.Update().Where(request.IDEQ(id)).SetPollClaim("")
	if err = apply(task, update); err != nil {
		return err
	}
	started = time.Now()
	count, err := update.Save(ctx)
	span.SetAttributes(attribute.Float64("db.update_ms", float64(time.Since(started))/float64(time.Millisecond)))
	if err != nil {
		return err
	}
	if count == 0 {
		return &ent.NotFoundError{}
	}
	phase = "commit"
	started = time.Now()
	err = tx.Commit()
	span.SetAttributes(attribute.Float64("db.commit_ms", float64(time.Since(started))/float64(time.Millisecond)))
	return err
}

func (s *Store) RecordResponse(ctx context.Context, id, claim string, response protocol.ModelResponse, interval time.Duration) error {
	return s.updateExecution(ctx, id, claim, func(task *ent.Request, update *ent.RequestUpdate) error {
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
		if response.Result.Generation == nil {
			response.Result.Generation = &protocol.Generation{}
		}
		if response.Result.Generation.Timings == nil {
			response.Result.Generation.Timings = &protocol.GenerationTimings{}
		}
		if task.DispatchAttempts > 0 {
			if response.Result.Generation.Retries == nil {
				response.Result.Generation.Retries = &protocol.GenerationRetries{}
			}
			retry := &protocol.RetrySummary{Count: task.DispatchAttempts - 1}
			if retry.Count > 0 {
				retry.LastReason = task.DispatchRetryReason
			}
			response.Result.Generation.Retries.GatewayDispatch = retry
		}
		timings := response.Result.Generation.Timings
		total := float64(now.Sub(task.CreatedAt)) / float64(time.Millisecond)
		timings.GatewayTotalMS = &total
		if task.StartedAt != nil {
			queue := float64(task.StartedAt.Sub(task.CreatedAt)) / float64(time.Millisecond)
			timings.GatewayQueueMS = &queue
		}
		raw, err := json.Marshal(response.Result)
		if err == nil {
			update.SetResult(raw)
		}
		return err
	})
}

func (s *Store) DispatchFailure(ctx context.Context, id, claim string, status int, code, message string, hold bool) error {
	return s.updateExecution(ctx, id, claim, func(task *ent.Request, update *ent.RequestUpdate) error {
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
	return s.updateExecution(ctx, id, claim, func(task *ent.Request, update *ent.RequestUpdate) error {
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
