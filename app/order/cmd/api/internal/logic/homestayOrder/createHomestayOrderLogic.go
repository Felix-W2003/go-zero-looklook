package homestayOrder

import (
	"context"
	"looklook/app/travel/cmd/rpc/pb"
	"looklook/pkg/ctxdata"

	"looklook/app/order/cmd/api/internal/svc"
	"looklook/app/order/cmd/api/internal/types"
	"looklook/app/order/cmd/rpc/order"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type CreateHomestayOrderLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewCreateHomestayOrderLogic(ctx context.Context, svcCtx *svc.ServiceContext) CreateHomestayOrderLogic {
	return CreateHomestayOrderLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// create order
func (l *CreateHomestayOrderLogic) CreateHomestayOrder(req types.CreateHomestayOrderReq) (*types.CreateHomestayOrderResp, error) {

	homestayResp, err := l.svcCtx.TravelRpc.HomestayDetail(l.ctx, &pb.HomestayDetailReq{
		Id: req.HomestayId,
	})
	if err != nil {
		return nil, err
	}
	if homestayResp.Homestay == nil || homestayResp.Homestay.Id == 0 {
		return nil, errors.Wrapf(xerr.NewErrMsg("homestay no exists"), "CreateHomestayOrder homestay no exists id : %d", req.HomestayId)
	}

	userId := ctxdata.GetUidFromCtx(l.ctx)

	resp, err := l.svcCtx.OrderRpc.CreateHomestayOrder(l.ctx, &order.CreateHomestayOrderReq{
		HomestayId:    req.HomestayId,
		IsFood:        req.IsFood,
		LiveStartTime: req.LiveStartTime,
		LiveEndTime:   req.LiveEndTime,
		UserId:        userId,
		LivePeopleNum: req.LivePeopleNum,
		Remark:        req.Remark,
		CouponCode:    req.CouponCode, //可选：透传优惠券码
	})
	if err != nil {
		// ⚠️ 这里必须用 errors.Wrapf(err, ...) —— 第一个参数是**原始 err**，
		//    这样 err 会作为 cause 保留在错误链里，httpResult 才能通过
		//    errors.Cause(err).(*xerr.CodeError) 取出真正的业务错误码。
		//
		//    反例（本项目原本的写法）：
		//      errors.Wrapf(xerr.NewErrMsg("create homestay order fail"), "... err:%v", err)
		//    第一个参数是**新建的错误**，原始 err 只被拼进了消息文本，
		//    错误码会退化成新错误的 100001 —— 前端永远看不到真正的原因
		//    （比如优惠券的 102003 / 102005）。
		return nil, errors.Wrapf(err, "create homestay order rpc CreateHomestayOrder fail req: %+v", req)
	}

	return &types.CreateHomestayOrderResp{
		OrderSn: resp.Sn,
	}, nil
}
