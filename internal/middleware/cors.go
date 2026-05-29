package middleware

import (
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// CORS 跨域中间件
func CORS() gin.HandlerFunc {
	return cors.New(cors.Config{
		// 允许的来源
		AllowOrigins: []string{"*"},
		// 允许的方法
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		// 允许的请求头
		AllowHeaders: []string{
			"Origin",
			"Content-Type",
			"Content-Length",
			"Accept-Encoding",
			"Authorization",
			"Accept",
			"X-Requested-With",
		},
		// 暴露的响应头
		ExposeHeaders: []string{
			"Content-Length",
			"Content-Type",
		},
		// 是否允许携带凭证
		// 本面板使用 Authorization 头（Bearer Token）进行认证，不依赖 Cookie。
		// 同时允许 AllowOrigins=["*"] 与 AllowCredentials=true 既违反 CORS 规范，
		// 也会放大跨站攻击面，因此关闭凭证携带。
		AllowCredentials: false,
		// 预检请求缓存时间
		MaxAge: 12 * time.Hour,
	})
}
