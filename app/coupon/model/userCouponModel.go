package model

import (
	"context"
	"fmt"
	"math"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ UserCouponModel = (*customUserCouponModel)(nil)

type (
	// UserCouponModel is an interface to be customized, add more methods here,
	// and implement the added methods in customUserCouponModel.
	UserCouponModel interface {
		userCouponModel
		withSession(session sqlx.Session) UserCouponModel

		// CountByUserAndTemplate 统计某用户在某模板下已领取的券数量（限领校验用）
		CountByUserAndTemplate(ctx context.Context, userId, templateId int64) (int64, error)

		// CountByUserAndTemplateForUpdate 限领校验的【当前读】版本。
		//
		// 与 CountByUserAndTemplate 的区别是加了 FOR UPDATE：会读取最新已提交的数据
		// 并加锁（RR 隔离级别下还会加 gap lock，从而阻止并发插入），
		// 用于防止「检查-使用」之间存在时间窗口（TOCTOU）而导致的超领。
		//
		// ⚠️ 必须在事务内调用（配合 WithSession），否则锁会立刻释放，失去意义。
		CountByUserAndTemplateForUpdate(ctx context.Context, userId, templateId int64) (int64, error)

		// Lock 锁定券（CAS：只有「未使用」才能锁定）。返回受影响行数，
		// 0 表示竞争失败（已被别人锁走 / 已核销 / 已过期）
		Lock(ctx context.Context, id, userId int64, orderSn string) (int64, error)

		// FindOneByOrderSn 按订单号查券（核销 / 释放 / 幂等回查用）
		FindOneByOrderSn(ctx context.Context, orderSn string) (*UserCoupon, error)

		// UseByOrderSn 核销（CAS：只有「已锁定」才能核销）。返回受影响行数
		UseByOrderSn(ctx context.Context, orderSn string) (int64, error)

		// ReleaseByOrderSn 释放（CAS：只有「已锁定」才能释放），同时清空 order_sn
		ReleaseByOrderSn(ctx context.Context, orderSn string) (int64, error)

		// FindListByUser 我的券列表。status 传 CouponStatusAll(-1) 表示全部；
		// 传 CouponStatusUnused(0) 时会额外过滤掉已过期的（定时任务的兜底）。
		// 游标分页（id 降序）：lastId 传 0 表示第一页
		FindListByUser(ctx context.Context, userId, status, lastId, pageSize int64) ([]*UserCoupon, error)

		// FindUsableList 查询用户当前可用于指定订单金额的券（未使用 + 未过期 + 满足门槛）
		FindUsableList(ctx context.Context, userId, amount, limit int64) ([]*UserCoupon, error)

		// FindExpiredList 扫描已过期但仍标记为「未使用」的券（定时任务用）
		FindExpiredList(ctx context.Context, limit int64) ([]*UserCoupon, error)

		// ExpireById 把券标记为已过期（CAS：只有「未使用」才能改成「已过期」）
		ExpireById(ctx context.Context, id int64) (int64, error)

		// WithSession 返回一个绑定到指定事务 session 的 model（对外暴露）
		WithSession(session sqlx.Session) UserCouponModel

		// Trans 在一个本地事务中执行 fn。
		//
		// ⚠️ 使用约定：fn 内**所有**数据库操作都必须使用 WithSession(session) 得到的
		// model —— 因为 Trans 只在某一条连接上开启了事务，而直接用 m.conn（连接池）
		// 的操作不会参与这个事务，会静默地变成"事务外的独立写入"。
		Trans(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error
	}

	customUserCouponModel struct {
		*defaultUserCouponModel
	}
)

// NewUserCouponModel returns a model for the database table.
func NewUserCouponModel(conn sqlx.SqlConn) UserCouponModel {
	return &customUserCouponModel{
		defaultUserCouponModel: newUserCouponModel(conn),
	}
}

func (m *customUserCouponModel) withSession(session sqlx.Session) UserCouponModel {
	return NewUserCouponModel(sqlx.NewSqlConnFromSession(session))
}

// CountByUserAndTemplate 统计已领取数量，用于「每人限领」校验。
// 命中索引 idx_user_template (user_id, template_id)。
func (m *customUserCouponModel) CountByUserAndTemplate(ctx context.Context, userId, templateId int64) (int64, error) {
	query := fmt.Sprintf("select count(*) from %s where `user_id` = ? and `template_id` = ?", m.table)

	var count int64
	if err := m.conn.QueryRowCtx(ctx, &count, query, userId, templateId); err != nil {
		return 0, err
	}
	return count, nil
}

// CountByUserAndTemplateForUpdate 见接口注释。
// 关键区别是 SQL 末尾的 for update —— 当前读 + 加锁。
func (m *customUserCouponModel) CountByUserAndTemplateForUpdate(ctx context.Context, userId, templateId int64) (int64, error) {
	query := fmt.Sprintf("select count(*) from %s where `user_id` = ? and `template_id` = ? for update", m.table)

	var count int64
	if err := m.conn.QueryRowCtx(ctx, &count, query, userId, templateId); err != nil {
		return 0, err
	}
	return count, nil
}

// Lock 锁定券。
//
// ⭐ 这是全功能最关键的 SQL —— 用 CAS（Compare-And-Swap）防止同一张券被并发使用：
//
//	UPDATE user_coupon SET status = 1, order_sn = ?, lock_time = now()
//	WHERE id = ? AND user_id = ? AND status = 0
//
// WHERE status = 0 是「比较」，SET status = 1 是「交换」。
// InnoDB 行锁保证**只有一个事务能成功**，影响行数为 0 即竞争失败。
//
// 为什么不用分布式锁？
//
//	能用数据库的一条原子语句解决的事情，不必引入 Redis 锁的超时、续期、
//	误删他人锁等一堆问题。
//
// 为什么不用 SELECT ... FOR UPDATE（悲观锁）？
//
//	悲观锁要在事务里一直持有到提交；而锁券之后还要写订单（甚至还有远程调用），
//	事务会被拉长。CAS 是一条语句，锁窗口极短。
//
// ⚠️ user_id 必须放在 WHERE 里（而不是只在业务代码里判断）——
// 这样「越权使用别人的券」在数据库层就不可能发生。
func (m *customUserCouponModel) Lock(ctx context.Context, id, userId int64, orderSn string) (int64, error) {
	query := fmt.Sprintf("update %s set `status` = ?, `order_sn` = ?, `lock_time` = now() where `id` = ? and `user_id` = ? and `status` = ?", m.table)
	res, err := m.conn.ExecCtx(ctx, query, CouponStatusLocked, orderSn, id, userId, CouponStatusUnused)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FindOneByOrderSn 按订单号查券。命中索引 idx_order_sn。
//
// ⚠️ 刻意**不加 status 过滤** —— 幂等回查时需要它能查到任意状态的券：
//
//	已锁定 → 正常占用中
//	已核销 → 重复收到核销消息（幂等返回成功）
//	已过期 → 状态异常（明确报错）
//
// 而券被释放时 order_sn 会被清空，所以不存在"陈旧的 order_sn"。
func (m *customUserCouponModel) FindOneByOrderSn(ctx context.Context, orderSn string) (*UserCoupon, error) {
	query := fmt.Sprintf("select %s from %s where `order_sn` = ? limit 1", userCouponRows, m.table)

	var resp UserCoupon
	err := m.conn.QueryRowCtx(ctx, &resp, query, orderSn)
	switch err {
	case nil:
		return &resp, nil
	case sqlx.ErrNotFound:
		return nil, ErrNotFound
	default:
		return nil, err
	}
}

// UseByOrderSn 核销券（支付成功后调用）。
//
//	UPDATE ... SET status = 2, use_time = now() WHERE order_sn = ? AND status = 1
//
// 影响行数为 0 时，调用方**不能直接报错** —— 因为可能是重复调用（消息重投），
// 需要回查当前状态判断是否符合预期。幂等判断放在 logic 层做。
func (m *customUserCouponModel) UseByOrderSn(ctx context.Context, orderSn string) (int64, error) {
	query := fmt.Sprintf("update %s set `status` = ?, `use_time` = now() where `order_sn` = ? and `status` = ?", m.table)
	res, err := m.conn.ExecCtx(ctx, query, CouponStatusUsed, orderSn, CouponStatusLocked)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReleaseByOrderSn 释放券（订单取消 / 超时未支付）。
//
//	UPDATE ... SET status = 0, order_sn = '' WHERE order_sn = ? AND status = 1
//
// ⚠️ WHERE 必须是 status = 1（已锁定）—— 防止把**已核销**的券错误地释放回未使用。
// 幂等判断同样放在 logic 层。
func (m *customUserCouponModel) ReleaseByOrderSn(ctx context.Context, orderSn string) (int64, error) {
	query := fmt.Sprintf("update %s set `status` = ?, `order_sn` = '', `lock_time` = NULL where `order_sn` = ? and `status` = ?", m.table)
	res, err := m.conn.ExecCtx(ctx, query, CouponStatusUnused, orderSn, CouponStatusLocked)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FindListByUser 我的券列表。
//
// 游标分页：id 降序（最新的在前）。lastId 传 0 表示第一页，
// 此时用一个极大值让 `id < ?` 恒成立。
//
// 注意 status = 未使用 时额外加了 `expire_time > now()`：
// 这是「定时任务改状态」的**兜底** —— 定时任务可能有延迟，
// 加这个条件能保证用户不会看到一个已经过期却还显示「可用」的券。
func (m *customUserCouponModel) FindListByUser(ctx context.Context, userId, status, lastId, pageSize int64) ([]*UserCoupon, error) {
	if lastId <= 0 {
		lastId = math.MaxInt64
	}

	var (
		query string
		args  []interface{}
	)

	switch {
	case status == CouponStatusAll:
		query = fmt.Sprintf("select %s from %s where `user_id` = ? and `id` < ? order by `id` desc limit ?", userCouponRows, m.table)
		args = []interface{}{userId, lastId, pageSize}

	case status == CouponStatusUnused:
		query = fmt.Sprintf("select %s from %s where `user_id` = ? and `status` = ? and `expire_time` > now() and `id` < ? order by `id` desc limit ?", userCouponRows, m.table)
		args = []interface{}{userId, status, lastId, pageSize}

	default:
		query = fmt.Sprintf("select %s from %s where `user_id` = ? and `status` = ? and `id` < ? order by `id` desc limit ?", userCouponRows, m.table)
		args = []interface{}{userId, status, lastId, pageSize}
	}

	var list []*UserCoupon
	if err := m.conn.QueryRowsCtx(ctx, &list, query, args...); err != nil {
		return nil, err
	}
	return list, nil
}

// FindUsableList 查询可用于该订单金额的券。
//
// 因为 discount_amount / min_amount 在领取时已经快照到本表，
// 这里**不需要 JOIN 券模板表** —— 既简单，也避免了"模板改动影响已发出的券"。
//
// 命中索引 idx_user_status (user_id, status)。
func (m *customUserCouponModel) FindUsableList(ctx context.Context, userId, amount, limit int64) ([]*UserCoupon, error) {
	query := fmt.Sprintf("select %s from %s where `user_id` = ? and `status` = ? and `expire_time` > now() and `min_amount` <= ? order by `discount_amount` desc, `expire_time` asc limit ?", userCouponRows, m.table)

	var list []*UserCoupon
	if err := m.conn.QueryRowsCtx(ctx, &list, query, userId, CouponStatusUnused, amount, limit); err != nil {
		return nil, err
	}
	return list, nil
}

// FindExpiredList 扫描已过期但仍标记为「未使用」的券（定时任务调用）。
// 命中索引 idx_status_expire (status, expire_time)。
func (m *customUserCouponModel) FindExpiredList(ctx context.Context, limit int64) ([]*UserCoupon, error) {
	query := fmt.Sprintf("select %s from %s where `status` = ? and `expire_time` < now() limit ?", userCouponRows, m.table)

	var list []*UserCoupon
	if err := m.conn.QueryRowsCtx(ctx, &list, query, CouponStatusUnused, limit); err != nil {
		return nil, err
	}
	return list, nil
}

// ExpireById 把券标记为已过期。
//
//	UPDATE ... SET status = 3 WHERE id = ? AND status = 0
//
// ⚠️ WHERE 里的 status = 0 是关键：如果这张券在这一瞬间刚好被下单锁定了
// （status 变成 1），这个 UPDATE 影响 0 行 → 不会把「已锁定」的券错误地置为过期。
func (m *customUserCouponModel) ExpireById(ctx context.Context, id int64) (int64, error) {
	query := fmt.Sprintf("update %s set `status` = ? where `id` = ? and `status` = ?", m.table)
	res, err := m.conn.ExecCtx(ctx, query, CouponStatusExpired, id, CouponStatusUnused)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WithSession 见接口注释。
func (m *customUserCouponModel) WithSession(session sqlx.Session) UserCouponModel {
	return m.withSession(session)
}

// Trans 见接口注释。
//
// TransactCtx 会从连接池取一条连接、开启事务、执行 fn，
// fn 返回 nil 则提交，返回 error 则回滚。
func (m *customUserCouponModel) Trans(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error {
	return m.conn.TransactCtx(ctx, fn)
}
