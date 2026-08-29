package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Version 编译时注入的版本号
var Version = "dev"

// weakDefaultJWTSecret 历史版本中硬编码的弱默认密钥。
// 一旦泄露，任何人都能伪造管理员 Token，因此检测到它时会强制替换为随机密钥。
const weakDefaultJWTSecret = "zxcvbnm123456"

// weakDefaultAdminPassword 历史版本中随镜像/发布包一起分发的出厂口令。
// 检测到它时会替换为随机初始口令，避免公开默认凭据流入生产环境。
const weakDefaultAdminPassword = "admin123"

// defaultUpdateRepo 在线更新的默认来源仓库。
// 面板会从这里的 Releases 下载并替换自己的二进制，
// 因此它必须指向本项目自身 —— 指向别处等于安装别人构建的产物。
const defaultUpdateRepo = "openbmx/gostPanel-master"

// Config 应用配置结构
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	JWT      JWTConfig      `mapstructure:"jwt"`
	Log      LogConfig      `mapstructure:"log"`
	Admin    AdminConfig    `mapstructure:"admin"`
	Update   UpdateConfig   `mapstructure:"update"`
}

// UpdateConfig 面板内在线更新配置
type UpdateConfig struct {
	// Enabled 是否允许从面板内执行更新。关闭后仍可查看版本信息。
	Enabled bool `mapstructure:"enabled"`

	// Repo 更新来源仓库，形如 owner/name。
	// 安全：刻意不提供环境变量覆盖 —— 能改环境变量的人本就能改二进制，
	// 但把它做成"可远程配置"会给面板增加一条把自己指向任意仓库的路径。
	Repo string `mapstructure:"repo"`

	// MirrorPrefix 可选的下载加速前缀，形如 https://ghfast.top/
	//
	// 安全：仅用于加速下载体积较大的二进制包；校验和文件始终直连 GitHub 获取。
	// 因此即便镜像不可信也无法投毒 —— 它没法同时替换掉校验基准。
	MirrorPrefix string `mapstructure:"mirror_prefix"`
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Port string `mapstructure:"port"`
	Mode string `mapstructure:"mode"`

	// TrustedProxies 受信任的反向代理 CIDR 列表。
	// 安全：留空表示不信任任何代理，此时 ClientIP() 只取 TCP 连接的真实对端地址，
	// X-Forwarded-For / X-Real-IP 一律忽略。这是防止限流绕过与审计日志伪造的关键。
	// 仅当面板确实部署在 Nginx/Caddy/CDN 之后时，才填写这些代理的 CIDR。
	TrustedProxies []string `mapstructure:"trusted_proxies"`

	// CORSOrigins 额外允许跨域访问 API 的来源，形如 https://panel.example.com。
	// 安全：默认留空 = 不允许任何跨域访问。面板前端与 API 同源，正常部署无需配置；
	// 仅在前端被单独部署到其他域名时才需要填写。切勿填 "*"。
	CORSOrigins []string `mapstructure:"cors_origins"`

	// ReadHeaderTimeout 完整读取请求头的最大秒数，用于限制慢速请求头攻击。
	// 不限制请求体读取与响应写出。
	ReadHeaderTimeout int `mapstructure:"read_header_timeout"`
	// ReadTimeout 读取整个请求（含请求体）的最大秒数
	ReadTimeout int `mapstructure:"read_timeout"`
	// WriteTimeout 写出响应的最大秒数
	WriteTimeout int `mapstructure:"write_timeout"`
	// IdleTimeout Keep-Alive 空闲连接超时秒数
	IdleTimeout int `mapstructure:"idle_timeout"`
	// MaxHeaderBytes 请求头上限（字节），同时约束 HTTP/2 header list
	MaxHeaderBytes int `mapstructure:"max_header_bytes"`

	// TLS HTTPS 配置
	TLS TLSConfig `mapstructure:"tls"`
}

// TLSConfig HTTPS 配置
type TLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Type string `mapstructure:"type"`
	Path string `mapstructure:"path"`
}

// JWTConfig JWT 配置
type JWTConfig struct {
	Secret string `mapstructure:"secret"`
	Expire int64  `mapstructure:"expire"`
}

// LogConfig 日志配置
type LogConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// AdminConfig 管理员配置
type AdminConfig struct {
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`

	// ForceReset 应急口令重置开关（GOSTPANEL_ADMIN_FORCE_RESET=true）。
	// 安全：默认 false。管理员账号一旦创建，其密码由数据库唯一持有，
	// 配置文件中的 password 只用于首次初始化，绝不会在重启时覆盖用户改过的密码。
	// 仅在管理员遗忘口令时，临时设置此开关启动一次以强制重置，随后必须移除。
	ForceReset bool `mapstructure:"force_reset"`
}

// 全局配置实例
var cfg *Config

// Load 加载配置文件
// configPath: 配置文件路径，为空则使用默认路径
func Load(configPath string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	// 布尔项必须用 SetDefault 而非 setDefaults()：解析后无法区分
	// "显式配置为 false" 和 "根本没配置"，在 setDefaults 里会把用户的 false 覆盖掉。
	v.SetDefault("update.enabled", true)

	// 环境变量覆盖（便于 Docker / Kubernetes 部署）：
	// 例如 GOSTPANEL_SERVER_PORT、GOSTPANEL_JWT_SECRET、GOSTPANEL_ADMIN_PASSWORD。
	v.SetEnvPrefix("GOSTPANEL")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for _, key := range []string{
		"server.port", "server.mode", "server.trusted_proxies", "server.cors_origins",
		"server.tls.enabled", "server.tls.cert_file", "server.tls.key_file",
		"server.read_header_timeout", "server.read_timeout", "server.write_timeout",
		"server.idle_timeout", "server.max_header_bytes",
		"database.type", "database.path",
		"jwt.secret", "jwt.expire",
		"log.level", "log.format", "log.output",
		"admin.username", "admin.password", "admin.force_reset",
		"update.enabled", "update.mirror_prefix",
	} {
		_ = v.BindEnv(key)
	}

	// 确定配置文件路径
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		// 获取可执行文件所在目录
		execPath, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("获取可执行文件路径失败: %w", err)
		}
		execDir := filepath.Dir(execPath)

		// 添加配置文件搜索路径
		v.SetConfigName("config")
		v.AddConfigPath(filepath.Join(execDir, "config"))
		v.AddConfigPath("./config")
		v.AddConfigPath(".")
	}

	// 读取配置文件
	if err := v.ReadInConfig(); err != nil {
		var configFileNotFoundError viper.ConfigFileNotFoundError
		if !errors.As(err, &configFileNotFoundError) {
			return nil, fmt.Errorf("读取配置文件失败: %w", err)
		}
	}

	// 解析配置到结构体
	cfg = &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 设置默认值
	setDefaults(cfg)

	return cfg, nil
}

// setDefaults 设置配置默认值
func setDefaults(cfg *Config) {
	// 服务器默认配置
	if cfg.Server.Port == "" {
		cfg.Server.Port = ":39100"
	}
	if cfg.Server.Mode == "" {
		cfg.Server.Mode = "debug"
	}

	// HTTP 服务超时与请求头上限。
	// 默认值面向"面板"这类短请求场景；若前面挂了慢速链路可适当放大。
	if cfg.Server.ReadHeaderTimeout <= 0 {
		cfg.Server.ReadHeaderTimeout = 10
	}
	if cfg.Server.ReadTimeout <= 0 {
		cfg.Server.ReadTimeout = 60
	}
	if cfg.Server.WriteTimeout <= 0 {
		cfg.Server.WriteTimeout = 120
	}
	if cfg.Server.IdleTimeout <= 0 {
		cfg.Server.IdleTimeout = 120
	}
	if cfg.Server.MaxHeaderBytes <= 0 {
		cfg.Server.MaxHeaderBytes = 64 << 10 // 64 KiB
	}

	// 数据库默认配置
	if cfg.Database.Type == "" {
		cfg.Database.Type = "sqlite"
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "./gost-panel.db"
	}

	// JWT 默认配置
	// 安全：绝不允许使用空密钥或历史弱默认密钥，否则任何人都能伪造管理员 Token。
	if cfg.JWT.Secret == "" || cfg.JWT.Secret == weakDefaultJWTSecret {
		if secret, err := randomSecret(48); err == nil {
			cfg.JWT.Secret = secret
			fmt.Fprintln(os.Stderr, "[安全警告] 未配置有效的 jwt.secret，已自动生成随机密钥。")
			fmt.Fprintln(os.Stderr, "          注意：随机密钥在每次重启后都会变化，导致已签发的 Token 失效（需重新登录）。")
			fmt.Fprintln(os.Stderr, "          请在 config.yaml 的 jwt.secret 或环境变量 GOSTPANEL_JWT_SECRET 中设置固定且足够强的密钥。")
		} else {
			// 极端情况下随机源不可用，退回弱默认值但给出明确警告。
			cfg.JWT.Secret = weakDefaultJWTSecret
			fmt.Fprintln(os.Stderr, "[安全警告] 生成随机 jwt.secret 失败，临时使用不安全的默认密钥，请尽快手动配置！")
		}
	}
	if cfg.JWT.Expire == 0 {
		cfg.JWT.Expire = 7200 // 2小时
	}

	// 日志默认配置
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = "json"
	}
	if cfg.Log.Output == "" {
		cfg.Log.Output = "./logs/app.log"
	}

	// 管理员默认配置
	// 安全：这里的密码只在数据库中尚不存在该管理员时用于首次创建。
	// 若未显式配置，生成一次性随机初始口令并打印，避免出厂弱口令 admin123 流入生产。
	if cfg.Admin.Username == "" {
		cfg.Admin.Username = "admin"
	}
	if cfg.Admin.Password == "" || cfg.Admin.Password == weakDefaultAdminPassword {
		if pwd, err := randomSecret(12); err == nil {
			cfg.Admin.Password = pwd
			fmt.Fprintln(os.Stderr, "[安全提示] 未配置 admin.password（或仍为弱默认值），已生成随机初始密码：")
			fmt.Fprintf(os.Stderr, "          %s\n", pwd)
			fmt.Fprintln(os.Stderr, "          该密码仅在首次创建管理员账号时生效；若账号已存在则不会有任何变化。")
			fmt.Fprintln(os.Stderr, "          请立即登录并修改密码，随后可忽略此提示。")
		} else {
			cfg.Admin.Password = weakDefaultAdminPassword
			fmt.Fprintln(os.Stderr, "[安全警告] 生成随机初始密码失败，回退到弱默认口令 admin123，请务必登录后立即修改！")
		}
	}

	// 在线更新默认值。默认开启但受多重前置条件约束（见 UpdateService.checkUpdatable）：
	// Docker 环境、非发布构建、二进制目录不可写时都会自动拒绝。
	if cfg.Update.Repo == "" {
		cfg.Update.Repo = defaultUpdateRepo
	}

	// TLS 配置校验：显式开启但未提供证书时直接失败，避免"以为开了 HTTPS 实际是明文"。
	if cfg.Server.TLS.Enabled && (cfg.Server.TLS.CertFile == "" || cfg.Server.TLS.KeyFile == "") {
		fmt.Fprintln(os.Stderr, "[安全警告] server.tls.enabled=true 但未提供 cert_file/key_file，TLS 未生效，仍以明文 HTTP 监听！")
		cfg.Server.TLS.Enabled = false
	}
	if !cfg.Server.TLS.Enabled {
		fmt.Fprintln(os.Stderr, "[安全提示] 面板正以明文 HTTP 监听。登录口令与 Token 会以明文传输，")
		fmt.Fprintln(os.Stderr, "          请配置 server.tls 或在前面放置 HTTPS 反向代理后再暴露到公网。")
	}
}

// randomSecret 生成指定字节数的密码学安全随机串（十六进制编码）。
func randomSecret(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Get 获取全局配置实例
func Get() *Config {
	if cfg == nil {
		panic("配置未初始化，请先调用 Load 方法")
	}
	return cfg
}
