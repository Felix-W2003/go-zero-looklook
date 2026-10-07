package logic

import (
	"context"
	"database/sql"

	"looklook/app/coupon/cmd/rpc/internal/svc"
	"looklook/app/coupon/cmd/rpc/pb"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type ListUserCouponsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListUserCouponsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListUserCouponsLogic {
	return &ListUserCouponsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListUserCoupons 我的券列表
//
// status 传 -1（model.CouponStatusAll）表示全部，其余按状态筛选。
// 分页用游标（lastId + pageSize），lastId 传 0 表示第一页。
func (l *ListUserCouponsLogic) ListUserCoupons(in *pb.ListUserCouponsReq) (*pb.ListUserCouponsResp, error) {

	if in.UserId <= 0 {
		return nil, xerr.NewErrCode(xerr.REUQEST_PARAM_ERROR)
	}

	pageSize := in.PageSize
	if pageSize <= 0 || pageSize > 50 {
		pageSize = 10
	}

	list, err := l.svcCtx.UserCouponModel.FindListByUser(l.ctx, in.UserId, in.Status, in.LastId, pageSize)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "FindListByUser err:%v , in:%+v", err, in)
	}

	// 券名称是**展示信息**，不做快照 —— 实时从模板查即可（模板改名是可接受的）。
	// 同一模板只查一次，避免 N+1：券列表里的 templateId 通常只有 1~2 个。
	tplNames := make(map[int64]string)

	resp := &pb.ListUserCouponsResp{
		List: make([]*pb.UserCoupon, 0, len(list)),
	}
	for _, c := range list {
		name, ok := tplNames[c.TemplateId]
		if !ok {
			if tpl, e := l.svcCtx.CouponTemplateModel.FindOne(l.ctx, c.TemplateId); e == nil {
				name = tpl.Name
			}
			tplNames[c.TemplateId] = name
		}

		resp.List = append(resp.List, &pb.UserCoupon{
			Id:             c.Id,
			UserId:         c.UserId,
			TemplateId:     c.TemplateId,
			CouponCode:     c.CouponCode,
			Status:         c.Status,
			OrderSn:        c.OrderSn,
			LockTime:       nullTimeToUnix(c.LockTime),
			UseTime:        nullTimeToUnix(c.UseTime),
			ExpireTime:     c.ExpireTime.Unix(),
			DiscountAmount: c.DiscountAmount,
			MinAmount:      c.MinAmount,
			Name:           name,
		})
	}

	return resp, nil
}

// nullTimeToUnix 把可空的 datetime 字段转成时间戳。
//
// lock_time / use_time 在库里是可空列（未锁定 / 未核销时是 NULL），
// goctl 生成的是 sql.NullTime，必须先判 Valid 再取 Time，否则会拿到零值时间
// （0001-01-01，Unix() 会得到一个很大的负数）。
func nullTimeToUnix(t sql.NullTime) int64 {
	if t.Valid {
		return t.Time.Unix()
	}
	return 0
}
