package middleware

import (
	"net/url"
	"strings"
	"time"

	"gost-panel/pkg/logger"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

// CORS 跨域中间件。
//
// 安全：面板前端与 API 同源（前端由本进程以内嵌静态资源方式提供），
// 正常使用完全不需要跨域。此前的 AllowOrigins=["*"] 允许任意站点脚本
// 读取本面板的 API 响应，没有任何收益。
//
// 现在默认不放行任何跨域来源；仅当运维在[系统设置]中填写了面板地址
// （PanelURL）或显式配置了额外来源时，才把这些来源加入白名单
// —— 用于把前端单独部署在其他域名的场景。
//
// AllowCredentials 保持 false：认证走 Authorization 头，不依赖 Cookie。
func CORS(allowedOrigins []string) gin.HandlerFunc {
	origins := normalizeOrigins(allowedOrigins)

	if len(origins) == 0 {
		// 无白名单：完全不下发 CORS 头，浏览器会拦截一切跨域读取
		return func(c *gin.Context) {
			c.Next()
		}
	}

	logger.Infof("CORS 允许的来源: %s", strings.Join(origins, ", "))

	return cors.New(cors.Config{
		AllowOrigins: origins,
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowHeaders: []string{
			"Origin",
			"Content-Type",
			"Content-Length",
			"Accept-Encoding",
			"Authorization",
			"Accept",
			"X-Requested-With",
		},
		ExposeHeaders: []string{
			"Content-Length",
			"Content-Type",
		},
		AllowCredentials: false,
		MaxAge:           12 * time.Hour,
	})
}

// normalizeOrigins 把配置值规整为 scheme://host[:port] 形式，
// 丢弃空值、通配符和无法解析的项。
func normalizeOrigins(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))

	for _, item := range raw {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		// 明确拒绝通配符：与本中间件的设计意图相悖
		if item == "*" {
			logger.Warnf("已忽略 CORS 通配来源 \"*\"：面板不允许任意站点跨域访问 API")
			continue
		}

		u, err := url.Parse(item)
		if err != nil || u.Scheme == "" || u.Host == "" {
			logger.Warnf("已忽略无法解析的 CORS 来源: %q", item)
			continue
		}

		origin := u.Scheme + "://" + u.Host
		if _, dup := seen[origin]; dup {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}

	return out
}
