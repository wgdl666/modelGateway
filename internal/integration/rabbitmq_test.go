package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rabbitmq/amqp091-go"
	"github.com/wgdl666/modelGateway/ent/request"
	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/testsupport"
	"github.com/wgdl666/modelGateway/internal/worker"
)

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
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			switch body.RequestID {
			case waitingID:
				waitingCalls.Add(1)
			case readyID:
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
	waiting := testInstance(protocol.VideoModel, "video", model.URL, 1)
	ready := testInstance(protocol.OutfitModel, "vton", model.URL, 1)
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
