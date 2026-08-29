package model

import (
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// User 用户模型
type User struct {
	ID       uint   `gorm:"primaryKey" json:"id"`
	Username string `gorm:"uniqueIndex;size:50;not null" json:"username"`
	Password string `gorm:"size:255;not null" json:"-"` // 不返回密码
	Email    string `gorm:"size:100" json:"email"`

	// TokenVersion 令牌版本号。
	// 安全：该值会写入 JWT claims，认证中间件在每次请求时与数据库中的值比对。
	// 修改密码时递增，使此前签发的所有 Token 立即失效 —— JWT 无状态，
	// 没有这个版本号就无法在改密/凭据泄露后撤销已签发的 Token。
	TokenVersion int `gorm:"not null;default:1" json:"-"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName 指定表名
func (User) TableName() string {
	return "users"
}

// BeforeCreate 创建前钩子，对密码进行加密
func (u *User) BeforeCreate(tx *gorm.DB) error {
	return u.hashPassword()
}

// hashPassword 密码加密
func (u *User) hashPassword() error {
	if u.Password == "" {
		return nil
	}
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u.Password = string(hashedPassword)
	return nil
}

// CheckPassword 验证密码
func (u *User) CheckPassword(password string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password))
	return err == nil
}

// dummyHash 一个固定的 bcrypt 摘要，仅用于 DummyPasswordCheck 消耗等量 CPU 时间。
// 明文为随机串，不对应任何真实口令。
const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// DummyPasswordCheck 执行一次与真实校验等价的 bcrypt 运算。
// 安全：用户不存在时若直接返回，该路径会明显快于"用户存在但密码错误"，
// 攻击者可据此枚举有效用户名。这里补偿掉这个时间差。
func DummyPasswordCheck() {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte("dummy-password"))
}

// SetPassword 设置新密码（会自动加密）
func (u *User) SetPassword(password string) error {
	u.Password = password
	return u.hashPassword()
}
