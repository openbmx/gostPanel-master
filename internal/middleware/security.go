package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// contentSecurityPolicy 面板的内容安全策略。
//
// 说明各指令的取值理由：
//   - default-src 'self'      —— 兜底只允许同源
//   - script-src              —— 只允许同源脚本；Cloudflare Turnstile 的验证组件
//     必须从 challenges.cloudflare.com 加载，因此单独放行该域。
//     刻意不含 'unsafe-inline'/'unsafe-eval'：这是 CSP 防御 XSS 的核心，
//     为此已移除 index.html 中的内联脚本。
//   - style-src 'unsafe-inline' —— Element Plus 在运行时注入内联样式，无法避免。
//     样式注入的危害远低于脚本执行，是可接受的折中。
//   - img-src http: https: data: —— 站点 Logo 允许管理员填写任意外部 URL。
//     面板本身常以 http 运行，若只放行 https 会导致 http 图片被拦掉。
//     img-src 不涉及脚本执行，放宽的风险很低。
//   - connect-src              —— XHR 限制在本站；Turnstile 的 api.js 在主文档中
//     会向 challenges.cloudflare.com 发起请求，需一并放行，否则人机验证无法工作。
//   - frame-ancestors 'none'  —— 禁止被任何页面嵌套，防点击劫持
//   - object-src 'none' / base-uri 'self' / form-action 'self' —— 常规收敛
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' https://challenges.cloudflare.com; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: http: https:; " +
	"font-src 'self' data:; " +
	"connect-src 'self' https://challenges.cloudflare.com; " +
	"frame-src https://challenges.cloudflare.com; " +
	"frame-ancestors 'none'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// SecurityHeaders 统一注入安全响应头。
// enableHSTS 仅在面板本身以 HTTPS 监听时开启 —— 在明文 HTTP 上下发 HSTS
// 不会生效，反而可能在用户后续启用 HTTPS 前造成困扰。
func SecurityHeaders(enableHSTS bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()

		// 防 MIME 嗅探：静态资源处理器在无法识别扩展名时会回退到
		// application/octet-stream，没有这个头浏览器可能自行猜测并执行。
		h.Set("X-Content-Type-Options", "nosniff")
		// 防点击劫持（与 CSP frame-ancestors 双保险，兼顾老浏览器）
		h.Set("X-Frame-Options", "DENY")
		// 跨源跳转时不泄露完整 URL
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// 面板不需要这些设备能力，一律关闭
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")

		if enableHSTS {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		// API 响应一律禁止缓存，避免 Token、节点凭据等被中间层或磁盘缓存留存
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store, no-cache, must-revalidate")
			h.Set("Pragma", "no-cache")
		}

		c.Next()
	}
}
