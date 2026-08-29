package utils

import "strings"

// likeEscaper 转义 SQL LIKE 中的通配符。
// 注意：这不是防注入措施 —— 查询本身走参数绑定，不存在注入。
// 这里解决的是语义问题：用户搜索 "100%" 时，未转义的 % 会退化成"匹配任意内容"。
var likeEscaper = strings.NewReplacer(
	`\`, `\\`,
	`%`, `\%`,
	`_`, `\_`,
)

// EscapeLike 转义 LIKE 模式中的特殊字符，需配合 ESCAPE '\' 使用
func EscapeLike(s string) string {
	return likeEscaper.Replace(s)
}
