package dto

// ==================== 认证相关 ====================

// LoginReq 登录请求
// 这里只做长度上限约束，避免超大字段进入 bcrypt/日志；
// 不校验格式，否则会把存量弱口令用户挡在门外，反而无法登录进来改密。
type LoginReq struct {
	Username       string `json:"username" binding:"required,max=64"`  // 用户名
	Password       string `json:"password" binding:"required,max=256"` // 密码
	TurnstileToken string `json:"turnstile_token" binding:"max=4096"`  // Turnstile 验证令牌
}

// LoginResp 登录响应
type LoginResp struct {
	Token    string `json:"token"`     // JWT Token
	ExpireAt int64  `json:"expire_at"` // 过期时间戳
}

// ChangePasswordReq 修改密码请求
// 新密码的强度策略由 utils.ValidatePasswordStrength 在 service 层执行，
// 以便返回具体的失败原因（过短 / 过于常见 / 字符类别不足）。
type ChangePasswordReq struct {
	OldPassword string `json:"old_password" binding:"required,max=256"` // 原密码
	NewPassword string `json:"new_password" binding:"required,max=256"` // 新密码
}

// UserInfoResp 用户信息响应
type UserInfoResp struct {
	ID       uint   `json:"id"`       // 用户 ID
	Username string `json:"username"` // 用户名
	Role     string `json:"role"`     // 角色
}
