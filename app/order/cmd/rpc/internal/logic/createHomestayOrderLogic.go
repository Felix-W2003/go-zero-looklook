package logic

import (
	"context"
	"encoding/json"
	"github.com/hibiken/asynq"
	"looklook/app/mqueue/cmd/job/jobtype"
	"strings"
	"time"

	"looklook/app/coupon/cmd/rpc/coupon"
	"looklook/app/order/cmd/rpc/internal/svc"
	"looklook/app/order/cmd/rpc/pb"
	"looklook/app/order/model"
	"looklook/app/travel/cmd/rpc/travel"
	"looklook/pkg/tool"
	"looklook/pkg/uniqueid"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

const CloseOrderTimeMinutes = 30 //defer close order time

type CreateHomestayOrderLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateHomestayOrderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateHomestayOrderLogic {
	return &CreateHomestayOrderLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateHomestayOrder.
//
// ⭐ 优惠券的集成顺序（很重要，决定了不一致窗口的大小）：
//
//	① 参数校验 / 查民宿 / 算价       —— 只读，失败无副作用
//	② 调 coupon-rpc.LockCoupon      —— 第一个**有副作用**的操作
//	③ Insert 订单                   —— 第二个写操作
//	④ 排延迟关单任务                —— 后续动作
//
// 把「锁券」放在「写订单」之前的原因：
//
//	锁券失败时订单还没建 → 直接返回错误即可，**没有任何脏状态需要清理**。
//	反过来（先写订单再锁券）失败时，要么删订单、要么留一张没券的订单，处理更麻烦。
//
// 而 ② 成功、③ 失败的情况用**补偿**处理（见下方 defer 补偿逻辑）。
func (l *CreateHomestayOrderLogic) CreateHomestayOrder(in *pb.CreateHomestayOrderReq) (*pb.CreateHomestayOrderResp, error) {

	//1、Create Order
	if in.LiveEndTime <= in.LiveStartTime {
		return nil, errors.Wrapf(xerr.NewErrMsg("Stay at least one night"), "Place an order at a B&B. The end time of your stay must be greater than the start time. in : %+v", in)
	}

	resp, err := l.svcCtx.TravelRpc.HomestayDetail(l.ctx, &travel.HomestayDetailReq{
		Id: in.HomestayId,
	})
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrMsg("Failed to query the record"), "Failed to query the record  rpc HomestayDetail fail , homestayId : %d , err : %v", in.HomestayId, err)
	}
	if resp.Homestay == nil {
		return nil, errors.Wrapf(xerr.NewErrMsg("This record does not exist"), "This record does not exist , homestayId : %d ", in.HomestayId)
	}

	var cover string //Get the cover...
	if len(resp.Homestay.Banner) > 0 {
		cover = strings.Split(resp.Homestay.Banner, ",")[0]
	}

	order := new(model.HomestayOrder)
	order.Sn = uniqueid.GenSn(uniqueid.SN_PREFIX_HOMESTAY_ORDER)
	order.UserId = in.UserId
	order.HomestayId = in.HomestayId
	order.Title = resp.Homestay.Title
	order.SubTitle = resp.Homestay.SubTitle
	order.Cover = cover
	order.Info = resp.Homestay.Info
	order.PeopleNum = resp.Homestay.PeopleNum
	order.RowType = resp.Homestay.RowType
	order.HomestayPrice = resp.Homestay.HomestayPrice
	order.MarketHomestayPrice = resp.Homestay.MarketHomestayPrice
	order.HomestayBusinessId = resp.Homestay.HomestayBusinessId
	order.HomestayUserId = resp.Homestay.UserId
	order.LivePeopleNum = in.LivePeopleNum
	order.TradeState = model.HomestayOrderTradeStateWaitPay
	order.TradeCode = tool.Krand(8, tool.KC_RAND_KIND_ALL)
	order.Remark = in.Remark
	order.FoodInfo = resp.Homestay.FoodInfo
	order.FoodPrice = resp.Homestay.FoodPrice
	order.LiveStartDate = time.Unix(in.LiveStartTime, 0)
	order.LiveEndDate = time.Unix(in.LiveEndTime, 0)

	liveDays := int64(order.LiveEndDate.Sub(order.LiveStartDate).Seconds() / 86400) //Stayed a few days in total

	order.HomestayTotalPrice = int64(resp.Homestay.HomestayPrice * liveDays) //Calculate the total price of the B&B
	if in.IsFood {
		order.NeedFood = model.HomestayOrderNeedFoodYes
		//Calculate the total price of the meal.
		order.FoodTotalPrice = int64(resp.Homestay.FoodPrice * in.LivePeopleNum * liveDays)
	}

	order.OrderTotalPrice = order.HomestayTotalPrice + order.FoodTotalPrice //Calculate total order price.

	//2、优惠券：先锁券，再写订单
	//
	// ⚠️ 注意 order.Sn 在上面已经生成好了，可以直接用来关联券。
	var couponLocked bool
	if in.CouponCode != "" {

		lockResp, err := l.svcCtx.CouponRpc.LockCoupon(l.ctx, &coupon.LockCouponReq{
			UserId:     in.UserId,
			CouponCode: in.CouponCode,
			OrderSn:    order.Sn,              // 用它把券和订单关联起来
			Amount:     order.OrderTotalPrice, // 券服务据此校验门槛并计算抵扣
		})
		if err != nil {
			// 锁券失败 → 订单还没建，直接返回，无任何脏状态
			// 这里用 errors.Wrapf(err, ...) 保留原始 err 作为 cause，
			// 保证 102003/102005/102006 等业务错误码能一路传到前端。
			return nil, errors.Wrapf(err, "lock coupon fail , couponCode:%s , sn:%s", in.CouponCode, order.Sn)
		}

		couponLocked = true

		// 用券服务返回的**权威金额**，而不是自己算 —— 避免两边算错
		order.OrderTotalPrice = lockResp.FinalAmount

		l.Infof("order %s locked coupon %s , discount:%d , final:%d",
			order.Sn, in.CouponCode, lockResp.DiscountAmount, lockResp.FinalAmount)
	}

	//3、补偿：如果锁了券但后续步骤失败，必须把券释放掉
	//
	// ⭐ 为什么用 defer + 命名返回？这样才能在**任何**return 路径上触发补偿。
	//    补偿本身是幂等的（ReleaseCoupon 内部用 CAS），重复调用无副作用。
	//
	//    即使补偿失败（比如 coupon-rpc 也挂了），券也会在 30 分钟后
	//    被延迟任务自动释放 —— 这是"用自愈机制兜底"的设计。
	success := false
	if couponLocked {
		defer func() {
			if success {
				return
			}
			releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, e := l.svcCtx.CouponRpc.ReleaseCoupon(releaseCtx, &coupon.ReleaseCouponReq{OrderSn: order.Sn}); e != nil {
				// 补偿失败不改变返回给调用方的错误，只记日志（靠延迟任务自愈）
				logx.Errorf("compensate release coupon failed! sn:%s , couponCode:%s , err:%v", order.Sn, in.CouponCode, e)
			} else {
				logx.Infof("compensate release coupon ok , sn:%s , couponCode:%s", order.Sn, in.CouponCode)
			}
		}()
	}

	_, err = l.svcCtx.HomestayOrderModel.Insert(l.ctx, nil, order)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "Order Database Exception order : %+v , err: %v", order, err)
	}

	//4、Delayed closing of order tasks.
	payload, err := json.Marshal(jobtype.DeferCloseHomestayOrderPayload{Sn: order.Sn})
	if err != nil {
		logx.WithContext(l.ctx).Errorf("create defer close order task json Marshal fail err :%+v , sn : %s", err, order.Sn)
	} else {
		_, err = l.svcCtx.AsynqClient.Enqueue(asynq.NewTask(jobtype.DeferCloseHomestayOrder, payload), asynq.ProcessIn(CloseOrderTimeMinutes*time.Minute))
		if err != nil {
			logx.WithContext(l.ctx).Errorf("create defer close order task insert queue fail err :%+v , sn : %s", err, order.Sn)
		}
	}

	// 走到这里说明订单已经落库，不需要补偿了
	success = true

	return &pb.CreateHomestayOrderResp{
		Sn: order.Sn,
	}, nil
}
