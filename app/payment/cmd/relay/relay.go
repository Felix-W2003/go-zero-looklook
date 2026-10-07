package main

import (
	"context"
	"flag"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"looklook/app/payment/model"
)

var configFile = flag.String("f", "app/payment/cmd/relay/etc/relay.yaml", "the config file")

type Config struct {
	Log logx.LogConf
	DB  struct {
		DataSource string
	}
	KqPaymentUpdatePayStatusConf struct {
		Brokers []string
		Topic   string
	}
	Relay struct {
		BatchSize    int `json:",default=100"`
		IntervalMs   int `json:",default=500"`
		MaxRetry     int `json:",default=10"`
		KeepSentDays int `json:",default=7"`
	}
}

func main() {
	flag.Parse()

	var c Config
	conf.MustLoad(*configFile, &c)

	conn := sqlx.NewMysql(c.DB.DataSource)
	outboxModel := model.NewOutboxModel(conn)

	// ★ 这里刻意不用 kq.Pusher：
	//   它的 Push 是缓冲的（返回 nil 不代表已发出）、真实错误会被吞掉、
	//   且用纳秒时间戳当 Kafka key 会导致同一实体的消息乱序。
	writer := &kafka.Writer{
		Addr:         kafka.TCP(c.KqPaymentUpdatePayStatusConf.Brokers...),
		Topic:        c.KqPaymentUpdatePayStatusConf.Topic,
		Balancer:     &kafka.Hash{},    // 按 key 分区 → 同一订单的消息有序
		RequiredAcks: kafka.RequireAll, // 等所有 ISR 确认，才算投递成功
		Async:        false,            // 同步发送，才能拿到真实错误
	}
	defer writer.Close()

	logx.Infof("payment-relay started: topic=%s batch=%d interval=%dms maxRetry=%d",
		c.KqPaymentUpdatePayStatusConf.Topic, c.Relay.BatchSize, c.Relay.IntervalMs, c.Relay.MaxRetry)

	ticker := time.NewTicker(time.Duration(c.Relay.IntervalMs) * time.Millisecond)
	defer ticker.Stop()

	cleanupTicker := time.NewTicker(time.Hour)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := relayOnce(ctx, outboxModel, writer, c.Relay.BatchSize, c.Relay.MaxRetry); err != nil {
				logx.Errorf("relayOnce err: %v", err)
			}
			cancel()

		case <-cleanupTicker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if n, err := outboxModel.DeleteSentBefore(ctx, time.Now().AddDate(0, 0, -c.Relay.KeepSentDays)); err != nil {
				logx.Errorf("cleanup sent outbox err: %v", err)
			} else if n > 0 {
				logx.Infof("cleanup sent outbox: %d rows deleted", n)
			}
			cancel()
		}
	}
}

// relayOnce 扫描一批到期的待投递消息并投递。
func relayOnce(ctx context.Context, m model.OutboxModel, w *kafka.Writer, batchSize, maxRetry int) error {
	return m.Trans(ctx, func(ctx context.Context, session sqlx.Session) error {
		// FOR UPDATE SKIP LOCKED：多实例并行时各取各的，不重复、不阻塞
		list, err := m.FetchPending(ctx, session, batchSize)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			return nil
		}

		for _, msg := range list {
			km := kafka.Message{
				Key:   []byte(msg.MsgKey),
				Value: []byte(msg.Payload),
			}

			if err := w.WriteMessages(ctx, km); err != nil {
				// 重试超限 → 转死信，不再无限重试拖累队列
				if msg.RetryCount+1 >= int64(maxRetry) {
					if e := m.MarkDead(ctx, session, msg.Id, err.Error()); e != nil {
						return e
					}
					logx.Errorf("outbox id=%d 重试 %d 次仍失败，已转死信: %v", msg.Id, msg.RetryCount+1, err)
					continue
				}

				// 指数退避：1s, 2s, 4s, 8s ...
				backoff := time.Duration(1<<uint(msg.RetryCount)) * time.Second
				if e := m.MarkRetry(ctx, session, msg.Id, msg.RetryCount+1, time.Now().Add(backoff), err.Error()); e != nil {
					return e
				}
				logx.Errorf("outbox id=%d 投递失败（第 %d 次），%s 后重试: %v", msg.Id, msg.RetryCount+1, backoff, err)
				continue
			}

			if err := m.MarkSent(ctx, session, msg.Id); err != nil {
				return err
			}
			logx.Infof("outbox id=%d 投递成功 key=%s", msg.Id, msg.MsgKey)
		}
		return nil
	})
}
