package logic

import (
	"context"
	"time"

	"looklook/app/coupon/cmd/rpc/internal/svc"
	"looklook/app/coupon/cmd/rpc/pb"
	"looklook/app/coupon/model"
	"looklook/pkg/tool"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ClaimCouponLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewClaimCouponLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ClaimCouponLogic {
	return &ClaimCouponLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ClaimCoupon 领取优惠券
//
// 三个写操作必须**原子**：① 扣库存 ② 发券 ③ 写流水。
// 如果 ① 成功而 ② 失败，库存被扣了但用户没拿到券 —— 凭空少发一张且无人察觉。
// 所以三者放进**同一个本地事务**，任一步失败整体回滚。
func (l *ClaimCouponLogic) ClaimCoupon(in *pb.ClaimCouponReq) (*pb.ClaimCouponResp, error) {

	//1、参数校验
	if in.TemplateId <= 0 || in.UserId <= 0 {
		return nil, xerr.NewErrCode(xerr.REUQEST_PARAM_ERROR)
	}

	//2、查券模板
	tpl, err := l.svcCtx.CouponTemplateModel.FindOne(l.ctx, in.TemplateId)
	if err != nil {
		if err == model.ErrNotFound {
			return nil, xerr.NewErrCode(xerr.COUPON_TEMPLATE_NOT_FOUND)
		}
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "FindOne template err:%v , templateId:%d", err, in.TemplateId)
	}

	//3、校验模板当前是否可领：上架 + 已开始 + 未结束
	//
	// 用 time.Time 的 Before/After 比较，不要用 Unix() 比较 —— 后者会丢掉亚秒精度，
	// 在有效期边界上可能出现"刚过期还能领"的问题。
	now := time.Now()
	if tpl.Status != model.CouponTemplateStatusOn ||
		now.Before(tpl.ValidStart) || now.After(tpl.ValidEnd) {
		return nil, xerr.NewErrCode(xerr.COUPON_TEMPLATE_NOT_FOUND)
	}

	//4、事务：扣库存 + 限领复核 + 发券 + 写流水
	//
	// ⚠️ 闭包内所有数据库操作都必须走 WithSession(session)：
	//    Trans 只在一条连接上开了事务，而 l.svcCtx.XxxModel 内部用的是连接池，
	//    直接用它写入会跑到事务外面 —— 不报错，但回滚时它不会回滚。
	var couponCode string
	err = l.svcCtx.UserCouponModel.Trans(l.ctx, func(ctx context.Context, session sqlx.Session) error {

		//4.1 扣库存（CAS：WHERE issued_count < total_count，影响 0 行即已领完）
		//
		// ⭐ 这一步除了扣库存，还有个副作用：它会锁住券模板的那一行，
		//    从而把「同一模板的并发领券请求」**串行化** —— 这是 4.2 能生效的前提。
		rows, err := l.svcCtx.CouponTemplateModel.WithSession(session).IncrIssuedCount(ctx, tpl.Id)
		if err != nil {
			return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "IncrIssuedCount err:%v , templateId:%d", err, tpl.Id)
		}
		if rows == 0 {
			// ⭐ 用 NewErrCode 而不是 Wrapf：这个错误要**原样返回给前端**，
			//    包一层会丢掉错误码（httpResult 的 IsCodeErr 白名单匹配不到）。
			return xerr.NewErrCode(xerr.COUPON_SOLD_OUT)
		}

		//4.2 ⭐ 限领复核（必须放在事务内 + 扣库存之后 + 用当前读）
		//
		// 为什么不能像最初那样放在事务外面查？
		//   那是 TOCTOU（检查-使用之间存在时间窗口）竞态：
		//   同一用户的两个并发请求都可能读到 count=0，然后各自插一张券 → 超领。
		//
		// 为什么放在「扣库存之后」？
		//   4.1 已经持有了券模板行锁，同一模板的并发请求会排队执行；
		//   后到的请求拿到锁时，前一个事务已经提交，它就能看到前一张券。
		//
		// 为什么必须用 FOR UPDATE（当前读）？
		//   MySQL 默认 REPEATABLE READ 下，事务内的普通 SELECT 是**快照读**，
		//   读的是事务开始时的数据版本，看不到刚刚提交的插入 —— 仍然会超领。
		cnt, err := l.svcCtx.UserCouponModel.WithSession(session).CountByUserAndTemplateForUpdate(ctx, in.UserId, in.TemplateId)
		if err != nil {
			return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "CountByUserAndTemplateForUpdate err:%v , userId:%d , templateId:%d", err, in.UserId, in.TemplateId)
		}
		if cnt >= tpl.PerUserLimit {
			// 返回错误 → 事务回滚 → 4.1 刚扣掉的库存也会自动退回 ✅
			return xerr.NewErrCode(xerr.COUPON_EXCEED_USER_LIMIT)
		}

		//4.3 生成券码
		couponCode, err = tool.GenCouponCode()
		if err != nil {
			return errors.Wrapf(xerr.NewErrCode(xerr.SERVER_COMMON_ERROR), "GenCouponCode err:%v", err)
		}

		//4.4 发券：把模板上的优惠信息【快照】过来
		//    不能只存 template_id 然后每次都去 join 模板表 ——
		//    否则运营改了模板的优惠金额，已发出的券会跟着缩水。
		userCoupon := &model.UserCoupon{
			UserId:         in.UserId,
			TemplateId:     tpl.Id,
			DiscountAmount: tpl.DiscountAmount, // 快照
			MinAmount:      tpl.MinAmount,      // 快照
			CouponCode:     couponCode,
			Status:         model.CouponStatusUnused,
			ExpireTime:     tpl.ValidEnd, // 快照
		}
		if _, err = l.svcCtx.UserCouponModel.WithSession(session).Insert(ctx, userCoupon); err != nil {
			return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "insert user_coupon err:%v , userId:%d , templateId:%d", err, in.UserId, tpl.Id)
		}

		//4.5 写操作流水（审计）
		if _, err = l.svcCtx.CouponUseRecordModel.WithSession(session).Insert(ctx, &model.CouponUseRecord{
			CouponCode: couponCode,
			UserId:     in.UserId,
			Action:     model.CouponActionClaim,
			Remark:     "claim",
		}); err != nil {
			return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "insert coupon_use_record err:%v , couponCode:%s", err, couponCode)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &pb.ClaimCouponResp{CouponCode: couponCode}, nil
}
