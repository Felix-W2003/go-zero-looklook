package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ CouponTemplateModel = (*customCouponTemplateModel)(nil)

type (
	// CouponTemplateModel is an interface to be customized, add more methods here,
	// and implement the added methods in customCouponTemplateModel.
	CouponTemplateModel interface {
		couponTemplateModel
		withSession(session sqlx.Session) CouponTemplateModel

		// IncrIssuedCount 原子地占用一个发行名额（CAS）。
		// 返回受影响行数：0 表示已领完（并发竞争失败）。
		IncrIssuedCount(ctx context.Context, id int64) (int64, error)

		// FindAvailableList 查询当前可领取的券模板（上架中 + 在有效期内 + 还有剩余）。
		// 游标分页：lastId 传 0 表示第一页。
		FindAvailableList(ctx context.Context, lastId, pageSize int64) ([]*CouponTemplate, error)

		// WithSession 返回一个绑定到指定事务 session 的 model（对外暴露）
		WithSession(session sqlx.Session) CouponTemplateModel

		// Trans 在一个本地事务中执行 fn。
		//
		// ⚠️ 使用约定：fn 内**所有**数据库操作都必须使用 WithSession(session) 得到的
		// model —— 因为 Trans 只在某一条连接上开启了事务，而直接用 m.conn（连接池）
		// 的操作不会参与这个事务，会静默地变成"事务外的独立写入"。
		Trans(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error
	}

	customCouponTemplateModel struct {
		*defaultCouponTemplateModel
	}
)

// NewCouponTemplateModel returns a model for the database table.
func NewCouponTemplateModel(conn sqlx.SqlConn) CouponTemplateModel {
	return &customCouponTemplateModel{
		defaultCouponTemplateModel: newCouponTemplateModel(conn),
	}
}

func (m *customCouponTemplateModel) withSession(session sqlx.Session) CouponTemplateModel {
	return NewCouponTemplateModel(sqlx.NewSqlConnFromSession(session))
}

// IncrIssuedCount 原子地占用一个发行名额。
//
// ⭐ 这是「秒杀防超卖」的标准解法：把「判断是否领完」和「递增」合并成**一条 SQL**。
//
//	UPDATE ... SET issued_count = issued_count + 1
//	WHERE id = ? AND issued_count < total_count
//
// 为什么不能「先 SELECT 判断、再 UPDATE」？
//
//	两个并发请求可能都读到 issued_count = 99 < 100，然后各自 UPDATE，
//	结果发出 101 张 —— 这就是竞态导致的超发。
//
// 为什么这一条 SQL 就没有竞态？
//
//	InnoDB 在执行 UPDATE 时会对该行加**排他锁**，第二个请求必须等第一个提交后
//	才能重新读取该行的最新值，此时 issued_count 已变成 100，条件不成立 → 影响 0 行。
//	并发被数据库天然串行化了。
func (m *customCouponTemplateModel) IncrIssuedCount(ctx context.Context, id int64) (int64, error) {
	query := fmt.Sprintf("update %s set `issued_count` = `issued_count` + 1 where `id` = ? and `issued_count` < `total_count`", m.table)
	res, err := m.conn.ExecCtx(ctx, query, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FindAvailableList 查询当前可领取的券模板。
// 游标分页（id 升序）：lastId 传 0 表示从第一页开始。
func (m *customCouponTemplateModel) FindAvailableList(ctx context.Context, lastId, pageSize int64) ([]*CouponTemplate, error) {
	query := fmt.Sprintf("select %s from %s where `status` = ? and `valid_start` <= now() and `valid_end` >= now() and `issued_count` < `total_count` and `id` > ? order by `id` asc limit ?", couponTemplateRows, m.table)

	var list []*CouponTemplate
	if err := m.conn.QueryRowsCtx(ctx, &list, query, CouponTemplateStatusOn, lastId, pageSize); err != nil {
		return nil, err
	}
	return list, nil
}

// WithSession 见接口注释。
func (m *customCouponTemplateModel) WithSession(session sqlx.Session) CouponTemplateModel {
	return m.withSession(session)
}

// Trans 见接口注释。
//
// TransactCtx 会从连接池取一条连接、开启事务、执行 fn，
// fn 返回 nil 则提交，返回 error 则回滚。
func (m *customCouponTemplateModel) Trans(ctx context.Context, fn func(ctx context.Context, session sqlx.Session) error) error {
	return m.conn.TransactCtx(ctx, fn)
}
