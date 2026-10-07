package coupon

import (
	"context"

	"looklook/app/coupon/cmd/api/internal/svc"
	"looklook/app/coupon/cmd/api/internal/types"
	couponRpc "looklook/app/coupon/cmd/rpc/coupon"
	"looklook/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type PreviewCouponLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewPreviewCouponLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PreviewCouponLogic {
	return &PreviewCouponLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PreviewCoupon 下单前预览可用券（需要登录）
//
// 传入订单原价（分），返回该用户当前可用的券及抵扣后的实付金额。
// 只读操作 —— 真正的占用发生在下单时的 LockCoupon。
func (l *PreviewCouponLogic) PreviewCoupon(req *types.PreviewCouponReq) (resp *types.PreviewCouponResp, err error) {

	userId := ctxdata.GetUidFromCtx(l.ctx)

	rpcResp, err := l.svcCtx.CouponRpc.PreviewCoupon(l.ctx, &couponRpc.PreviewCouponReq{
		UserId: userId,
		Amount: req.Amount,
	})
	if err != nil {
		return nil, err
	}

	resp = &types.PreviewCouponResp{
		List: make([]types.PreviewCouponItem, 0, len(rpcResp.List)),
	}
	for _, c := range rpcResp.List {
		resp.List = append(resp.List, types.PreviewCouponItem{
			CouponCode:     c.CouponCode,
			DiscountAmount: c.DiscountAmount,
			FinalAmount:    c.FinalAmount,
		})
	}

	return resp, nil
}
