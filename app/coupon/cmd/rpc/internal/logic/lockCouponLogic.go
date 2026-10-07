package logic

import (
	"context"
	"time"

	"looklook/app/coupon/cmd/rpc/internal/svc"
	"looklook/app/coupon/cmd/rpc/pb"
	"looklook/app/coupon/model"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type LockCouponLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLockCouponLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LockCouponLogic {
	return &LockCouponLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// LockCoupon 下单时锁定券
//
// ⚠️ 这是一个**必须幂等**的接口：
//
//	order-rpc 调用它时若网络超时，调用方无法判断到底成功了没有 ——
//	只有幂等，调用方才能放心重试；否则只能选择"不重试"（可能漏）或
//	"重试"（可能重）。
//
// 返回 discountAmount / finalAmount，让**券服务作为金额的权威计算方** ——
// 调用方不需要自己算抵扣，避免两边算错。
func (l *LockCouponLogic) LockCoupon(in *pb.LockCouponReq) (*pb.LockCouponResp, error) {

	if in.UserId <= 0 || in.CouponCode == "" || in.OrderSn == "" {
		return nil, xerr.NewErrCode(xerr.REUQEST_PARAM_ERROR)
	}

	//1、按券码查券
	coupon, err := l.svcCtx.UserCouponModel.FindOneByCouponCode(l.ctx, in.CouponCode)
	if err != nil {
		if err == model.ErrNotFound {
			return nil, xerr.NewErrCode(xerr.COUPON_NOT_FOUND)
		}
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "FindOneByCouponCode err:%v , couponCode:%s", err, in.CouponCode)
	}

	//2、越权校验：券必须属于调用者。
	//
	// ⚠️ 这里刻意返回 COUPON_NOT_FOUND 而不是"这不是你的券" ——
	//    否则攻击者可以拿别人的券码来试探"这个券码是否存在"。
	if coupon.UserId != in.UserId {
		return nil, xerr.NewErrCode(xerr.COUPON_NOT_FOUND)
	}

	// 计算抵扣结果（无论走哪个分支都返回同一份金额）
	finalAmount := in.Amount - coupon.DiscountAmount
	if finalAmount < 0 {
		finalAmount = 0
	}
	okResp := &pb.LockCouponResp{
		DiscountAmount: coupon.DiscountAmount,
		FinalAmount:    finalAmount,
	}

	//3、⭐ 幂等分支：如果是**同一笔订单**重复锁券（order-rpc 超时重试），
	//   说明上次已经锁成功了，直接返回成功。
	if coupon.Status == model.CouponStatusLocked && coupon.OrderSn == in.OrderSn {
		return okResp, nil
	}

	//4、状态检查（排除第 3 步命中的幂等场景后，其余状态都不允许锁）
	switch coupon.Status {
	case model.CouponStatusLocked:
		// 已锁定，且锁它的不是我这张订单
		return nil, xerr.NewErrCode(xerr.COUPON_OCCUPIED_BY_OTHER_ORDER)
	case model.CouponStatusUsed:
		return nil, xerr.NewErrCode(xerr.COUPON_ALREADY_USED)
	case model.CouponStatusExpired:
		return nil, xerr.NewErrCode(xerr.COUPON_EXPIRED)
	}

	//5、有效期检查（状态还是"未使用"，但可能已经过期而定时任务还没扫到）
	if time.Now().After(coupon.ExpireTime) {
		return nil, xerr.NewErrCode(xerr.COUPON_EXPIRED)
	}

	//6、⭐ 门槛校验 —— 由**券服务自己**保证，不信任调用方。
	//
	//   把门槛校验放在这里（而不是只让 order-rpc 算价时判断）的原因：
	//   PreviewCoupon 是"预览"，客户端可以不调它直接调 LockCoupon；
	//   券的可用性规则必须由券服务自己兜底，否则等于把风控交给了调用方。
	if in.Amount < coupon.MinAmount {
		return nil, xerr.NewErrCode(xerr.COUPON_BELOW_MIN_AMOUNT)
	}

	//7、⭐ CAS 锁定（UPDATE ... WHERE id=? AND user_id=? AND status=0）
	rows, err := l.svcCtx.UserCouponModel.Lock(l.ctx, coupon.Id, in.UserId, in.OrderSn)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "Lock err:%v , couponId:%d , orderSn:%s", err, coupon.Id, in.OrderSn)
	}

	if rows == 0 {
		// 影响 0 行 → 说明在第 4~6 步检查之后、这次更新之前，
		// 有并发请求把券的状态改掉了（TOCTOU 的经典场景）。
		//
		// 不能直接报"被占用" —— 有可能那个并发请求就是我们自己（重试），
		// 所以要回查一次拿到准确原因。
		latest, e := l.svcCtx.UserCouponModel.FindOne(l.ctx, coupon.Id)
		if e == nil && latest.Status == model.CouponStatusLocked && latest.OrderSn == in.OrderSn {
			return okResp, nil // 确实是我们自己锁的 → 幂等成功
		}
		return nil, xerr.NewErrCode(xerr.COUPON_OCCUPIED_BY_OTHER_ORDER)
	}

	//8、写操作流水（审计）
	//
	// 注意：这里**不用事务** —— 锁券已经通过 CAS 生效了，流水是附加记录。
	// 如果为了流水把整个操作放进事务，反而会拉长锁窗口。
	// 流水失败只记日志，不影响锁券结果（下一次状态变更还会再写一条流水）。
	if _, err = l.svcCtx.CouponUseRecordModel.Insert(l.ctx, &model.CouponUseRecord{
		CouponCode: in.CouponCode,
		UserId:     in.UserId,
		OrderSn:    in.OrderSn,
		Action:     model.CouponActionLock,
		Remark:     "lock",
	}); err != nil {
		l.Errorf("insert coupon_use_record failed, couponCode:%s , orderSn:%s , err:%v", in.CouponCode, in.OrderSn, err)
	}

	return okResp, nil
}
