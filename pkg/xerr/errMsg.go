package xerr

var message map[uint32]string

func init() {
	message = make(map[uint32]string)
	message[OK] = "SUCCESS"
	message[SERVER_COMMON_ERROR] = "服务器开小差啦,稍后再来试一试"
	message[REUQEST_PARAM_ERROR] = "参数错误"
	message[TOKEN_EXPIRE_ERROR] = "token失效，请重新登陆"
	message[TOKEN_GENERATE_ERROR] = "生成token失败"
	message[DB_ERROR] = "数据库繁忙,请稍后再试"
	message[DB_UPDATE_AFFECTED_ZERO_ERROR] = "更新数据影响行数为0"

	//优惠券模块
	message[COUPON_TEMPLATE_NOT_FOUND] = "优惠券不存在"
	message[COUPON_SOLD_OUT] = "优惠券已领完"
	message[COUPON_EXCEED_USER_LIMIT] = "您已达到领取上限"
	message[COUPON_NOT_FOUND] = "优惠券不存在"
	message[COUPON_ALREADY_USED] = "优惠券已被使用"
	message[COUPON_EXPIRED] = "优惠券已过期"
	message[COUPON_BELOW_MIN_AMOUNT] = "订单金额未满使用门槛"
	message[COUPON_OCCUPIED_BY_OTHER_ORDER] = "优惠券已被其他订单使用"
	message[COUPON_STATUS_INVALID] = "优惠券状态异常"
}

func MapErrMsg(errcode uint32) string {
	if msg, ok := message[errcode]; ok {
		return msg
	} else {
		return "服务器开小差啦,稍后再来试一试"
	}
}

func IsCodeErr(errcode uint32) bool {
	if _, ok := message[errcode]; ok {
		return true
	} else {
		return false
	}
}
