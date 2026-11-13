package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/store"
)

func TestSynchronousCompletionRecoversAfterRowLockTimeout(t *testing.T) {
	tasks, _, db := stores(t)
	cfg := testConfig()
	cfg.VLMAddr = "172.31.3.219:30332"
	id := uuid.NewString()
	var calls atomic.Int32
	released := make(chan error, 1)
	output, err := os.CreateTemp(t.TempDir(), "completion-log")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(output, nil)))
	defer slog.SetDefault(previous)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		tx, err := db.Tx(t.Context())
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		if _, err = tx.Request.Query().Where(request.IDEQ(id)).ForUpdate().Only(t.Context()); err != nil {
			tx.Rollback()
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		// 模型只执行一次，终态写入的第一轮在行锁上耗尽原来的 3 秒预算。
		go func() { time.Sleep(store.ExecutionWriteTimeout + 350*time.Millisecond); released <- tx.Rollback() }()
		io.WriteString(w, `{"result":"done"}`)
	}))
	defer upstream.Close()
	spec := testInstance(protocol.DreamModel, "http://"+cfg.VLMAddr, 1)
	registerReady(t, tasks, spec)
	base := synchronousServer(t, tasks, cfg, upstream)
	resp := sendSynchronous(t, base, "/v1/chat/completions", "application/json", `{"model":"Vmind_dream_v1"}`, id)
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = <-released; err != nil {
		t.Fatal(err)
	}
	row := waitSynchronousState(t, db, id, request.StateSucceeded, false)
	if calls.Load() != 1 || resp.StatusCode != 200 || string(body) != `{"result":"done"}` || row.DispatchAttempts != 1 {
		t.Fatalf("model replay or missing response: calls=%d status=%d body=%s", calls.Load(), resp.StatusCode, body)
	}
	raw, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"db.phase":"lock_request"`) || !strings.Contains(string(raw), "synchronous outcome persistence retry") {
		t.Fatalf("missing failed stage/retry: %s", raw)
	}
}

func TestCompletionRecoversAfterConnectionPoolTimeout(t *testing.T) {
	tasks, _, db := stores(t)
	cfg := testConfig()
	cfg.VLMAddr = "172.31.3.219:30332"
	spec := testInstance(protocol.DreamModel, "http://"+cfg.VLMAddr, 1)
	registerReady(t, tasks, spec)
	id := uuid.NewString()
	if _, err := tasks.Admit(t.Context(), testAdmission(spec, id, 1)); err != nil {
		t.Fatal(err)
	}
	row, err := tasks.Claim(t.Context(), cfg, id)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	pg, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if pg.RuntimeParams["search_path"] == "" {
		pg.RuntimeParams["search_path"] = "model_gateway"
	}
	pool := stdlib.OpenDB(*pg)
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	client := ent.NewClient(ent.Driver(sql.OpenDB(dialect.Postgres, pool)))
	completion := store.New(client)
	held, err := pool.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() { time.Sleep(store.ExecutionWriteTimeout + 350*time.Millisecond); released <- held.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), store.SynchronousWriteTimeout)
	defer cancel()
	if err = completion.CompleteSynchronous(ctx, id, row.PollClaim, protocol.HTTPResult{StatusCode: 200}, true); err != nil {
		t.Fatal(err)
	}
	if err = <-released; err != nil {
		t.Fatal(err)
	}
	after := waitSynchronousState(t, db, id, request.StateSucceeded, false)
	if pool.Stats().WaitCount < 2 {
		t.Fatal("test did not exercise failed first connection acquisition and retry")
	}
	// 重复完成及旧轮次不能覆盖已保存结果，也不能改写更新时间。
	if err = completion.CompleteSynchronous(ctx, id, row.PollClaim, protocol.HTTPResult{StatusCode: 502}, true); err != nil {
		t.Fatal(err)
	}
	duplicate, err := db.Request.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.State != request.StateSucceeded || !duplicate.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("duplicate completion rewrote terminal state")
	}
}
