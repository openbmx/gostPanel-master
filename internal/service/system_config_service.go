package service

import (
	"strings"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/repository"
)

// SystemConfigService 系统配置服务
type SystemConfigService struct {
	repo *repository.SystemConfigRepository
}

// NewSystemConfigService 创建系统配置服务
func NewSystemConfigService(repo *repository.SystemConfigRepository) *SystemConfigService {
	return &SystemConfigService{repo: repo}
}

// SecretPlaceholder 已设置密钥的占位符。
//
// 前端在展示表单时收到该占位符（而非真实密钥），保存时原样回传即表示"不修改"。
// 这样同时解决两个问题：
//  1. 真实密钥不再离开服务端（此前 SMTP 密码是明文回显的）；
//  2. 前端不必持有真实值也能安全提交表单 —— 此前 Turnstile Secret 因为不在
//     响应里，保存任何一个设置项都会把它清空，导致人机验证失效甚至无法登录。
//
// #nosec G101 -- 这是一个哨兵标记，不是凭据；真实密钥永远不会离开服务端
const SecretPlaceholder = "__GOSTPANEL_UNCHANGED__"

// maskSecret 已设置则返回占位符，未设置返回空串
func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	return SecretPlaceholder
}

// resolveSecret 根据提交值决定最终要写入的密钥。
// 返回占位符或空串都表示保持原值不变。
func resolveSecret(submitted, current string) string {
	trimmed := strings.TrimSpace(submitted)
	if trimmed == "" || trimmed == SecretPlaceholder {
		return current
	}
	return submitted
}

// GetConfig 获取配置DTO
func (s *SystemConfigService) GetConfig() (*dto.SystemConfigResp, error) {
	config, err := s.repo.Get()
	if err != nil {
		return nil, err
	}

	return &dto.SystemConfigResp{
		Panel: dto.PanelConfigResp{
			PanelURL: config.PanelURL,
		},
		Email: dto.EmailConfigResp{
			Host:     config.SMTPHost,
			Port:     config.SMTPPort,
			Username: config.SMTPUsername,
			// 安全：不回显 SMTP 明文密码
			Password:  maskSecret(config.SMTPPassword),
			FromEmail: config.SMTPFrom,
		},
		Config: dto.PanelSettingResp{
			SiteTitle: config.SiteTitle,
			LogoURL:   config.LogoURL,
			Copyright: config.Copyright,
		},
		Login: dto.LoginProtectResp{
			TurnstileEnabled: config.TurnstileEnabled,
			TurnstileSiteKey: config.TurnstileSiteKey,
			// 不回显真实 Secret，只告知"是否已设置"
			TurnstileSecretKey: maskSecret(config.TurnstileSecretKey),
		},
		Log: dto.LogConfigResp{
			RetentionDays: config.LogRetentionDays,
			Level:         config.LogLevel,
		},
		Backup: dto.BackupConfigResp{
			AutoBackup:     config.AutoBackup,
			RetentionCount: config.BackupRetentionCount,
		},
	}, nil
}

// GetPublicConfig 获取公开配置
func (s *SystemConfigService) GetPublicConfig() (*dto.PublicSystemConfigResp, error) {
	config, err := s.repo.Get()
	if err != nil {
		return nil, err
	}

	return &dto.PublicSystemConfigResp{
		SiteTitle:        config.SiteTitle,
		LogoURL:          config.LogoURL,
		Copyright:        config.Copyright,
		TurnstileEnabled: config.TurnstileEnabled,
		TurnstileSiteKey: config.TurnstileSiteKey,
	}, nil
}

// UpdateConfig 更新配置
func (s *SystemConfigService) UpdateConfig(req *dto.UpdateSystemConfigReq) error {
	config, err := s.repo.Get()
	if err != nil {
		return err
	}

	// 映射 Panel
	config.PanelURL = strings.TrimSpace(req.Panel.PanelURL)

	// 映射 Email
	config.SMTPHost = req.Email.Host
	config.SMTPPort = req.Email.Port
	config.SMTPUsername = req.Email.Username
	// 空值/占位符表示不修改，避免保存其他设置时误清空
	config.SMTPPassword = resolveSecret(req.Email.Password, config.SMTPPassword)
	config.SMTPFrom = req.Email.FromEmail

	// 映射 Config
	config.SiteTitle = req.Config.SiteTitle
	config.LogoURL = req.Config.LogoURL
	config.Copyright = req.Config.Copyright

	config.TurnstileEnabled = req.Login.TurnstileEnabled
	config.TurnstileSiteKey = req.Login.TurnstileSiteKey
	// 同上：此前这里是无条件赋值，而 GET 接口又不返回该密钥，
	// 导致在任意标签页点一次"保存"就会清空 Secret，人机验证随之失效。
	config.TurnstileSecretKey = resolveSecret(req.Login.TurnstileSecretKey, config.TurnstileSecretKey)

	// 开启人机验证却没有密钥属于无效配置，直接拒绝而不是保存成一个坏状态
	if config.TurnstileEnabled &&
		(strings.TrimSpace(config.TurnstileSiteKey) == "" || strings.TrimSpace(config.TurnstileSecretKey) == "") {
		return errors.ErrTurnstileConfigIncomplete
	}

	// 映射 Log
	config.LogRetentionDays = req.Log.RetentionDays
	config.LogLevel = req.Log.Level

	// 映射 Backup
	config.AutoBackup = req.Backup.AutoBackup
	config.BackupRetentionCount = req.Backup.RetentionCount

	return s.repo.Update(config)
}

// ResolveSMTPPassword 供测试邮件接口使用：
// 前端持有的是占位符，需要在发送前换回真实密码。
func (s *SystemConfigService) ResolveSMTPPassword(submitted string) (string, error) {
	config, err := s.repo.Get()
	if err != nil {
		return "", err
	}
	return resolveSecret(submitted, config.SMTPPassword), nil
}
