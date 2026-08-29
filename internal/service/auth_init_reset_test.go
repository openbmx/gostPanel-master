package service

import (
	"path/filepath"
	"testing"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/pkg/jwt"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	// 出厂密码：Docker 镜像与发布包中的 config.yaml 曾内置该值
	factoryPassword = "admin123"
	// 用户在 Web UI 中自行设置的强密码
	userPassword = "S7r0ng!Passw0rd"
)

func newAuthTestService(t *testing.T) *AuthService {
	t.Helper()

	db, err := gorm.Open(
		sqlite.Open(filepath.Join(t.TempDir(), "test.db")),
		&gorm.Config{Logger: gormlogger.Discard},
	)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.User{}, &model.OperationLog{}, &model.SystemConfig{}); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	return NewAuthService(db, &jwt.Config{Secret: "test-secret-for-unit-tests", Expire: 7200})
}

// changeAdminPassword 走完整的改密流程
func changeAdminPassword(t *testing.T, svc *AuthService, oldPwd, newPwd string) {
	t.Helper()

	user, err := svc.userRepo.FindByUsername("admin")
	if err != nil {
		t.Fatalf("查询用户失败: %v", err)
	}
	if err := svc.ChangePassword(user.ID, &dto.ChangePasswordReq{
		OldPassword: oldPwd,
		NewPassword: newPwd,
	}, "127.0.0.1", "go-test"); err != nil {
		t.Fatalf("修改密码失败: %v", err)
	}
}

// TestInitDefaultAdmin_PasswordSurvivesRestart 回归测试 C-1。
//
// 旧实现每次启动都会比对 config.yaml 中的密码与数据库摘要，不一致就用配置值
// 覆盖数据库 —— 用户在 UI 里改过的密码会在重启/升级后被静默还原成出厂口令。
func TestInitDefaultAdmin_PasswordSurvivesRestart(t *testing.T) {
	svc := newAuthTestService(t)

	// 第 1 次启动：创建管理员
	if err := svc.InitDefaultAdmin("admin", factoryPassword, false); err != nil {
		t.Fatalf("首次初始化失败: %v", err)
	}

	// 用户通过 Web UI 修改密码
	changeAdminPassword(t, svc, factoryPassword, userPassword)

	user, _ := svc.userRepo.FindByUsername("admin")
	if !user.CheckPassword(userPassword) || user.CheckPassword(factoryPassword) {
		t.Fatal("前置条件不成立：改密未生效")
	}

	// 第 2 次启动：systemctl restart / docker restart / 升级后重启
	// config.yaml 未变，仍然传入出厂密码
	if err := svc.InitDefaultAdmin("admin", factoryPassword, false); err != nil {
		t.Fatalf("重启初始化失败: %v", err)
	}

	// 断言：重启不得改变用户设置的密码
	user, _ = svc.userRepo.FindByUsername("admin")
	if user.CheckPassword(factoryPassword) {
		t.Errorf("重启后出厂密码 %q 重新可用 —— 密码被 config.yaml 覆盖", factoryPassword)
	}
	if !user.CheckPassword(userPassword) {
		t.Errorf("重启后用户设置的密码失效")
	}
}

// TestInitDefaultAdmin_ForceResetOptIn 验证应急重置通道仍然可用，
// 但必须由运维显式开启。
func TestInitDefaultAdmin_ForceResetOptIn(t *testing.T) {
	svc := newAuthTestService(t)

	if err := svc.InitDefaultAdmin("admin", factoryPassword, false); err != nil {
		t.Fatalf("首次初始化失败: %v", err)
	}
	changeAdminPassword(t, svc, factoryPassword, userPassword)

	const recoveryPassword = "R3covery!Pass99"
	if err := svc.InitDefaultAdmin("admin", recoveryPassword, true); err != nil {
		t.Fatalf("强制重置失败: %v", err)
	}

	user, _ := svc.userRepo.FindByUsername("admin")
	if !user.CheckPassword(recoveryPassword) {
		t.Error("开启 force_reset 后密码未被重置")
	}
}

// TestChangePassword_RevokesExistingTokens 回归测试 M-2。
// 改密必须让此前签发的所有 Token 立即失效。
func TestChangePassword_RevokesExistingTokens(t *testing.T) {
	svc := newAuthTestService(t)
	if err := svc.InitDefaultAdmin("admin", factoryPassword, false); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 登录拿到一个有效 Token
	login, err := svc.Login(&dto.LoginReq{Username: "admin", Password: factoryPassword}, "127.0.0.1", "go-test")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	claims, err := svc.ParseToken(login.Token)
	if err != nil {
		t.Fatalf("解析 Token 失败: %v", err)
	}
	if err := svc.VerifyTokenVersion(claims.UserID, claims.TokenVersion); err != nil {
		t.Fatalf("改密前 Token 应当有效: %v", err)
	}

	// 改密
	changeAdminPassword(t, svc, factoryPassword, userPassword)

	// 同一个 Token 现在必须被拒绝：签名依然合法，但版本号已经过期
	if _, err := svc.ParseToken(login.Token); err != nil {
		t.Fatalf("Token 签名本身应仍然有效: %v", err)
	}
	if err := svc.VerifyTokenVersion(claims.UserID, claims.TokenVersion); err == nil {
		t.Error("改密后旧 Token 仍被判定为有效，Token 未被吊销")
	}
}

// TestChangePassword_EnforcesStrengthPolicy 回归测试 L-2
func TestChangePassword_EnforcesStrengthPolicy(t *testing.T) {
	svc := newAuthTestService(t)
	if err := svc.InitDefaultAdmin("admin", factoryPassword, false); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	user, _ := svc.userRepo.FindByUsername("admin")

	cases := []struct {
		name    string
		newPwd  string
		wantErr *errors.BizError
	}{
		{"过短", "Ab1!xy", errors.ErrPasswordTooShort},
		{"常见弱口令", "password123", errors.ErrPasswordTooCommon},
		{"字符类别不足", "abcdefghijkl", errors.ErrPasswordTooSimple},
		{"单字符重复", "aaaaaaaaaaaa", errors.ErrPasswordTooSimple},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.ChangePassword(user.ID, &dto.ChangePasswordReq{
				OldPassword: factoryPassword,
				NewPassword: tc.newPwd,
			}, "127.0.0.1", "go-test")
			if err != tc.wantErr {
				t.Errorf("期望 %v，实际 %v", tc.wantErr, err)
			}
		})
	}

	// 确认弱口令一个都没有写进去
	user, _ = svc.userRepo.FindByUsername("admin")
	if !user.CheckPassword(factoryPassword) {
		t.Error("被拒绝的弱口令不应改变现有密码")
	}
}

// TestLogin_RecordsFailedAttempts 回归测试 L-1。
// 失败的登录尝试必须留下审计记录，否则无法发现暴力破解。
func TestLogin_RecordsFailedAttempts(t *testing.T) {
	svc := newAuthTestService(t)
	if err := svc.InitDefaultAdmin("admin", factoryPassword, false); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 密码错误
	if _, err := svc.Login(&dto.LoginReq{Username: "admin", Password: "wrong-password"},
		"10.0.0.1", "go-test"); err != errors.ErrInvalidCredentials {
		t.Fatalf("期望凭据错误，实际 %v", err)
	}
	// 用户不存在——必须返回同一个错误，避免用户名枚举
	if _, err := svc.Login(&dto.LoginReq{Username: "nobody", Password: "whatever"},
		"10.0.0.2", "go-test"); err != errors.ErrInvalidCredentials {
		t.Fatalf("期望凭据错误，实际 %v", err)
	}

	logs, total, err := svc.logService.List(&dto.LogListReq{Action: model.ActionLoginFailed})
	if err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	if total != 2 {
		t.Errorf("期望 2 条登录失败审计记录，实际 %d", total)
	}
	for _, l := range logs {
		if l.IPAddress == "" {
			t.Error("登录失败记录缺少来源 IP")
		}
	}
}
