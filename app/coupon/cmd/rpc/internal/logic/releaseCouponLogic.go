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

type ReleaseCouponLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReleaseCouponLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReleaseCouponLogic {
	return &ReleaseCouponLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ReleaseCoupon 订单取消 / 超时未支付时释放券
//
// 调用方有两类，都要求幂等：
//  1. order-rpc：用户主动取消订单时立即释放
//  2. asynq 延迟任务：下单 30 分钟后检查订单仍未支付 → 释放
//
// ⚠️ 最关键的约束：**已核销的券绝不能被释放**。
// 那意味着用户已经用掉这张券了；若被释放回"未使用"，等于白送一张。
// 所以 CAS 的 WHERE 条件必须是 status = 1（已锁定）。
func (l *ReleaseCouponLogic) ReleaseCoupon(in *pb.ReleaseCouponReq) (*pb.ReleaseCouponResp, error) {

	if in.OrderSn == "" {
		return nil, xerr.NewErrCode(xerr.REUQEST_PARAM_ERROR)
	}

	//1、先查出这笔订单关联的券
	//
	// 为什么要先查而不是直接 CAS？
	//   ① 写流水需要券码、用户 id
	//   ② 需要区分"没券"和"状态异常"，给出准确的处理
	coupon, err := l.svcCtx.UserCouponModel.FindOneByOrderSn(l.ctx, in.OrderSn)
	if err != nil {
		if err == model.ErrNotFound {
			// 这笔订单没有关联的券，或者券已经被释放过了
			// （释放时会把 order_sn 清空）—— 两种情况都返回成功
			l.Infof("order %s has no locked coupon, skip release", in.OrderSn)
			return &pb.ReleaseCouponResp{}, nil
		}
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "FindOneByOrderSn err:%v , orderSn:%s", err, in.OrderSn)
	}

	//2、状态检查
	switch coupon.Status {
	case model.CouponStatusUnused:
		// 已经是"未使用"—— 幂等（重复调用 / 并发释放）
		l.Infof("coupon %s already released, idempotent skip", coupon.CouponCode)
		return &pb.ReleaseCouponResp{}, nil

	case model.CouponStatusUsed:
		// ⚠️ 券已被核销（订单已支付）却来释放 —— 不能允许
		l.Errorf("try to release a USED coupon! couponCode:%s , orderSn:%s", coupon.CouponCode, in.OrderSn)
		return nil, xerr.NewErrCode(xerr.COUPON_ALREADY_USED)

	case model.CouponStatusExpired:
		l.Errorf("release coupon but expired, couponCode:%s , orderSn:%s", coupon.CouponCode, in.OrderSn)
		return nil, xerr.NewErrCode(xerr.COUPON_STATUS_INVALID)
	}

	//3、CAS 释放（UPDATE ... SET status=0, order_sn='' WHERE order_sn=? AND status=1）
	rows, err := l.svcCtx.UserCouponModel.ReleaseByOrderSn(l.ctx, in.OrderSn)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "ReleaseByOrderSn err:%v , orderSn:%s", err, in.OrderSn)
	}

	if rows == 0 {
		// 影响 0 行 → 在第 2 步之后、这次 UPDATE 之前状态被并发改掉了。
		// 回查区分「别人刚释放了（幂等成功）」和「状态异常」。
		latest, e := l.svcCtx.UserCouponModel.FindOne(l.ctx, coupon.Id)
		if e == nil && latest.Status == model.CouponStatusUnused {
			l.Infof("coupon %s released by concurrent request, idempotent skip", coupon.CouponCode)
			return &pb.ReleaseCouponResp{}, nil
		}
		l.Errorf("release coupon failed, couponCode:%s , orderSn:%s , status:%d", coupon.CouponCode, in.OrderSn, coupon.Status)
		return nil, xerr.NewErrCode(xerr.COUPON_STATUS_INVALID)
	}

	//4、写操作流水（审计）—— 用第 1 步查到的券码
	if _, err = l.svcCtx.CouponUseRecordModel.Insert(l.ctx, &model.CouponUseRecord{
		CouponCode: coupon.CouponCode,
		UserId:     coupon.UserId,
		OrderSn:    in.OrderSn,
		Action:     model.CouponActionRelease,
		Remark:     "release",
	}); err != nil {
		l.Errorf("insert coupon_use_record failed, couponCode:%s , orderSn:%s , err:%v", coupon.CouponCode, in.OrderSn, err)
	}

	return &pb.ReleaseCouponResp{}, nil
}
