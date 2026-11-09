package testsupport

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rabbitmq/amqp091-go"
)

// RabbitQueue 为每个测试隔离持久队列，测试退出后清理该队列。
func RabbitQueue(t testing.TB) (string, string, *amqp091.Channel) {
	t.Helper()
	uri := os.Getenv("TEST_RABBITMQ_URL")
	if uri == "" {
		t.Skip("TEST_RABBITMQ_URL required for RabbitMQ integration test")
	}
	conn, err := amqp091.DialConfig(uri, amqp091.Config{Dial: amqp091.DefaultDial(5 * time.Second)})
	if err != nil {
		t.Fatalf("connect to test RabbitMQ: %T", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	queue := "model-gateway-test-" + uuid.NewString()
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := ch.QueueDelete(queue, false, false, false); err != nil {
			t.Errorf("delete test queue: %v", err)
		}
	})
	return uri, queue, ch
}
