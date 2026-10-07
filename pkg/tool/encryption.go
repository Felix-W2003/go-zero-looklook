package tool

import (
	"crypto/md5"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

/** 加密方式 **/

func Md5ByString(str string) string {
	m := md5.New()
	_, err := io.WriteString(m, str)
	if err != nil {
		panic(err)
	}
	arr := m.Sum(nil)
	return fmt.Sprintf("%x", arr)
}

func Md5ByBytes(b []byte) string {
	return fmt.Sprintf("%x", md5.Sum(b))
}

// HashPassword 使用 bcrypt 生成密码哈希。
// bcrypt 会自动生成随机盐并编码进哈希串，因此同一密码每次生成的结果都不同。
// 注意：bcrypt 不支持超过 72 字节的密码，超长会返回错误。
func HashPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

// CheckPassword 校验明文密码与 bcrypt 哈希是否匹配。
func CheckPassword(hashedPassword, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password)) == nil
}

// IsBcryptHash 判断存储的哈希是否为 bcrypt 格式，用于存量 MD5 数据的渐进式迁移。
// bcrypt 哈希固定 60 字符，形如 $2a$10$N9qo8uLOickgx2ZMRZoMye...
func IsBcryptHash(hash string) bool {
	return len(hash) == 60 && strings.HasPrefix(hash, "$2")
}
