package config

import (
	"github.com/zeromicro/go-zero/core/stores/cache"
	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf

	DB struct {
		DataSource string
	}
	Cache cache.CacheConf

	TravelRpcConf zrpc.RpcClientConf

	//优惠券服务（下单锁券 / 取消释放券）
	CouponRpcConf zrpc.RpcClientConf
}
