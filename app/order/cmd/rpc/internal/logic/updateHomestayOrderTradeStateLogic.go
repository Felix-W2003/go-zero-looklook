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

	//4、优惠券联动 + 通知用户
	//
	// ⭐ 这里是**所有订单状态变更的唯一入口**：
	//    · 支付成功（order-mq 消费 Kafka 后调用）→ 核销券
	//    · 用户主动取消 / 30 分钟超时关单（asynq 延迟任务）→ 释放券
	//    所以券的核销与释放只需要在这一个地方处理。
	//
	// ⚠️ 已知的权衡（记录在设计文档的「已知风险」里）：
	//    订单状态已经更新完了，此时再调 coupon-rpc 属于**又一次跨服务双写**。
	//    若这里失败，只记日志（不回滚订单状态）—— 否则会把订单状态回滚成"待支付"，
	//    反而引入更严重的不一致。
	//    影响面：券会停留在"已锁定"，30 分钟后被自动释放（用户白用了优惠）。
	//    彻底解决需要把 order 侧也接入事务性发件箱（Outbox），属于二期工作。
	switch in.TradeState {
	case model.HomestayOrderTradeStateWaitUse:
		// 支付成功 → 核销券（UseCoupon 幂等：Kafka 消息重投不会重复核销）
		if _, e := l.svcCtx.CouponRpc.UseCoupon(l.ctx, &coupon.UseCouponReq{OrderSn: homestayOrder.Sn}); e != nil {
			logx.WithContext(l.ctx).Errorf("use coupon fail , sn:%s , err:%v", homestayOrder.Sn, e)
		}

		// 通知用户（原有逻辑）
		payload, err := json.Marshal(jobtype.PaySuccessNotifyUserPayload{Order: homestayOrder})
		if err != nil {
			logx.WithContext(l.ctx).Errorf("pay success notify user task json Marshal fail, err :%+v , sn : %s", err, homestayOrder.Sn)
		} else {
			_, err := l.svcCtx.AsynqClient.Enqueue(asynq.NewTask(jobtype.MsgPaySuccessNotifyUser, payload))
			if err != nil {
				logx.WithContext(l.ctx).Errorf("pay success notify user  insert queue fail err :%+v , sn : %s", err, homestayOrder.Sn)
			}
		}

	case model.HomestayOrderTradeStateCancel:
		// 订单取消 / 超时关单 → 释放券（ReleaseCoupon 幂等）
		//
		// ⚠️ 券服务内部会校验"只有已锁定的券才能释放"，
		//    所以即使订单本来就没用券，这里也是安全的（会走幂等分支返回成功）。
		if _, e := l.svcCtx.CouponRpc.ReleaseCoupon(l.ctx, &coupon.ReleaseCouponReq{OrderSn: homestayOrder.Sn}); e != nil {
			logx.WithContext(l.ctx).Errorf("release coupon fail , sn:%s , err:%v", homestayOrder.Sn, e)
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
