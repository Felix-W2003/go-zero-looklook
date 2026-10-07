package coupon

import (
	"context"

	"looklook/app/coupon/cmd/api/internal/svc"
	"looklook/app/coupon/cmd/api/internal/types"
	couponRpc "looklook/app/coupon/cmd/rpc/coupon"
	"looklook/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ClaimLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewClaimLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ClaimLogic {
	return &ClaimLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// Claim 领取优惠券（api 层是薄封装：取 userId → 调 rpc → 转 types）
func (l *ClaimLogic) Claim(req *types.ClaimReq) (resp *types.ClaimResp, err error) {

	//1、从 JWT 解析出的 ctx 里取 userId。
	//   GetUidFromCtx 断言的是 json.Number —— 因为 go-zero 的 token parser 开了
	//   jwt.WithJSONNumber()，claims 里的数字是 json.Number 而不是 float64。
	userId := ctxdata.GetUidFromCtx(l.ctx)

	//2、调用 rpc
	claimResp, err := l.svcCtx.CouponRpc.ClaimCoupon(l.ctx, &couponRpc.ClaimCouponReq{
		UserId:     userId,
		TemplateId: req.TemplateId,
	})
	if err != nil {
		// ⚠️ 这里**不要**再包一层 errors.Wrapf：
		//    rpc 返回的 *xerr.CodeError 必须原样透出，否则前端只能看到"服务器开小差"。
		return nil, err
	}

	//3、返回
	return &types.ClaimResp{CouponCode: claimResp.CouponCode}, nil
}
