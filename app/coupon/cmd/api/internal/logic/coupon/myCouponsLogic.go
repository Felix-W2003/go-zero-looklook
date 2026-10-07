package coupon

import (
	"context"

	"looklook/app/coupon/cmd/api/internal/svc"
	"looklook/app/coupon/cmd/api/internal/types"
	couponRpc "looklook/app/coupon/cmd/rpc/coupon"
	"looklook/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type MyCouponsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewMyCouponsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MyCouponsLogic {
	return &MyCouponsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MyCoupons 我的优惠券（需要登录）
//
// ⚠️ 关于 userId 的来源：**只从 JWT 解析出的 ctx 里取，绝不从请求参数里取**。
//    否则任何人都能伪造 userId 查看别人的券。
func (l *MyCouponsLogic) MyCoupons(req *types.MyCouponsReq) (resp *types.MyCouponsResp, err error) {

	userId := ctxdata.GetUidFromCtx(l.ctx)

	rpcResp, err := l.svcCtx.CouponRpc.ListUserCoupons(l.ctx, &couponRpc.ListUserCouponsReq{
		UserId:   userId,
		Status:   req.Status,
		LastId:   req.LastId,
		PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}

	resp = &types.MyCouponsResp{
		List: make([]types.UserCoupon, 0, len(rpcResp.List)),
	}
	for _, c := range rpcResp.List {
		resp.List = append(resp.List, types.UserCoupon{
			CouponCode:     c.CouponCode,
			Name:           c.Name,
			DiscountAmount: c.DiscountAmount,
			MinAmount:      c.MinAmount,
			Status:         c.Status,
			ExpireTime:     c.ExpireTime,
		})
	}

	return resp, nil
}
