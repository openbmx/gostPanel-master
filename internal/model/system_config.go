package model

import (
	"time"

	"gorm.io/gorm"
)

// SystemConfig 系统配置模型
type SystemConfig struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`

	// 面板地址
	PanelURL string `gorm:"size:255" json:"panel_url"`

	// 邮箱配置
	SMTPHost     string `gorm:"size:255" json:"smtp_host"`
	SMTPPort     int    `json:"smtp_port"`
	SMTPUsername string `gorm:"size:100" json:"smtp_username"`
	SMTPPassword string `gorm:"size:255" json:"smtp_password"`
	SMTPFrom     string `gorm:"size:255" json:"smtp_from"` // 发件人邮箱

	// 日志策略
	LogRetentionDays int    `gorm:"default:7" json:"log_retention_days"`
	LogLevel         string `gorm:"size:20;default:info" json:"log_level"`

	// 备份与恢复
	AutoBackup           bool `gorm:"default:false" json:"auto_backup"`
	BackupRetentionCount int  `gorm:"default:7" json:"backup_retention_count"`

	// 面板配置
	SiteTitle string `gorm:"size:100;default:Gost Panel" json:"site_title"`
	LogoURL   string `gorm:"size:255" json:"logo_url"`
	Copyright string `gorm:"size:255" json:"copyright"`

	// 登录防护
	TurnstileEnabled   bool   `gorm:"default:false" json:"turnstile_enabled"`
	TurnstileSiteKey   string `gorm:"size:255" json:"turnstile_site_key"`
	TurnstileSecretKey string `gorm:"size:255" json:"turnstile_secret_key"`

	// ObserverToken 节点流量上报令牌。
	// 安全：/api/v1/observer/report 无法使用管理员 JWT（调用方是各节点上的 GOST 进程），
	// 因此用一个独立的高熵令牌鉴权。面板在给节点下发观察器配置时会把它写进回调 URL。
	// 首次使用时自动生成，不会下发给前端。
	ObserverToken string `gorm:"size:128" json:"-"`
}

// TableName 指定表名
func (SystemConfig) TableName() string {
	return "system_configs"
}
