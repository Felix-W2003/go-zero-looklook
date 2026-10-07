package tool

import (
	crand "crypto/rand"
	"encoding/hex"
	"math/rand"
	"time"
)

const (
	KC_RAND_KIND_NUM   = 0 // 纯数字
	KC_RAND_KIND_LOWER = 1 // 小写字母
	KC_RAND_KIND_UPPER = 2 // 大写字母
	KC_RAND_KIND_ALL   = 3 // 数字、大小写字母
)

// 随机字符串
func Krand(size int, kind int) string {
	ikind, kinds, result := kind, [][]int{[]int{10, 48}, []int{26, 97}, []int{26, 65}}, make([]byte, size)
	is_all := kind > 2 || kind < 0
	rand.Seed(time.Now().UnixNano())
	for i := 0; i < size; i++ {
		if is_all { // random ikind
			ikind = rand.Intn(3)
		}
		scope, base := kinds[ikind][0], kinds[ikind][1]
		result[i] = uint8(base + rand.Intn(scope))
	}
	return string(result)
}

// GenCouponCode 生成 32 位十六进制券码（16 字节随机数）。
//
// 为什么用 crypto/rand 而不是上面 Krand 用的 math/rand？
//
//	math/rand 是**可预测的伪随机**（给定种子能推出完整序列），够用于生成昵称这种场景；
//	而券码是对外可见、可用于兑换的标识，必须不可预测 —— 否则别人能推算出有效券码。
//
// 16 字节 → hex 编码后正好 32 个字符，与 user_coupon.coupon_code 的 varchar(32) 对应。
func GenCouponCode() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
