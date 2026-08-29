package middleware

import (
	"strings"

	"gost-panel/pkg/jwt"
	"gost-panel/pkg/response"

	"github.com/gin-gonic/gin"
)

// maxTokenLength JWT 长度上限。
// 安全：解析前先做长度检查，避免超长 Authorization 头进入解析器
// （参考 CVE-2025-30204：大量 '.' 会导致 O(n) 内存分配）。
// 正常 Token 约 300 字节，4 KiB 已是极宽松的上限。
const maxTokenLength = 4096

// TokenVersionVerifier 校验 Token 中的版本号是否与数据库中的当前值一致。
// 返回 nil 表示有效。
type TokenVersionVerifier func(userID uint, tokenVersion int) error

// Auth JWT 认证中间件
func Auth(jwtInstance *jwt.JWT, verifyVersion TokenVersionVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 获取 Authorization 头
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			response.Unauthorized(c, "请先登录")
			c.Abort()
			return
		}

		// 检查 Bearer 前缀
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			response.Unauthorized(c, "认证格式错误")
			c.Abort()
			return
		}

		tokenString := parts[1]
		if len(tokenString) > maxTokenLength {
			response.Unauthorized(c, "Token 格式错误")
			c.Abort()
			return
		}

		// 解析 Token
		claims, err := jwtInstance.ParseToken(tokenString)
		if err != nil {
			switch err {
			case jwt.ErrTokenExpired:
				response.Unauthorized(c, "登录已过期，请重新登录")
			case jwt.ErrTokenMalformed:
				response.Unauthorized(c, "Token 格式错误")
			default:
				response.Unauthorized(c, "Token 无效")
			}
			c.Abort()
			return
		}

		// 校验令牌版本：改密后签发的旧 Token 必须立即失效。
		// 签名有效不等于 Token 仍然有效 —— JWT 无状态，这一步是唯一的吊销依据。
		if verifyVersion != nil {
			if err := verifyVersion(claims.UserID, claims.TokenVersion); err != nil {
				response.Unauthorized(c, "登录状态已失效，请重新登录")
				c.Abort()
				return
			}
		}

		// 将用户信息存储到上下文
		c.Set("userID", claims.UserID)
		c.Set("username", claims.Username)

		c.Next()
	}
}
