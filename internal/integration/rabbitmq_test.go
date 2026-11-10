package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rabbitmq/amqp091-go"
	"github.com/wgdl666/modelGateway/ent"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/testsupport"
	"github.com/wgdl666/modelGateway/internal/worker"
)

func TestRabbitMQRequeuesOnlyConfirmedWorkerBusy(t *testing.T) {
	ledger, _, db := stores(t)
	cfg := testConfig()
	uri, name, ch := testsupport.RabbitQueue(t)
	cfg.RabbitMQURL, cfg.RabbitMQQueue = uri, name

	const busyBody = `{"error":{"code":"worker_busy","execution_started":false}}`
	busyID := uuid.NewString()
	var busyCalls atomic.Int32
	terminal := map[string]*struct {
		status int
		body   string
		calls  atomic.Int32
	}{
		uuid.NewString(): {status: http.StatusUnprocessableEntity, body: busyBody},
		uuid.NewString(): {status: http.StatusTooManyRequests, body: `{"error":{"code":"worker_busy"}}`},
		uuid.NewString(): {status: http.StatusTooManyRequests, body: `{"error":{"code":"worker_busy","execution_started":true}}`},
		uuid.NewString(): {status: http.StatusTooManyRequests, body: `{"error":{"code":"invalid_input","execution_started":false}}`},
	}
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /health":
			_, _ = io.WriteString(w, `{"ready":true}`)
		case "POST /requests":
			var input protocol.ModelRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.ModelKey != protocol.OutfitModel {
				t.Errorf("invalid Worker dispatch: model=%q err=%v", input.ModelKey, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if failure := terminal[input.RequestID]; failure != nil {
				failure.calls.Add(1)
				w.WriteHeader(failure.status)
				_, _ = io.WriteString(w, failure.body)
				return
			}
			if input.RequestID != busyID {
				t.Errorf("retry changed the request ID to %s", input.RequestID)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if busyCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, busyBody)
				return
			}
			_ = json.NewEncoder(w).Encode(succeeded(input.RequestID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer model.Close()
	endpoint, _ := url.Parse(model.URL)
	cfg.AllowedPorts = append(cfg.AllowedPorts, endpoint.Port())
	spec := testInstance(protocol.OutfitModel, model.URL, len(terminal)+1)
	registerReady(t, ledger, spec)
	admitted, err := ledger.Admit(t.Context(), testAdmission(spec, busyID, 1))
	if err != nil {
		t.Fatal(err)
	}
	// PostgreSQL persists microseconds; compare the persisted deadline, not
	// the nanosecond timestamp returned by an in-memory insert result.
	admitted, err = db.Request.Get(t.Context(), admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(ledger, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	reconciler.Start()

	waitForTask := func(id string, ready func(*ent.Request) bool) *ent.Request {
		t.Helper()
		for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
			task, err := db.Request.Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if ready(task) {
				return task
			}
		}
		t.Fatalf("request %s did not reach the expected state", id)
		return nil
	}
	queued := waitForTask(busyID, func(task *ent.Request) bool {
		return busyCalls.Load() == 1 && task.State == request.StateQueued && task.QueuePublishAt != nil
	})
	if queued.SlotReserved || queued.InstanceID != "" || queued.StartedAt != nil || queued.NextPollAt != nil ||
		!queued.DeadlineAt.Equal(admitted.DeadlineAt) {
		t.Fatalf("requeued request kept its dispatch reservation or changed its deadline: %+v", queued)
	}
	// 模拟旧投递重现及迟到的发布确认，两者都不能提前执行或丢掉下一次投递。
	if err := ch.PublishWithContext(t.Context(), "", name, true, false, amqp091.Publishing{Body: []byte(busyID)}); err != nil {
		t.Fatal(err)
	}
	waitForTask(busyID, func(task *ent.Request) bool {
		return task.State == request.StateQueued && task.QueuePublishAt != nil && task.QueuePublishAt.After(*queued.QueuePublishAt)
	})
	if err := ledger.MarkPublished(t.Context(), busyID, *queued.QueuePublishAt); err != nil {
		t.Fatal(err)
	}
	finished := waitForTask(busyID, func(task *ent.Request) bool { return task.State == request.StateSucceeded })
	if busyCalls.Load() != 2 || finished.SlotReserved || finished.StartedAt == nil ||
		finished.StartedAt.Before(*queued.QueuePublishAt) || !finished.DeadlineAt.Equal(admitted.DeadlineAt) {
		t.Fatalf("busy request did not finish through a single delayed redispatch: %+v, calls=%d", finished, busyCalls.Load())
	}

	for id, failure := range terminal {
		if _, err := ledger.Admit(t.Context(), testAdmission(spec, id, 1)); err != nil {
			t.Fatal(err)
		}
		failed := waitForTask(id, func(task *ent.Request) bool { return task.State == request.StateFailed })
		if failure.calls.Load() != 1 || failed.FinishedAt == nil || failed.QueuePublishAt != nil {
			t.Fatalf("HTTP %d without a safe retry should fail after one dispatch: %+v", failure.status, failed)
		}
	}
}

func TestRabbitMQDispatchRetriesUnavailableModelAndDeduplicates(t *testing.T) {
	ledger, _, db := stores(t)
	cfg := testConfig()
	// 从独立 AMQP 连接观察消费者数量和模拟消息重投。
	uri, name, ch := testsupport.RabbitQueue(t)
	cfg.RabbitMQURL, cfg.RabbitMQQueue = uri, name

	waitingID, readyID := uuid.NewString(), uuid.NewString()
	var waitingCalls, readyCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			_ = json.NewEncoder(w).Encode(map[string]bool{"ready": true})
		case r.Method == http.MethodPost && r.URL.Path == "/requests":
			var body protocol.ModelRequest
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			switch body.RequestID {
			case waitingID:
				if body.ModelKey != protocol.VideoModel {
					t.Errorf("wrong worker model key %q", body.ModelKey)
				}
				waitingCalls.Add(1)
			case readyID:
				if body.ModelKey != protocol.OutfitModel {
					t.Errorf("wrong worker model key %q", body.ModelKey)
				}
				readyCalls.Add(1)
			default:
				t.Errorf("unexpected request ID %q", body.RequestID)
			}
			_ = json.NewEncoder(w).Encode(succeeded(body.RequestID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer model.Close()
	endpoint, _ := url.Parse(model.URL)
	cfg.AllowedPorts = append(cfg.AllowedPorts, endpoint.Port())
	waiting := testInstance(protocol.VideoModel, model.URL, 1)
	ready := testInstance(protocol.OutfitModel, model.URL, 1)
	registerReady(t, ledger, ready)
	// 模拟进程尚未启动发布循环时已受理的请求，旧请求没有模型也不能卡住下一条。
	if _, err := ledger.Admit(t.Context(), testAdmission(waiting, waitingID, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Admit(t.Context(), testAdmission(ready, readyID, 1)); err != nil {
		t.Fatal(err)
	}
	runtime, err := config.NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := worker.New(ledger, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.Close()
	reconciler.Start()

	waitSucceeded := func(id string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			task, err := db.Request.Get(t.Context(), id)
			if err != nil || task.State == request.StateFailed {
				t.Fatalf("request failed before success: %v %v", task, err)
			}
			if task.State == request.StateSucceeded && !task.SlotReserved {
				return
			}
		}
		t.Fatalf("request %s was not dispatched to completion", id)
	}
	waitSucceeded(readyID)
	pending, err := db.Request.Get(t.Context(), waitingID)
	if err != nil || pending.State != request.StateQueued || pending.SlotReserved {
		t.Fatalf("unavailable model request did not stay queued: %v %v", pending, err)
	}
	if err := ch.PublishWithContext(t.Context(), "", cfg.RabbitMQQueue, true, false, amqp091.Publishing{Body: []byte(readyID)}); err != nil {
		t.Fatal(err)
	}
	registerReady(t, ledger, waiting)
	waitSucceeded(waitingID)
	queue, err := ch.QueueInspect(cfg.RabbitMQQueue)
	if err != nil || queue.Consumers != 2 {
		t.Fatalf("consumer count=%d err=%v", queue.Consumers, err)
	}
	if readyCalls.Load() != 1 || waitingCalls.Load() != 1 {
		t.Fatalf("model dispatch count ready=%d waiting=%d", readyCalls.Load(), waitingCalls.Load())
	}
}
