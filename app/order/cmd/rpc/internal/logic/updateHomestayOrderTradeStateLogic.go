package logic

import (
	"context"
	"encoding/json"
	"github.com/hibiken/asynq"
	"looklook/app/mqueue/cmd/job/jobtype"

	"looklook/app/coupon/cmd/rpc/coupon"
	"looklook/app/order/cmd/rpc/internal/svc"
	"looklook/app/order/cmd/rpc/pb"
	"looklook/app/order/model"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateHomestayOrderTradeStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateHomestayOrderTradeStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateHomestayOrderTradeStateLogic {
	return &UpdateHomestayOrderTradeStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Update homestay order status
//
// ⭐ 这里是**所有订单状态变更的唯一入口**：
//   - 支付成功（order-mq 消费 Kafka 后调用）→ 核销券
//   - 用户主动取消 / 30 分钟超时关单（asynq 延迟任务）→ 释放券
//
// 所以券的核销与释放只需要在这一个地方处理。
func (l *UpdateHomestayOrderTradeStateLogic) UpdateHomestayOrderTradeState(in *pb.UpdateHomestayOrderTradeStateReq) (*pb.UpdateHomestayOrderTradeStateResp, error) {

	// 1、Check current order
	homestayOrder, err := l.svcCtx.HomestayOrderModel.FindOneBySn(l.ctx, in.Sn)
	if err != nil && err != model.ErrNotFound {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "UpdateHomestayOrderTradeState FindOneBySn db err : %v , in:%+v", err, in)
	}
	if homestayOrder == nil {
		return nil, errors.Wrapf(xerr.NewErrMsg("order no exists"), "order no exists  in : %+v", in)
	}

	if homestayOrder.TradeState == in.TradeState {
		// ⭐⭐ 已经在目标状态了 —— 订单本身不用再改，但**券的联动不能跳过**！
		//
		// 为什么？（这是一个真实存在的 bug，务必保留这段逻辑）
		//
		//	「更新订单状态」和「同步券状态」是两次**独立的跨服务调用**，
		//	完全可能「订单更新成功、券同步失败」。此时调用方重试（Kafka 重投 /
		//	延迟任务重跑）就会走到这个分支 —— 如果直接 return，券将**永远得不到补偿**：
		//
		//	  · 取消路径失败 → 券永久卡在「已锁定」（延迟任务也是走这里，同样被跳过）
		//	  · 核销路径失败 → 券停在「已锁定」，30 分钟后被自动释放 → 用户白用优惠
		//
		// 而 UseCoupon / ReleaseCoupon **本身就是幂等的**（内部用 CAS + 状态回查），
		//	在这里重放完全安全 —— 这正是幂等设计的价值：让调用方可以放心重试。
		l.syncCoupon(homestayOrder.Sn, in.TradeState)
		return &pb.UpdateHomestayOrderTradeStateResp{}, nil
	}

	// 2、Verify order status
	if err := l.verifyOrderTradeState(in.TradeState, homestayOrder.TradeState); err != nil {
		return nil, errors.WithMessagef(err, " , in : %+v", in)
	}

	// 3、Pre-update status judgment.
	homestayOrder.TradeState = in.TradeState
	if err := l.svcCtx.HomestayOrderModel.UpdateWithVersion(l.ctx, nil, homestayOrder); err != nil {
		return nil, errors.Wrapf(xerr.NewErrMsg("Failed to update homestay order status"), "Failed to update homestay order status db UpdateWithVersion err:%v , in : %v", err, in)
	}

	// 4、优惠券联动
	l.syncCoupon(homestayOrder.Sn, in.TradeState)

	// 5、notify user
	if in.TradeState == model.HomestayOrderTradeStateWaitUse {
		payload, err := json.Marshal(jobtype.PaySuccessNotifyUserPayload{Order: homestayOrder})
		if err != nil {
			logx.WithContext(l.ctx).Errorf("pay success notify user task json Marshal fail, err :%+v , sn : %s", err, homestayOrder.Sn)
		} else {
			_, err := l.svcCtx.AsynqClient.Enqueue(asynq.NewTask(jobtype.MsgPaySuccessNotifyUser, payload))
			if err != nil {
				logx.WithContext(l.ctx).Errorf("pay success notify user  insert queue fail err :%+v , sn : %s", err, homestayOrder.Sn)
			}
		}
	}

	return &pb.UpdateHomestayOrderTradeStateResp{
		Id:              homestayOrder.Id,
		UserId:          homestayOrder.UserId,
		Sn:              homestayOrder.Sn,
		TradeCode:       homestayOrder.TradeCode,
		Title:           homestayOrder.Title,
		LiveStartDate:   homestayOrder.LiveStartDate.Unix(),
		LiveEndDate:     homestayOrder.LiveEndDate.Unix(),
		OrderTotalPrice: homestayOrder.OrderTotalPrice,
	}, nil
}

// syncCoupon 把订单状态变化同步到优惠券服务。
//
// ⚠️ 这里**不回滚订单状态**（订单状态已经改完了）—— 否则会把订单退回"待支付"，
// 引入更严重的不一致。失败只记日志。
//
// 兜底机制（重要）：
//   - 这个方法是**幂等**的，调用方重试时会再次执行，最终能收敛
//   - 即使始终失败，锁定的券也会被「订单 30 分钟超时关单」流程带走
//     （那条路径同样会走到本方法）
//
// 彻底解决需要把 order 侧也接入事务性发件箱（Outbox），属于二期工作。
func (l *UpdateHomestayOrderTradeStateLogic) syncCoupon(sn string, tradeState int64) {
	switch tradeState {
	case model.HomestayOrderTradeStateWaitUse:
		// 支付成功 → 核销券
		if _, err := l.svcCtx.CouponRpc.UseCoupon(l.ctx, &coupon.UseCouponReq{OrderSn: sn}); err != nil {
			l.Errorf("use coupon fail , sn:%s , err:%v", sn, err)
		}

	case model.HomestayOrderTradeStateCancel:
		// 订单取消 / 超时关单 → 释放券
		//
		// ⚠️ 券服务内部会校验"只有已锁定的券才能释放"，
		//    所以即使这笔订单本来就没用券也是安全的（会走幂等分支返回成功）。
		if _, err := l.svcCtx.CouponRpc.ReleaseCoupon(l.ctx, &coupon.ReleaseCouponReq{OrderSn: sn}); err != nil {
			l.Errorf("release coupon fail , sn:%s , err:%v", sn, err)
		}

	default:
		// 其他状态（已使用 / 已退款 / 已过期）与券无关：
		// 一期不做"退款退券"，券的最终状态在核销时就已确定。
	}
}

// Update homestay order status
func (l *UpdateHomestayOrderTradeStateLogic) verifyOrderTradeState(newTradeState, oldTradeState int64) error {
	if newTradeState == model.HomestayOrderTradeStateWaitPay {
		return errors.Wrapf(xerr.NewErrMsg("Changing this status is not supported"),
			"Changing this status is not supported newTradeState: %d, oldTradeState: %d",
			newTradeState,
			oldTradeState)
	}

	if newTradeState == model.HomestayOrderTradeStateCancel {

		if oldTradeState != model.HomestayOrderTradeStateWaitPay {
			return errors.Wrapf(xerr.NewErrMsg("只有待支付的订单才能被取消"),
				"Only orders pending payment can be cancelled newTradeState: %d, oldTradeState: %d",
				newTradeState,
				oldTradeState)
		}

	} else if newTradeState == model.HomestayOrderTradeStateWaitUse {
		if oldTradeState != model.HomestayOrderTradeStateWaitPay {
			return errors.Wrapf(xerr.NewErrMsg("Only orders pending payment can change this status"),
				"Only orders pending payment can change this status newTradeState: %d, oldTradeState: %d",
				newTradeState,
				oldTradeState)
		}
	} else if newTradeState == model.HomestayOrderTradeStateUsed {
		if oldTradeState != model.HomestayOrderTradeStateWaitUse {
			return errors.Wrapf(xerr.NewErrMsg("Only unused orders can be changed to this status"),
				"Only unused orders can be changed to this status newTradeState: %d, oldTradeState: %d",
				newTradeState,
				oldTradeState)
		}
	} else if newTradeState == model.HomestayOrderTradeStateRefund {
		if oldTradeState != model.HomestayOrderTradeStateWaitUse {
			return errors.Wrapf(xerr.NewErrMsg("Only unused orders can be changed to this status"),
				"Only unused orders can be changed to this status newTradeState: %d, oldTradeState: %d",
				newTradeState,
				oldTradeState)
		}
	} else if newTradeState == model.HomestayOrderTradeStateExpire {
		if oldTradeState != model.HomestayOrderTradeStateWaitUse {
			return errors.Wrapf(xerr.NewErrMsg("Only unused orders can be changed to this status"),
				"Only unused orders can be changed to this status newTradeState: %d, oldTradeState: %d",
				newTradeState,
				oldTradeState)
		}
	}

	return nil
}
