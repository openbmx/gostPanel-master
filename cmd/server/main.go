package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"gost-panel/internal/config"
	"gost-panel/internal/model"
	"gost-panel/internal/router"
	"gost-panel/internal/service"
	"gost-panel/internal/utils"
	"gost-panel/pkg/jwt"
	"gost-panel/pkg/logger"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	// 解析命令行参数
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "c", "", "配置文件路径")
	flag.StringVar(&configPath, "config", "", "配置文件路径")
	flag.BoolVar(&showVersion, "version", false, "打印版本号并退出")
	flag.BoolVar(&showVersion, "v", false, "打印版本号并退出")
	flag.Parse()

	// 打印版本号（供升级脚本/运维查询）
	if showVersion {
		fmt.Println("gost-panel", config.Version)
		return
	}

	// 加载配置
	cfg, err := config.Load(configPath)
	if err != nil {
		panic("加载配置失败: " + err.Error())
	}

	// 初始化日志
	if err = logger.Init(&logger.Config{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Output: cfg.Log.Output,
	}); err != nil {
		panic("初始化日志失败: " + err.Error())
	}
	defer func() {
		_ = logger.Sync()
	}()

	logger.Info("Gost Panel 启动中...")

	// 设置 Gin 模式
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 迁移历史遗留的数据库位置（Docker 下老位置不在持久化卷内）
	if err = migrateLegacyDatabase(cfg); err != nil {
		logger.Fatalf("迁移数据库失败: %v", err)
	}

	// 初始化数据库
	db, err := initDatabase(cfg)
	if err != nil {
		logger.Fatalf("初始化数据库失败: %v", err)
	}
	logger.Info("数据库初始化完成")

	// 自动迁移
	if err = autoMigrate(db); err != nil {
		logger.Fatalf("数据库迁移失败: %v", err)
	}
	logger.Info("数据库迁移完成")

	// 初始化默认管理员
	if err = initDefaultAdmin(db, cfg); err != nil {
		logger.Fatalf("初始化管理员失败: %v", err)
	}

	// 初始化系统配置
	if err = initSystemConfig(db); err != nil {
		logger.Fatalf("初始化系统配置失败: %v", err)
	}

	// 创建 Gin 引擎
	engine := gin.New()

	// 安全：Gin 默认信任所有代理（trustedProxies = 0.0.0.0/0），
	// 此时 ClientIP() 会直接采信请求头里的 X-Forwarded-For / X-Real-IP。
	// 后果是攻击者只要每次请求换一个 XFF 值，就能绕过登录限流，
	// 并让操作日志里的来源 IP 完全失真。
	// 因此默认改为不信任任何代理；确有反代时再通过 server.trusted_proxies 显式声明。
	if err = engine.SetTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		logger.Fatalf("配置受信任代理失败: %v", err)
	}
	if len(cfg.Server.TrustedProxies) == 0 {
		logger.Info("未配置受信任代理，ClientIP 取 TCP 对端地址，忽略 X-Forwarded-For")
	} else {
		logger.Infof("受信任代理: %v", cfg.Server.TrustedProxies)
	}

	// 配置路由
	jwtCfg := &jwt.Config{
		Secret: cfg.JWT.Secret,
		Expire: cfg.JWT.Expire,
	}
	r := router.NewRouter(db, jwtCfg, &cfg.Server)
	r.Setup(engine)

	srv := &http.Server{
		Addr:    cfg.Server.Port,
		Handler: engine,
		// 防止慢速连接长期占用资源
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 限制请求头大小，配合中间件里的 Token 长度检查
		MaxHeaderBytes: 1 << 20, // 1 MiB
	}

	// 启动服务器
	go func() {
		if cfg.Server.TLS.Enabled {
			logger.Infof("服务器以 HTTPS 启动在 %s", cfg.Server.Port)
			err = srv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			logger.Infof("服务器以 HTTP 启动在 %s", cfg.Server.Port)
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("服务器启动失败: %v", err)
		}
	}()

	// 启动节点健康检测服务
	healthService := service.NewNodeHealthService(db)
	healthService.Start()

	// 启动规则状态同步服务
	syncService := service.NewRuleSyncService(db)
	syncService.Start()

	// 启动自动备份服务
	backupService := service.NewBackupService(db)
	backupService.Start()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Gost Panel 正在关闭...")

	// 先停止接收新请求，给在途请求 10 秒完成时间
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = srv.Shutdown(shutdownCtx); err != nil {
		logger.Warnf("HTTP 服务优雅关闭超时: %v", err)
	}

	// 停止相关的后台服务
	healthService.Stop()
	syncService.Stop()
	backupService.Stop()
}

// migrateLegacyDatabase 把历史版本遗留在工作目录下的数据库迁移到配置指定的位置。
//
// 背景：Docker 镜像内的 database.path 曾是 "./gost-panel.db"（即 /app/gost-panel.db），
// 而 compose 只挂载了 /app/data 与 /app/logs —— 数据库根本不在卷里，
// 一旦 `docker compose pull && up -d` 重建容器，全部面板数据都会丢失。
// 现在默认路径改到 ./data/ 下，这里负责把老位置的数据库无损搬过去。
func migrateLegacyDatabase(cfg *config.Config) error {
	const legacyPath = "./gost-panel.db"

	target := cfg.Database.Path
	if target == "" || filepath.Clean(target) == filepath.Clean(legacyPath) {
		return nil
	}
	// 目标已存在，说明已经迁移过或是全新部署
	if _, err := os.Stat(target); err == nil {
		return nil
	}
	// 老位置没有数据库，无需迁移
	if _, err := os.Stat(legacyPath); err != nil {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	if err := os.Rename(legacyPath, target); err != nil {
		return fmt.Errorf("迁移数据库 %s -> %s 失败: %w", legacyPath, target, err)
	}
	// SQLite 的 WAL / shm 附属文件一并搬走，避免残留导致状态不一致
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(legacyPath + suffix); err == nil {
			_ = os.Rename(legacyPath+suffix, target+suffix)
		}
	}

	logger.Warnf("已将数据库从 %s 迁移到 %s（该位置在 Docker 中位于持久化卷内）", legacyPath, target)
	return nil
}

// initDatabase 初始化数据库
func initDatabase(cfg *config.Config) (*gorm.DB, error) {
	// 确保数据库所在目录存在
	if dir := filepath.Dir(cfg.Database.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	// 配置 GORM 日志
	var gormLogLevel gormlogger.LogLevel
	switch cfg.Log.Level {
	case "debug":
		gormLogLevel = gormlogger.Info
	case "info":
		gormLogLevel = gormlogger.Warn
	default:
		gormLogLevel = gormlogger.Error
	}

	gormConfig := &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormLogLevel),
	}

	// 连接 SQLite 数据库
	db, err := gorm.Open(sqlite.Open(cfg.Database.Path), gormConfig)
	if err != nil {
		return nil, err
	}

	return db, nil
}

// autoMigrate 自动迁移数据库表结构
func autoMigrate(db *gorm.DB) error {
	// 1. 执行自动迁移（添加新字段）
	if err := db.AutoMigrate(
		&model.User{},
		&model.GostNode{},
		&model.GostRule{},
		&model.GostTunnel{},
		&model.OperationLog{},
		&model.SystemConfig{},
	); err != nil {
		return err
	}

	return nil
}

// initDefaultAdmin 初始化管理员账号。
// 安全：配置文件中的密码只用于账号不存在时的首次创建，
// 绝不会覆盖用户后来在 Web UI 中修改的密码（除非显式开启 admin.force_reset）。
func initDefaultAdmin(db *gorm.DB, cfg *config.Config) error {
	jwtCfg := &jwt.Config{
		Secret: cfg.JWT.Secret,
		Expire: cfg.JWT.Expire,
	}
	authService := service.NewAuthService(db, jwtCfg)
	return authService.InitDefaultAdmin(cfg.Admin.Username, cfg.Admin.Password, cfg.Admin.ForceReset)
}

// initSystemConfig 初始化系统配置
func initSystemConfig(db *gorm.DB) error {
	var count int64
	if err := db.Model(&model.SystemConfig{}).Count(&count).Error; err != nil {
		return err
	}

	if count == 0 {
		// 节点上报令牌，首次初始化时生成
		token, err := utils.RandomToken(32)
		if err != nil {
			return err
		}

		// 设置默认值
		sysConfig := &model.SystemConfig{
			SiteTitle:            "Gost Panel",
			LogoURL:              "https://gost.run/images/gost.png",
			Copyright:            "https://github.com/openbmx/gostPanel-master",
			LogRetentionDays:     7,
			LogLevel:             "info",
			AutoBackup:           false,
			BackupRetentionCount: 7,
			TurnstileEnabled:     false,
			ObserverToken:        token,
		}
		if err := db.Create(sysConfig).Error; err != nil {
			return err
		}
		logger.Info("初始化默认系统配置完成")
	}
	return nil
}
