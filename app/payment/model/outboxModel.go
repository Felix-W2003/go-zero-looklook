package model

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Outbox 事务性发件箱记录。
//
// 用途：把「待发送的消息」与业务数据放进同一个本地事务，
// 消除「更新数据库」与「发送 MQ 消息」之间的双写不一致。
type Outbox struct {
	Id          int64        `db:"id"`
	Topic       string       `db:"topic"`
	MsgKey      string       `db:"msg_key"`
	Payload     string       `db:"payload"`
	Status      int64        `db:"status"`
	RetryCount  int64        `db:"retry_count"`
	NextRetryAt time.Time    `db:"next_retry_at"`
	LastError   string       `db:"last_error"`
	CreatedAt   time.Time    `db:"created_at"`
	SentAt      sql.NullTime `db:"sent_at"`
}

const (
	OutboxStatusPending int64 = 0 // 待投递
	OutboxStatusSent    int64 = 1 // 已投递
	OutboxStatusDead    int64 = 2 // 已死信（重试超限，需人工介入）
)

type OutboxModel interface {
	Insert(ctx context.Context, session sqlx.Session, data *Outbox) (sql.Result, error)
	FetchPending(ctx context.Context, session sqlx.Session, limit int) ([]*Outbox, error)
	MarkSent(ctx context.Context, session sqlx.Session, id int64) error
	MarkRetry(ctx context.Context, session sqlx.Session, id, retryCount int64, nextRetryAt time.Time, lastErr string) error
	MarkDead(ctx context.Context, session sqlx.Session, id int64, lastErr string) error
	DeleteSentBefore(ctx context.Context, before time.Time) (int64, error)
	Trans(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error
}

type defaultOutboxModel struct {
	conn  sqlx.SqlConn
	table string
}

func NewOutboxModel(conn sqlx.SqlConn) OutboxModel {
	return &defaultOutboxModel{conn: conn, table: "outbox"}
}

func (m *defaultOutboxModel) Insert(ctx context.Context, session sqlx.Session, data *Outbox) (sql.Result, error) {
	query := fmt.Sprintf("insert into %s (`topic`, `msg_key`, `payload`, `status`, `next_retry_at`) values (?, ?, ?, ?, ?)", m.table)
	if session != nil {
		return session.ExecCtx(ctx, query, data.Topic, data.MsgKey, data.Payload, data.Status, time.Now())
	}
	return m.conn.ExecCtx(ctx, query, data.Topic, data.MsgKey, data.Payload, data.Status, time.Now())
}

// FetchPending 取一批已到期的待投递记录。
//
// FOR UPDATE SKIP LOCKED：多个 relay 实例并行时各取各的，
// 既不重复投递，也不会互相阻塞。
func (m *defaultOutboxModel) FetchPending(ctx context.Context, session sqlx.Session, limit int) ([]*Outbox, error) {
	query := fmt.Sprintf("select id, topic, msg_key, payload, status, retry_count, next_retry_at, last_error, created_at, sent_at from %s where `status` = ? and `next_retry_at` <= ? order by `id` asc limit ? for update skip locked", m.table)

	var queryRows func(ctx context.Context, v interface{}, q string, args ...interface{}) error = m.conn.QueryRowsCtx
	if session != nil {
		queryRows = session.QueryRowsCtx
	}

	var list []*Outbox
	if err := queryRows(ctx, &list, query, OutboxStatusPending, time.Now(), limit); err != nil {
		return nil, err
	}
	return list, nil
}

func (m *defaultOutboxModel) MarkSent(ctx context.Context, session sqlx.Session, id int64) error {
	query := fmt.Sprintf("update %s set `status` = ?, `sent_at` = ? where `id` = ?", m.table)
	if session != nil {
		_, err := session.ExecCtx(ctx, query, OutboxStatusSent, time.Now(), id)
		return err
	}
	_, err := m.conn.ExecCtx(ctx, query, OutboxStatusSent, time.Now(), id)
	return err
}

// MarkRetry 记录一次投递失败，并设置下次重试时间（指数退避由调用方计算）。
func (m *defaultOutboxModel) MarkRetry(ctx context.Context, session sqlx.Session, id, retryCount int64, nextRetryAt time.Time, lastErr string) error {
	query := fmt.Sprintf("update %s set `retry_count` = ?, `next_retry_at` = ?, `last_error` = ? where `id` = ?", m.table)
	if session != nil {
		_, err := session.ExecCtx(ctx, query, retryCount, nextRetryAt, lastErr, id)
		return err
	}
	_, err := m.conn.ExecCtx(ctx, query, retryCount, nextRetryAt, lastErr, id)
	return err
}

// MarkDead 标记为死信：重试超过上限，不再自动重试，需人工介入。
func (m *defaultOutboxModel) MarkDead(ctx context.Context, session sqlx.Session, id int64, lastErr string) error {
	query := fmt.Sprintf("update %s set `status` = ?, `last_error` = ? where `id` = ?", m.table)
	if session != nil {
		_, err := session.ExecCtx(ctx, query, OutboxStatusDead, lastErr, id)
		return err
	}
	_, err := m.conn.ExecCtx(ctx, query, OutboxStatusDead, lastErr, id)
	return err
}

// DeleteSentBefore 清理已投递成功的历史记录，避免表无限膨胀。
func (m *defaultOutboxModel) DeleteSentBefore(ctx context.Context, before time.Time) (int64, error) {
	query := fmt.Sprintf("delete from %s where `status` = ? and `sent_at` < ? limit 1000", m.table)
	res, err := m.conn.ExecCtx(ctx, query, OutboxStatusSent, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (m *defaultOutboxModel) Trans(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error {
	return m.conn.TransactCtx(ctx, fn)
}
