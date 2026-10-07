package svc

import (
	"looklook/app/coupon/cmd/api/internal/config"
	"looklook/app/coupon/cmd/rpc/coupon"

	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config config.Config

	CouponRpc coupon.Coupon
}

func NewServiceContext(c config.Config) *ServiceContext {
	return &ServiceContext{
		Config:    c,
		CouponRpc: coupon.NewCoupon(zrpc.MustNewClient(c.CouponRpcConf)),
	}
}
