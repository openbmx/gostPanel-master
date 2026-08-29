package jwt

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims 自定义 JWT 声明
type Claims struct {
	UserID   uint   `json:"user_id"`
	Username string `json:"username"`
	// TokenVersion 签发时用户的令牌版本号。
	// 安全：认证中间件会与数据库中的当前值比对，不一致则拒绝 —— 这是改密后
	// 立即吊销所有历史 Token 的依据。
	TokenVersion int `json:"tv"`
	jwt.RegisteredClaims
}

// Config JWT 配置
type Config struct {
	Secret string // 密钥
	Expire int64  // 过期时间（秒）
}

// JWT 实例
type JWT struct {
	config *Config
}

// 错误定义
var (
	ErrTokenExpired     = errors.New("token 已过期")
	ErrTokenNotValidYet = errors.New("token 尚未生效")
	ErrTokenMalformed   = errors.New("token 格式错误")
	ErrTokenInvalid     = errors.New("token 无效")
)

// New 创建 JWT 实例
func New(cfg *Config) *JWT {
	return &JWT{config: cfg}
}

// GenerateToken 生成 Token
func (j *JWT) GenerateToken(userID uint, username string, tokenVersion int) (string, error) {
	now := time.Now()
	expireAt := now.Add(time.Duration(j.config.Expire) * time.Second)

	claims := Claims{
		UserID:       userID,
		Username:     username,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expireAt),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    "gost-panel",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(j.config.Secret))
}

// ExpireSeconds 返回 Token 的有效期（秒）
func (j *JWT) ExpireSeconds() int64 {
	return j.config.Expire
}

// ParseToken 解析 Token
func (j *JWT) ParseToken(tokenString string) (*Claims, error) {
	// 显式限定仅接受 HS256，防止 alg 混淆 / alg=none 等签名算法降级攻击。
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, ErrTokenInvalid
		}
		return []byte(j.config.Secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))

	if err != nil {
		// 判断具体错误类型
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		if errors.Is(err, jwt.ErrTokenNotValidYet) {
			return nil, ErrTokenNotValidYet
		}
		if errors.Is(err, jwt.ErrTokenMalformed) {
			return nil, ErrTokenMalformed
		}
		return nil, ErrTokenInvalid
	}

	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		return claims, nil
	}

	return nil, ErrTokenInvalid
}

// RefreshToken 刷新 Token
// 注意：调用方必须已经通过认证中间件校验过 TokenVersion，
// 否则被吊销的 Token 可以借由刷新接口无限续期。
func (j *JWT) RefreshToken(tokenString string) (string, error) {
	claims, err := j.ParseToken(tokenString)
	if err != nil {
		return "", err
	}

	// 生成新 Token
	return j.GenerateToken(claims.UserID, claims.Username, claims.TokenVersion)
}
