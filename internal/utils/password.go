package utils

import (
	"strings"
	"unicode"

	"gost-panel/internal/errors"
)

// 口令强度策略。
// 面板管理员账号等同于全部受管节点的控制权，且面板通常直接暴露在公网，
// 因此这里刻意比一般业务系统严格。
const (
	// MinPasswordLength 最小长度
	MinPasswordLength = 10
	// MaxPasswordLength 最大长度（bcrypt 只取前 72 字节，超出部分被静默截断，
	// 这里显式拒绝以免用户误以为超长口令都参与了校验）
	MaxPasswordLength = 72
	// MinPasswordClasses 至少需要覆盖的字符类别数（小写/大写/数字/符号 四选三）
	MinPasswordClasses = 3
)

// commonWeakPasswords 常见弱口令与本项目历史出厂口令。
// 不追求完整字典，只拦截最高频的几类，避免"合规但一撞就中"的口令。
var commonWeakPasswords = map[string]struct{}{
	"admin123":     {},
	"admin1234":    {},
	"admin12345":   {},
	"password":     {},
	"password1":    {},
	"password123":  {},
	"passw0rd":     {},
	"12345678":     {},
	"123456789":    {},
	"1234567890":   {},
	"qwertyuiop":   {},
	"1qaz2wsx":     {},
	"abc123456":    {},
	"administrator": {},
	"gostpanel":    {},
	"gost123456":   {},
	"zxcvbnm123456": {},
}

// PasswordPolicyDescription 供前端与错误提示复用的策略描述
const PasswordPolicyDescription = "密码至少 10 位，且需包含小写字母、大写字母、数字、符号中的至少三类"

// ValidatePasswordStrength 校验口令是否满足强度策略。
// 返回 nil 表示通过；否则返回可直接展示给用户的中文原因。
func ValidatePasswordStrength(password string) error {
	runes := []rune(password)

	if len(runes) < MinPasswordLength {
		return errors.ErrPasswordTooShort
	}
	// 按字节计算，与 bcrypt 的 72 字节上限保持一致
	if len(password) > MaxPasswordLength {
		return errors.ErrPasswordTooLong
	}

	if _, weak := commonWeakPasswords[strings.ToLower(password)]; weak {
		return errors.ErrPasswordTooCommon
	}

	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, r := range runes {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			// 空格、标点、符号，以及任何非 ASCII 字符都算作"符号"类
			hasSymbol = true
		}
	}

	classes := 0
	for _, ok := range []bool{hasLower, hasUpper, hasDigit, hasSymbol} {
		if ok {
			classes++
		}
	}
	if classes < MinPasswordClasses {
		return errors.ErrPasswordTooSimple
	}

	// 拒绝单字符重复（aaaaaaaaaa）这类满足长度但毫无熵的口令
	if isSingleRuneRepeat(runes) {
		return errors.ErrPasswordTooSimple
	}

	return nil
}

func isSingleRuneRepeat(runes []rune) bool {
	if len(runes) == 0 {
		return false
	}
	for _, r := range runes[1:] {
		if r != runes[0] {
			return false
		}
	}
	return true
}
