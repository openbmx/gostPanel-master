package router

import (
	"gost-panel/internal/config"
	"gost-panel/internal/handler"
	"gost-panel/internal/middleware"
	"gost-panel/internal/repository"
	"gost-panel/internal/service"
	"gost-panel/pkg/jwt"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Router 路由配置
type Router struct {
	db     *gorm.DB
	jwtCfg *jwt.Config
	srvCfg *config.ServerConfig
}

// NewRouter 创建路由实例
func NewRouter(db *gorm.DB, jwtCfg *jwt.Config, srvCfg *config.ServerConfig) *Router {
	return &Router{
		db:     db,
		jwtCfg: jwtCfg,
		srvCfg: srvCfg,
	}
}

// Setup 配置路由
func (r *Router) Setup(engine *gin.Engine) {
	// 创建 JWT 实例
	jwtInstance := jwt.New(r.jwtCfg)

	// 初始化服务
	authService := service.NewAuthService(r.db, r.jwtCfg)
	nodeService := service.NewNodeService(r.db)
	ruleService := service.NewRuleService(r.db)
	tunnelService := service.NewTunnelService(r.db)
	statsService := service.NewStatsService(r.db)
	logService := service.NewLogService(r.db)
	observerService := service.NewObserverService(r.db)

	// 初始化系统配置
	systemConfigRepo := repository.NewSystemConfigRepository(r.db)
	systemConfigService := service.NewSystemConfigService(systemConfigRepo)
	backupService := service.NewBackupService(r.db)

	// 全局中间件
	// SecurityHeaders 放在最前面，保证包括错误响应在内的所有出站响应都带上安全头
	engine.Use(middleware.SecurityHeaders(r.srvCfg.TLS.Enabled))
	engine.Use(middleware.CORS(r.srvCfg.CORSOrigins))
	engine.Use(middleware.Logger())
	engine.Use(middleware.Recovery())
	engine.Use(middleware.ErrorHandler())

	// 健康检查
	engine.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// API v1 路由组
	apiV1 := engine.Group("/api/v1")

	// API v1 健康检查
	// 安全：不再返回构建时间等内部信息，版本号供升级脚本判断，保留
	apiV1.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status":  "ok",
			"version": config.Version,
		})
	})

	// 初始化控制器
	authHandler := handler.NewAuthHandler(authService)
	nodeHandler := handler.NewNodeHandler(nodeService)
	ruleHandler := handler.NewRuleHandler(ruleService)
	tunnelHandler := handler.NewTunnelHandler(tunnelService)
	statsHandler := handler.NewStatsHandler(statsService)
	logHandler := handler.NewLogHandler(logService)
	observerHandler := handler.NewObserverHandler(observerService)
	systemConfigHandler := handler.NewSystemConfigHandler(systemConfigService, backupService)

	// 公开路由（无需管理员登录）
	{
		// 登录接口：失败次数限流，缓解暴力破解
		apiV1.POST("/auth/login", middleware.LoginRateLimit(), authHandler.Login)

		// 节点流量上报。
		// 不使用管理员 JWT（调用方是节点上的 GOST 进程），改用面板下发的
		// 独立上报令牌鉴权，见 ObserverHandler.Report。额外叠加速率上限兜底。
		apiV1.POST("/observer/report", middleware.ObserverRateLimit(), observerHandler.Report)

		// 登录页所需的公开配置（站点标题/Logo/Turnstile SiteKey）
		apiV1.GET("/system/public-config", systemConfigHandler.GetPublicConfig)
	}

	// 需要认证的路由
	authRoutes := apiV1.Group("")
	authRoutes.Use(middleware.Auth(jwtInstance, authService.VerifyTokenVersion))
	{
		// 认证相关
		authRoutes.GET("/auth/info", authHandler.GetUserInfo)
		authRoutes.PUT("/auth/password", authHandler.ChangePassword)
		authRoutes.POST("/auth/refresh", authHandler.RefreshToken)

		// 仪表盘统计
		authRoutes.GET("/dashboard/stats", statsHandler.GetDashboard)

		// 节点管理
		authRoutes.GET("/nodes", nodeHandler.List)
		authRoutes.GET("/nodes/:id", nodeHandler.GetByID)
		authRoutes.POST("/nodes", nodeHandler.Create)
		authRoutes.PUT("/nodes/:id", nodeHandler.Update)
		authRoutes.DELETE("/nodes/:id", nodeHandler.Delete)
		authRoutes.GET("/nodes/:id/config", nodeHandler.GetConfig)
		// 节点凭据：唯一会返回节点密码的接口，每次调用都会写入操作日志
		authRoutes.GET("/nodes/:id/credentials", nodeHandler.GetCredentials)

		// 规则管理
		authRoutes.GET("/rules", ruleHandler.List)
		authRoutes.GET("/rules/:id", ruleHandler.GetByID)
		authRoutes.POST("/rules", ruleHandler.Create)
		authRoutes.PUT("/rules/:id", ruleHandler.Update)
		authRoutes.DELETE("/rules/:id", ruleHandler.Delete)
		authRoutes.POST("/rules/:id/start", ruleHandler.Start)
		authRoutes.POST("/rules/:id/stop", ruleHandler.Stop)

		// 隧道管理
		authRoutes.GET("/tunnels", tunnelHandler.List)
		authRoutes.GET("/tunnels/:id", tunnelHandler.GetByID)
		authRoutes.POST("/tunnels", tunnelHandler.Create)
		authRoutes.PUT("/tunnels/:id", tunnelHandler.Update)
		authRoutes.DELETE("/tunnels/:id", tunnelHandler.Delete)
		authRoutes.POST("/tunnels/:id/start", tunnelHandler.Start)
		authRoutes.POST("/tunnels/:id/stop", tunnelHandler.Stop)

		// 操作日志
		authRoutes.GET("/logs", logHandler.List)

		// 系统设置
		authRoutes.GET("/system/config", systemConfigHandler.GetConfig)
		authRoutes.PUT("/system/config", systemConfigHandler.UpdateConfig)
		authRoutes.POST("/system/email/test", systemConfigHandler.TestEmail)
		authRoutes.POST("/system/backup", systemConfigHandler.Backup)
	}

	// 静态文件
	r.setupStatic(engine)
}
