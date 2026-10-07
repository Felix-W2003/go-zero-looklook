package logic

import (
	"context"

	"looklook/app/coupon/cmd/rpc/internal/svc"
	"looklook/app/coupon/cmd/rpc/pb"
	"looklook/app/coupon/model"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type UseCouponLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUseCouponLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UseCouponLogic {
	return &UseCouponLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UseCoupon 支付成功后核销券（order-mq 调用）
//
// ⚠️ 必须幂等：调用方消费的是 Kafka 消息，而 Kafka 是 at-least-once 语义，
// 同一条"支付成功"消息可能被投递多次。
//
// 两种"没有可核销的券"都是正常情况，都应返回成功：
//  1. 这笔订单本来就没用券（用户没选券）—— 不是错误
//  2. 券已经核销过了（重复消息）—— 幂等
func (l *UseCouponLogic) UseCoupon(in *pb.UseCouponReq) (*pb.UseCouponResp, error) {

	if in.OrderSn == "" {
		return nil, xerr.NewErrCode(xerr.REUQEST_PARAM_ERROR)
	}

	//1、查出这笔订单关联的券（用于幂等判断 + 写流水时拿券码）
	coupon, err := l.svcCtx.UserCouponModel.FindOneByOrderSn(l.ctx, in.OrderSn)
	if err != nil {
		if err == model.ErrNotFound {
			// 没找到 → 这笔订单没用券，正常情况，幂等返回成功
			l.Infof("order %s has no coupon, skip use", in.OrderSn)
			return &pb.UseCouponResp{}, nil
		}
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "FindOneByOrderSn err:%v , orderSn:%s", err, in.OrderSn)
	}

	//2、幂等分支：已经核销过了（消息重投）
	if coupon.Status == model.CouponStatusUsed {
		l.Infof("coupon %s already used, idempotent skip", coupon.CouponCode)
		return &pb.UseCouponResp{}, nil
	}

	//3、CAS 核销（UPDATE ... WHERE order_sn=? AND status=1）
	rows, err := l.svcCtx.UserCouponModel.UseByOrderSn(l.ctx, in.OrderSn)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "UseByOrderSn err:%v , orderSn:%s", err, in.OrderSn)
	}

	if rows == 0 {
		// 影响 0 行 → 在步骤 2 之后、这次 UPDATE 之前有并发改动。
		// 回查一次区分「别人刚核销了（幂等成功）」和「状态异常（真错误）」。
		latest, e := l.svcCtx.UserCouponModel.FindOne(l.ctx, coupon.Id)
		if e == nil && latest.Status == model.CouponStatusUsed {
			l.Infof("coupon %s used by concurrent request, idempotent skip", coupon.CouponCode)
			return &pb.UseCouponResp{}, nil
		}
		l.Errorf("use coupon but status invalid, couponCode:%s , orderSn:%s , status:%d", coupon.CouponCode, in.OrderSn, coupon.Status)
		return nil, xerr.NewErrCode(xerr.COUPON_STATUS_INVALID)
	}

	//4、写操作流水（审计）
	if _, err = l.svcCtx.CouponUseRecordModel.Insert(l.ctx, &model.CouponUseRecord{
		CouponCode: coupon.CouponCode,
		UserId:     coupon.UserId,
		OrderSn:    in.OrderSn,
		Action:     model.CouponActionUse,
		Remark:     "use",
	}); err != nil {
		l.Errorf("insert coupon_use_record failed, couponCode:%s , orderSn:%s , err:%v", coupon.CouponCode, in.OrderSn, err)
	}

	return &pb.UseCouponResp{}, nil
}
