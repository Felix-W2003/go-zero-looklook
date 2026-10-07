package svc

import (
	"looklook/app/coupon/cmd/rpc/internal/config"
	"looklook/app/coupon/model"
	"looklook/app/usercenter/cmd/rpc/usercenter"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config config.Config

	UsercenterRpc usercenter.Usercenter

	CouponTemplateModel  model.CouponTemplateModel
	UserCouponModel      model.UserCouponModel
	CouponUseRecordModel model.CouponUseRecordModel
}

func NewServiceContext(c config.Config) *ServiceContext {

	sqlConn := sqlx.NewMysql(c.DB.DataSource)

	return &ServiceContext{
		Config:        c,
		UsercenterRpc: usercenter.NewUsercenter(zrpc.MustNewClient(c.UsercenterRpcConf)),

		CouponTemplateModel:  model.NewCouponTemplateModel(sqlConn),
		UserCouponModel:      model.NewUserCouponModel(sqlConn),
		CouponUseRecordModel: model.NewCouponUseRecordModel(sqlConn),
	}
}
