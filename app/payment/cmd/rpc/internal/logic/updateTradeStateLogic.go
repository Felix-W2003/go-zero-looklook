package logic

import (
	"context"
	"encoding/json"
	"looklook/pkg/kqueue"
	"time"

	"looklook/app/payment/cmd/rpc/internal/svc"
	"looklook/app/payment/cmd/rpc/pb"
	"looklook/app/payment/model"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpdateTradeStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateTradeStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateTradeStateLogic {
	return &UpdateTradeStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *UpdateTradeStateLogic) UpdateTradeState(in *pb.UpdateTradeStateReq) (*pb.UpdateTradeStateResp, error) {

	//1、payment record confirm
	thirdPayment, err := l.svcCtx.ThirdPaymentModel.FindOneBySn(l.ctx, in.Sn)
	if err != nil && err != model.ErrNotFound {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "UpdateTradeState FindOneBySn db err , sn : %s , err : %+v", in.Sn, err)
	}

	if thirdPayment == nil {
		return nil, errors.Wrapf(xerr.NewErrMsg("third payment record no exists"), " sn : %s", in.Sn)
	}

	//2、Judgment Status
	if in.PayStatus == model.ThirdPaymentPayTradeStateSuccess || in.PayStatus == model.ThirdPaymentPayTradeStateFAIL {
		//Want to modify as payment success, failure scenarios
		if thirdPayment.PayStatus != model.ThirdPaymentPayTradeStateWait {
			return &pb.UpdateTradeStateResp{}, nil
		}

	} else if in.PayStatus == model.ThirdPaymentPayTradeStateRefund {
		//Want to change to refund success scenario

		if thirdPayment.PayStatus != model.ThirdPaymentPayTradeStateSuccess {
			return nil, errors.Wrapf(xerr.NewErrMsg("Only orders with successful payment can be refunded"), "Only orders with successful payment can be refunded in : %+v", in)
		}
	} else {
		return nil, errors.Wrapf(xerr.NewErrMsg("This status is not currently supported"), "Modify payment flow status is not supported  in : %+v", in)
	}

	//3、更新支付流水 + 写入发件箱，二者在同一本地事务内完成
	thirdPayment.TradeState = in.TradeState
	thirdPayment.TransactionId = in.TransactionId
	thirdPayment.TradeType = in.TradeType
	thirdPayment.TradeStateDesc = in.TradeStateDesc
	thirdPayment.PayStatus = in.PayStatus
	thirdPayment.PayTime = time.Unix(in.PayTime, 0)

	err = l.svcCtx.OutboxModel.Trans(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		// ① 更新业务数据
		if err := l.svcCtx.ThirdPaymentModel.UpdateWithVersion(ctx, session, thirdPayment); err != nil {
			return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "UpdateTradeState UpdateWithVersion db err:%v ,thirdPayment : %+v , in : %+v", err, thirdPayment, in)
		}

		// ② 写待发消息（与①同事务 → 原子）
		body, err := json.Marshal(kqueue.ThirdPaymentUpdatePayStatusNotifyMessage{
			OrderSn:   in.Sn,
			PayStatus: in.PayStatus,
		})
		if err != nil {
			return errors.Wrapf(xerr.NewErrMsg("outbox payload marshal error"), "marshal err:%v , sn:%s , payStatus:%d", err, in.Sn, in.PayStatus)
		}

		if _, err = l.svcCtx.OutboxModel.Insert(ctx, session, &model.Outbox{
			Topic:   l.svcCtx.Config.KqPaymentUpdatePayStatusConf.Topic,
			MsgKey:  in.Sn, // ★ 用订单号作 Kafka key → 同一订单的消息落在同一分区，保证有序
			Payload: string(body),
			Status:  model.OutboxStatusPending,
		}); err != nil {
			return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "insert outbox err:%v , sn:%s", err, in.Sn)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &pb.UpdateTradeStateResp{}, nil
}
