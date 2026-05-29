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

// Config 应用配置结构
type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	JWT      JWTConfig      `mapstructure:"jwt"`
	Log      LogConfig      `mapstructure:"log"`
	Admin    AdminConfig    `mapstructure:"admin"`
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Port string `mapstructure:"port"`
	Mode string `mapstructure:"mode"`
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
}

// 全局配置实例
var cfg *Config

// Load 加载配置文件
// configPath: 配置文件路径，为空则使用默认路径
func Load(configPath string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	// 环境变量覆盖（便于 Docker / Kubernetes 部署）：
	// 例如 GOSTPANEL_SERVER_PORT、GOSTPANEL_JWT_SECRET、GOSTPANEL_ADMIN_PASSWORD。
	v.SetEnvPrefix("GOSTPANEL")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	for _, key := range []string{
		"server.port", "server.mode",
		"database.type", "database.path",
		"jwt.secret", "jwt.expire",
		"log.level", "log.format", "log.output",
		"admin.username", "admin.password",
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
	if cfg.Admin.Username == "" {
		cfg.Admin.Username = "admin"
	}
	if cfg.Admin.Password == "" {
		cfg.Admin.Password = "admin123"
	}
	if cfg.Admin.Password == "admin123" {
		fmt.Fprintln(os.Stderr, "[安全警告] 正在使用默认管理员密码 admin123，存在被暴力破解风险，请登录后立即修改或通过配置 / 环境变量设置强密码。")
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
