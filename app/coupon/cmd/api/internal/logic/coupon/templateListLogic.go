package coupon

import (
	"context"

	"looklook/app/coupon/cmd/api/internal/svc"
	"looklook/app/coupon/cmd/api/internal/types"
	couponRpc "looklook/app/coupon/cmd/rpc/coupon"

	"github.com/zeromicro/go-zero/core/logx"
)

type TemplateListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

func NewTemplateListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TemplateListLogic {
	return &TemplateListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// TemplateList 可领取的优惠券列表（不需要登录）
//
// api 层是薄封装：调 rpc → 转 types。
// 唯一需要做的是「字段映射」和「api 层特有的计算」——
// 这里 RemainCount（剩余可领数量）在 rpc 里没有，属于展示层的加工，
// 放在 api 层算最合适（rpc 只负责返回原始业务数据）。
func (l *TemplateListLogic) TemplateList(req *types.TemplateListReq) (resp *types.TemplateListResp, err error) {

	rpcResp, err := l.svcCtx.CouponRpc.TemplateList(l.ctx, &couponRpc.TemplateListReq{
		LastId:   req.LastId,
		PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}

	resp = &types.TemplateListResp{
		List: make([]types.TemplateInfo, 0, len(rpcResp.List)),
	}
	for _, t := range rpcResp.List {
		remain := t.TotalCount - t.IssuedCount
		if remain < 0 {
			remain = 0
		}
		resp.List = append(resp.List, types.TemplateInfo{
			TemplateId:     t.Id,
			Name:           t.Name,
			DiscountAmount: t.DiscountAmount,
			MinAmount:      t.MinAmount,
			ValidStart:     t.ValidStart,
			ValidEnd:       t.ValidEnd,
			RemainCount:    remain,
		})
	}

	return resp, nil
}
