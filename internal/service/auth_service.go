// Package service 提供业务逻辑层服务
package service

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/internal/repository"
	"gost-panel/internal/utils"
	"gost-panel/pkg/jwt"
	"gost-panel/pkg/logger"

	"gorm.io/gorm"
)

// AuthService 认证服务
// 负责用户登录、Token 管理和密码修改
type AuthService struct {
	userRepo   *repository.UserRepository
	sysRepo    *repository.SystemConfigRepository
	logService *LogService
	jwt        *jwt.JWT
}

// NewAuthService 创建认证服务
func NewAuthService(db *gorm.DB, jwtCfg *jwt.Config) *AuthService {
	return &AuthService{
		userRepo:   repository.NewUserRepository(db),
		sysRepo:    repository.NewSystemConfigRepository(db),
		logService: NewLogService(db),
		jwt:        jwt.New(jwtCfg),
	}
}

// LoginResponse 登录响应
type LoginResponse struct {
	Token    string      `json:"token"`
	ExpireAt int64       `json:"expire_at"`
	User     *model.User `json:"user"`
}

// Login 用户登录
func (s *AuthService) Login(req *dto.LoginReq, ip, userAgent string) (*LoginResponse, error) {
	if err := s.verifyTurnstile(req.TurnstileToken, ip); err != nil {
		s.recordLoginFailure(req.Username, "人机验证失败", ip, userAgent)
		return nil, err
	}

	// 查询用户
	user, err := s.userRepo.FindByUsername(req.Username)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			// 安全：用户不存在与密码错误返回同一个错误，避免用户名枚举。
			// 同时补偿一次 bcrypt 计算，抹平"用户不存在"路径明显更快的时间差。
			model.DummyPasswordCheck()
			s.recordLoginFailure(req.Username, "用户不存在", ip, userAgent)
			return nil, errors.ErrInvalidCredentials
		}
		logger.Errorf("查询用户失败: %v", err)
		return nil, errors.ErrInternal
	}

	// 验证密码
	if !user.CheckPassword(req.Password) {
		s.recordLoginFailure(req.Username, "密码错误", ip, userAgent)
		return nil, errors.ErrInvalidCredentials
	}

	// 生成 Token
	token, err := s.jwt.GenerateToken(user.ID, user.Username, user.TokenVersion)
	if err != nil {
		logger.Errorf("生成 Token 失败: %v", err)
		return nil, errors.ErrTokenGenerationFailed
	}

	// 记录登录日志
	s.logService.Record(
		user.ID,
		user.Username,
		model.ActionLogin,
		model.ResourceTypeAuth,
		user.ID,
		"登录成功",
		ip,
		userAgent)

	return &LoginResponse{
		Token:    token,
		ExpireAt: time.Now().Add(time.Duration(s.jwt.ExpireSeconds()) * time.Second).Unix(),
		User:     user,
	}, nil
}

// recordLoginFailure 记录失败的登录尝试。
// 安全：没有失败审计就无法发现暴力破解，面板"操作日志"页此前只记成功登录。
// 注意只记录尝试的用户名，绝不记录尝试的口令。
func (s *AuthService) recordLoginFailure(username, reason, ip, userAgent string) {
	// 防止超长用户名撑爆日志表
	if len(username) > 64 {
		username = username[:64]
	}
	logger.Warnf("登录失败: user=%q reason=%s ip=%s", username, reason, ip)
	s.logService.Record(
		0,
		username,
		model.ActionLoginFailed,
		model.ResourceTypeAuth,
		0,
		reason,
		ip,
		userAgent)
}

func (s *AuthService) verifyTurnstile(token, remoteIP string) error {
	config, err := s.sysRepo.Get()
	if err != nil {
		logger.Errorf("读取系统配置失败: %v", err)
		return errors.ErrInternal
	}
	if !config.TurnstileEnabled {
		return nil
	}
	// 开启了人机验证但密钥缺失属于配置损坏。此前这里会让所有人都无法登录（自锁），
	// 现在降级为放行并高声告警：可用性优先，且缺失密钥本身不构成新的攻击面
	// —— 未开启验证时本来也是放行的。
	if strings.TrimSpace(config.TurnstileSecretKey) == "" {
		logger.Errorf("Turnstile 已启用但 Secret Key 为空，本次登录跳过人机验证。请到[系统设置-登录防护]重新填写密钥。")
		return nil
	}
	if strings.TrimSpace(token) == "" {
		return errors.ErrTurnstileVerificationFailed
	}

	form := url.Values{}
	form.Set("secret", config.TurnstileSecretKey)
	form.Set("response", token)
	// 带上客户端 IP，让 Cloudflare 侧也能做风控关联
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequest(http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		logger.Errorf("创建 Turnstile 验证请求失败: %v", err)
		return errors.ErrTurnstileVerificationFailed
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Errorf("Turnstile 验证请求失败: %v", err)
		return errors.ErrTurnstileVerificationFailed
	}
	defer resp.Body.Close()

	var result struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Errorf("解析 Turnstile 验证响应失败: %v", err)
		return errors.ErrTurnstileVerificationFailed
	}
	if resp.StatusCode != http.StatusOK || !result.Success {
		logger.Warnf("Turnstile 验证失败: status=%d, errors=%v", resp.StatusCode, result.ErrorCodes)
		return errors.ErrTurnstileVerificationFailed
	}

	return nil
}

// ChangePassword 修改密码
func (s *AuthService) ChangePassword(userID uint, req *dto.ChangePasswordReq, ip, userAgent string) error {
	// 查询用户
	user, err := s.userRepo.FindByID(userID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrUserNotFound
		}
		logger.Errorf("查询用户失败: %v", err)
		return errors.ErrInternal
	}

	// 验证旧密码
	if !user.CheckPassword(req.OldPassword) {
		s.recordLoginFailure(user.Username, "改密时原密码错误", ip, userAgent)
		return errors.ErrPasswordMismatch
	}

	// 强度校验
	if err = utils.ValidatePasswordStrength(req.NewPassword); err != nil {
		return err
	}
	if req.OldPassword == req.NewPassword {
		return errors.ErrPasswordReused
	}

	// 设置新密码
	if err = user.SetPassword(req.NewPassword); err != nil {
		logger.Errorf("口令哈希失败: %v", err)
		return errors.ErrInternal
	}

	// 更新密码，并递增 TokenVersion 使所有历史 Token 立即失效
	if err = s.userRepo.UpdatePasswordAndRevokeTokens(userID, user.Password); err != nil {
		logger.Errorf("更新密码失败: %v", err)
		return errors.ErrInternal
	}

	// 记录操作日志
	s.logService.Record(
		userID,
		user.Username,
		model.ActionChangePassword,
		model.ResourceTypeAuth,
		userID,
		"密码已修改，此前签发的所有 Token 已失效",
		ip,
		userAgent)

	return nil
}

// RefreshToken 刷新 Token
func (s *AuthService) RefreshToken(tokenString string) (string, error) {
	return s.jwt.RefreshToken(tokenString)
}

// ParseToken 解析 Token
func (s *AuthService) ParseToken(tokenString string) (*jwt.Claims, error) {
	return s.jwt.ParseToken(tokenString)
}

// VerifyTokenVersion 校验 Token 中携带的版本号是否仍然有效。
// 供认证中间件在每次请求时调用，是改密后吊销历史 Token 的依据。
func (s *AuthService) VerifyTokenVersion(userID uint, tokenVersion int) error {
	current, err := s.userRepo.GetTokenVersion(userID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrUserNotFound
		}
		logger.Errorf("读取令牌版本失败: %v", err)
		return errors.ErrInternal
	}
	if current != tokenVersion {
		return errors.ErrTokenRevoked
	}
	return nil
}

// GetUserByID 根据 ID 获取用户
func (s *AuthService) GetUserByID(id uint) (*model.User, error) {
	return s.userRepo.FindByID(id)
}

// InitDefaultAdmin 初始化管理员账号。
//
// 安全（C-1）：配置文件中的 admin.password 只用于「账号尚不存在时」的首次创建。
// 账号一旦创建，其口令的唯一真相来源就是数据库。
//
// 旧实现会在每次启动时比对配置口令与数据库摘要，不一致就用配置值覆盖数据库 ——
// 这意味着用户在 Web UI 里改过的密码，会在下一次重启/升级时被静默还原成
// config.yaml 里的出厂值（Docker 与发布包中该值为公开的 admin123）。
//
// forceReset 是给"管理员遗忘口令"准备的应急通道，必须由运维显式开启，
// 且开启后会强制吊销所有已签发的 Token。
func (s *AuthService) InitDefaultAdmin(username, password string, forceReset bool) error {
	user, err := s.userRepo.FindByUsername(username)

	// 只有确实是"记录不存在"才创建；数据库故障必须向上抛，
	// 否则会被误当成"用户不存在"而走进创建分支，掩盖真实问题。
	if stderrors.Is(err, gorm.ErrRecordNotFound) {
		admin := &model.User{
			Username:     username,
			Password:     password,
			TokenVersion: 1,
		}
		if err = s.userRepo.Create(admin); err != nil {
			return fmt.Errorf("创建管理员账号失败: %w", err)
		}
		logger.Infof("管理员账号已创建: %s", username)
		return nil
	}
	if err != nil {
		return fmt.Errorf("查询管理员账号失败: %w", err)
	}

	if !forceReset {
		// 正常路径：绝不触碰已存在账号的口令
		logger.Infof("管理员账号 %s 已存在，沿用数据库中的密码（配置文件中的 admin.password 已被忽略）", username)
		return nil
	}

	// 应急重置路径
	logger.Warnf("检测到 admin.force_reset=true，正在将管理员 %s 的密码重置为配置值", username)
	if err = user.SetPassword(password); err != nil {
		return fmt.Errorf("口令哈希失败: %w", err)
	}
	if err = s.userRepo.UpdatePasswordAndRevokeTokens(user.ID, user.Password); err != nil {
		return fmt.Errorf("重置管理员密码失败: %w", err)
	}
	logger.Warnf("管理员密码已重置，所有历史 Token 已失效。请立即移除 admin.force_reset 配置并重启，否则每次启动都会重置密码。")
	return nil
}
