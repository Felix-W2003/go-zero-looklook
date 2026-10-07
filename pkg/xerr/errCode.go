package xerr

//成功返回
const OK uint32 = 200

/**(前3位代表业务,后三位代表具体功能)**/

//全局错误码
const SERVER_COMMON_ERROR uint32 = 100001
const REUQEST_PARAM_ERROR uint32 = 100002
const TOKEN_EXPIRE_ERROR uint32 = 100003
const TOKEN_GENERATE_ERROR uint32 = 100004
const DB_ERROR uint32 = 100005
const DB_UPDATE_AFFECTED_ZERO_ERROR uint32 = 100006

//用户模块

//优惠券模块
//
// ⚠️ 新增错误码时，必须同时在 errMsg.go 的 message 白名单里登记，
// 否则跨服务（rpc → api）返回的错误会被 httpResult 替换成 SERVER_COMMON_ERROR。
// 参见 pkg/result/httpResult.go 中 IsCodeErr 的分支。
const COUPON_TEMPLATE_NOT_FOUND uint32 = 102001
const COUPON_SOLD_OUT uint32 = 102002
const COUPON_EXCEED_USER_LIMIT uint32 = 102003
const COUPON_NOT_FOUND uint32 = 102004
const COUPON_ALREADY_USED uint32 = 102005
const COUPON_EXPIRED uint32 = 102006
const COUPON_BELOW_MIN_AMOUNT uint32 = 102007
const COUPON_OCCUPIED_BY_OTHER_ORDER uint32 = 102008
const COUPON_STATUS_INVALID uint32 = 102009
