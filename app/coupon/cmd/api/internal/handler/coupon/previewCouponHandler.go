// Code scaffolded by goctl, then adapted to this project's conventions.
// 详见 claimHandler.go 顶部说明（统一使用 pkg/result 的信封）。
package coupon

import (
	"net/http"

	"looklook/app/coupon/cmd/api/internal/logic/coupon"
	"looklook/app/coupon/cmd/api/internal/svc"
	"looklook/app/coupon/cmd/api/internal/types"
	"looklook/pkg/result"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 下单前预览可用券
func PreviewCouponHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.PreviewCouponReq
		if err := httpx.Parse(r, &req); err != nil {
			result.ParamErrorResult(r, w, err)
			return
		}

		l := coupon.NewPreviewCouponLogic(r.Context(), svcCtx)
		resp, err := l.PreviewCoupon(&req)
		result.HttpResult(r, w, resp, err)
	}
}
