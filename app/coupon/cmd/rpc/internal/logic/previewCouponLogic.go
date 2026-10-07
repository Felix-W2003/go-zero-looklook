package logic

import (
	"context"

	"looklook/app/coupon/cmd/rpc/internal/svc"
	"looklook/app/coupon/cmd/rpc/pb"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type PreviewCouponLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPreviewCouponLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PreviewCouponLogic {
	return &PreviewCouponLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PreviewCoupon 下单前预览可用券
//
// 输入订单原价（分），返回该用户所有可用券及抵扣后的实付金额。
// 这是**只读**操作，不锁券、不改状态 —— 真正的占用发生在 LockCoupon。
func (l *PreviewCouponLogic) PreviewCoupon(in *pb.PreviewCouponReq) (*pb.PreviewCouponResp, error) {

	if in.UserId <= 0 || in.Amount < 0 {
		return nil, xerr.NewErrCode(xerr.REUQEST_PARAM_ERROR)
	}

	// 取最多 20 张可用券（已按优惠金额降序、过期时间升序排好）
	list, err := l.svcCtx.UserCouponModel.FindUsableList(l.ctx, in.UserId, in.Amount, 20)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "FindUsableList err:%v , in:%+v", err, in)
	}

	resp := &pb.PreviewCouponResp{
		List: make([]*pb.PreviewCouponItem, 0, len(list)),
	}
	for _, c := range list {
		// ⭐ 抵扣后金额不能为负。
		//    满减券的优惠额有可能大于订单金额（例如"满 0 减 10"的券用在 5 元订单上），
		//    这里必须夹住下界，否则会出现负数金额。
		finalAmount := in.Amount - c.DiscountAmount
		if finalAmount < 0 {
			finalAmount = 0
		}

		resp.List = append(resp.List, &pb.PreviewCouponItem{
			CouponCode:     c.CouponCode,
			DiscountAmount: c.DiscountAmount,
			FinalAmount:    finalAmount,
		})
	}

	return resp, nil
}
