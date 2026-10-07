package model

// 用户券状态。
//
// 状态机：
//
//	0 未使用 ──锁券(下单)──> 1 已锁定 ──支付成功──> 2 已核销（终态）
//	    │                        │
//	    │                        └──取消/超时释放──> 0 未使用
//	    └──到期──> 3 已过期（终态）
//
// 注意：status 字段本身充当乐观锁版本号 —— 所有状态迁移都用
// 「带 status 条件的 UPDATE」(CAS)，因此本表不需要额外的 version 列。
const (
	CouponStatusUnused  int64 = 0 // 未使用
	CouponStatusLocked  int64 = 1 // 已锁定（被某笔订单占用）
	CouponStatusUsed    int64 = 2 // 已核销（终态）
	CouponStatusExpired int64 = 3 // 已过期（终态）
)

// 券模板状态
const (
	CouponTemplateStatusOff int64 = 0 // 下架
	CouponTemplateStatusOn  int64 = 1 // 上架
)

// 券操作流水的 action（审计用）
const (
	CouponActionClaim   int64 = 1 // 领取
	CouponActionLock    int64 = 2 // 锁定
	CouponActionUse     int64 = 3 // 核销
	CouponActionRelease int64 = 4 // 释放
	CouponActionExpire  int64 = 5 // 过期
)

// 我的券列表：status 传该值表示"全部状态"
const CouponStatusAll int64 = -1
