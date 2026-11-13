package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/rabbitmq/amqp091-go"
	"github.com/wgdl666/modelGateway/internal/protocol"
	"github.com/wgdl666/modelGateway/internal/telemetry"
)

// 发布者与消费者各自恢复 AMQP 连接，回查和在途模型调用沿用网关的生命周期。
func (w *Reconciler) runRabbit(label string, work func(*amqp091.Channel, string) error) {
	cfg := w.runtime.Current()
	w.wg.Go(func() {
		for w.ctx.Err() == nil {
			if err := w.rabbitSession(cfg.RabbitMQURL, cfg.RabbitMQQueue, work); err != nil && w.ctx.Err() == nil {
				telemetry.LogError(w.ctx, label, err)
			}
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	})
}

func (w *Reconciler) rabbitSession(uri, queue string, work func(*amqp091.Channel, string) error) error {
	// 用 Pod 主机名标记连接，便于在 RabbitMQ 按副本核对实际消费者。
	hostname, _ := os.Hostname()
	properties := amqp091.NewConnectionProperties()
	properties.SetClientConnectionName("model-gateway/" + hostname)
	conn, err := amqp091.DialConfig(uri, amqp091.Config{
		Dial: amqp091.DefaultDial(5 * time.Second), Properties: properties,
	})
	if err != nil {
		return err
	}
	closeConn := func() { _ = conn.CloseDeadline(time.Now().Add(time.Second)) }
	stop := context.AfterFunc(w.ctx, closeConn)
	defer func() {
		if stop() {
			closeConn()
		}
	}()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		return err
	}
	// 只在建立连接且确认队列后记录一次；不能把消费者启动当成模型或数据库就绪。
	slog.InfoContext(w.ctx, "gateway RabbitMQ session ready", "queue", queue)
	return work(ch, queue)
}

func (w *Reconciler) publishQueued(ch *amqp091.Channel, queue string) error {
	if err := ch.Confirm(false); err != nil {
		return err
	}
	returned := ch.NotifyReturn(make(chan amqp091.Return, 1))
	closed := ch.NotifyClose(make(chan *amqp091.Error, 1))
	// 提交通知负责即时投递；10 秒补偿进程重启、跨副本写入或通知丢失。
	timer := time.NewTimer(0)
	defer timer.Stop()
	for w.ctx.Err() == nil {
		pending, err := w.store.ScheduledPublications(w.ctx)
		if err != nil {
			return err
		}
		wait := 10 * time.Second
		for _, task := range pending {
			if until := time.Until(*task.QueuePublishAt); until > 0 {
				wait = min(wait, until)
				break
			}
			ttl := time.Until(task.DeadlineAt)
			if ttl <= 0 {
				continue
			}
			if err := w.publishID(ch, queue, task.ID, ttl, returned); err != nil {
				return err
			}
			if err := w.store.MarkPublished(w.ctx, task.ID, *task.QueuePublishAt); err != nil {
				return err
			}
		}
		// 一批已到期记录发布完后立即继续，避免批量上限把积压拖到补偿周期。
		if len(pending) == 32 && !pending[len(pending)-1].QueuePublishAt.After(time.Now()) {
			wait = 0
		}
		timer.Reset(wait)
		select {
		case <-w.ctx.Done():
			return nil
		case <-closed:
			return amqp091.ErrClosed
		case <-w.store.PublicationWake():
		case <-timer.C:
		}
	}
	return nil
}

func (w *Reconciler) publishID(ch *amqp091.Channel, queue, id string, ttl time.Duration, returned <-chan amqp091.Return) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	// 消息只携带账本 ID，图片请求体仍由 PostgreSQL 保存。
	confirmation, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", queue, true, false, amqp091.Publishing{
		Body: []byte(id), DeliveryMode: amqp091.Persistent, Expiration: strconv.FormatInt(max(1, ttl.Milliseconds()), 10),
	})
	if err != nil {
		return err
	}
	ack, err := confirmation.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ack {
		return errors.New("RabbitMQ did not confirm request publication")
	}
	// mandatory 消息可能先被退回、再收到 ACK，两者都要核对才能清待投递标记。
	select {
	case <-returned:
		return errors.New("RabbitMQ returned the request publication")
	default:
		return nil
	}
}

func (w *Reconciler) consumeQueued(ch *amqp091.Channel, queue string) error {
	// 每个消费者只预取一条，两个消费者对应每副本网关两个首次调用。
	if err := ch.Qos(1, 0, false); err != nil {
		return err
	}
	deliveries, err := ch.ConsumeWithContext(w.ctx, queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-w.ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return amqp091.ErrClosed
			}
			if id := string(delivery.Body); protocol.ValidID(id) {
				// 领取时保存派发和回查共用的地址。失败时关闭通道，让 broker 重投未确认的消息。
				cfg := w.runtime.Current()
				task, err := w.store.Claim(w.ctx, cfg.Config, id)
				if err != nil {
					return err
				}
				if task != nil {
					if err := w.execute(cfg, task); err != nil {
						return err
					}
				}
			}
			if err := delivery.Ack(false); err != nil {
				return err
			}
		}
	}
}
