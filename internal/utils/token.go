package utils

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
)

// RandomToken 生成 numBytes 字节的密码学安全随机令牌（十六进制编码）
func RandomToken(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// SecureCompare 恒定时间字符串比较。
// 安全：令牌校验必须避免按字节短路的比较，否则响应时间会泄露前缀匹配长度，
// 使攻击者可以逐字节猜解令牌。
func SecureCompare(a, b string) bool {
	// subtle.ConstantTimeCompare 在长度不等时直接返回 0，
	// 长度本身不是秘密（令牌长度固定），可以接受。
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
