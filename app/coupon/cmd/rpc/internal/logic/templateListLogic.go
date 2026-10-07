package logic

import (
	"context"

	"looklook/app/coupon/cmd/rpc/internal/svc"
	"looklook/app/coupon/cmd/rpc/pb"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type TemplateListLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTemplateListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TemplateListLogic {
	return &TemplateListLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// TemplateList 可领券列表
//
// 这是一个「标准只读 logic」的模板，后面几个查询类 logic 都照这个结构写：
//  1. 参数兜底（不要让客户端传的非法值直接进 SQL）
//  2. 调 model 拿数据（只读，不需要事务、不需要加锁）
//  3. 数据转换（model 结构体 → pb 结构体），注意时间字段要转成时间戳
//  4. 错误包装（errors.Wrapf + xerr.NewErrCode，保留原始错误便于排查）
func (l *TemplateListLogic) TemplateList(in *pb.TemplateListReq) (*pb.TemplateListResp, error) {

	// 1、分页兜底：PageSize 非法时给默认值，同时限制上限防止一次拉太多
	pageSize := in.PageSize
	if pageSize <= 0 || pageSize > 50 {
		pageSize = 10
	}

	// 2、查询（只读）
	list, err := l.svcCtx.CouponTemplateModel.FindAvailableList(l.ctx, in.LastId, pageSize)
	if err != nil {
		return nil, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "FindAvailableList err:%v , in:%+v", err, in)
	}

	// 3、转换
	resp := &pb.TemplateListResp{
		List: make([]*pb.CouponTemplate, 0, len(list)),
	}
	for _, t := range list {
		resp.List = append(resp.List, &pb.CouponTemplate{
			Id:             t.Id,
			Name:           t.Name,
			Type:           t.Type,
			DiscountAmount: t.DiscountAmount,
			MinAmount:      t.MinAmount,
			TotalCount:     t.TotalCount,
			IssuedCount:    t.IssuedCount,
			PerUserLimit:   t.PerUserLimit,
			ValidStart:     t.ValidStart.Unix(),
			ValidEnd:       t.ValidEnd.Unix(),
			Status:         t.Status,
		})
	}

	return resp, nil
}
