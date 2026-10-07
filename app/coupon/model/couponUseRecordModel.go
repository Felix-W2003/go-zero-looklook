package model

import (
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

var _ CouponUseRecordModel = (*customCouponUseRecordModel)(nil)

type (
	// CouponUseRecordModel is an interface to be customized, add more methods here,
	// and implement the added methods in customCouponUseRecordModel.
	CouponUseRecordModel interface {
		couponUseRecordModel
		withSession(session sqlx.Session) CouponUseRecordModel

		// WithSession 返回一个绑定到指定事务 session 的 model（对外暴露，供 logic 层
		// 把多个写操作放进同一个本地事务）。
		WithSession(session sqlx.Session) CouponUseRecordModel
	}

	customCouponUseRecordModel struct {
		*defaultCouponUseRecordModel
	}
)

// NewCouponUseRecordModel returns a model for the database table.
func NewCouponUseRecordModel(conn sqlx.SqlConn) CouponUseRecordModel {
	return &customCouponUseRecordModel{
		defaultCouponUseRecordModel: newCouponUseRecordModel(conn),
	}
}

func (m *customCouponUseRecordModel) withSession(session sqlx.Session) CouponUseRecordModel {
	return NewCouponUseRecordModel(sqlx.NewSqlConnFromSession(session))
}

// WithSession 见接口注释。
func (m *customCouponUseRecordModel) WithSession(session sqlx.Session) CouponUseRecordModel {
	return m.withSession(session)
}
