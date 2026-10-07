// Code scaffolded by goctl, then adapted to this project's conventions.
//
// ⚠️ 与 goctl 默认生成的区别（很重要）：
//   goctl 1.10.2 默认生成的是 httpx.ErrorCtx / httpx.OkJsonCtx，
//   而本项目统一使用 pkg/result 的信封：
//     · 成功 → {"code":200,"msg":"OK","data":{...}}
//     · 失败 → HTTP 400 + {"code":102003,"msg":"您已达到领取上限"}
//   如果沿用 httpx 的默认写法，业务错误会变成 HTTP 500 且响应体没有统一信封，
//   前端既拿不到 data，也拿不到业务错误码。
package coupon

import (
	"net/http"

	"looklook/app/coupon/cmd/api/internal/logic/coupon"
	"looklook/app/coupon/cmd/api/internal/svc"
	"looklook/app/coupon/cmd/api/internal/types"
	"looklook/pkg/result"

	"github.com/zeromicro/go-zero/rest/httpx"
)

// 领取优惠券
func ClaimHandler(svcCtx *svc.ServiceContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req types.ClaimReq
		if err := httpx.Parse(r, &req); err != nil {
			result.ParamErrorResult(r, w, err)
			return
		}

		l := coupon.NewClaimLogic(r.Context(), svcCtx)
		resp, err := l.Claim(&req)
		result.HttpResult(r, w, resp, err)
	}
}
