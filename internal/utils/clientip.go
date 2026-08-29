package utils

import (
	"net"
	"strings"

	"github.com/gin-gonic/gin"
)

// ClientIP 返回本项目唯一权威的客户端地址。
//
// 所有安全敏感路径（审计日志、限流、将来可能的 IP 白名单）都必须走这里，
// 而不是各自调用 c.ClientIP()。单一入口的意义在于：这些用途必须看到
// 完全一致的地址，否则会出现"限流按代理地址聚合、日志按真实地址记录"
// 这类难以察觉的不一致。
//
// 当前实现依赖 main.go 中的 SetTrustedProxies：
//   - 未配置受信任代理时，Gin 忽略 X-Forwarded-For，返回 TCP 对端地址；
//   - 配置了受信任代理时，才按可信链解析转发头。
func ClientIP(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return NormalizeIP(c.ClientIP())
}

// NormalizeIP 去除端口与首尾空白，返回规范化的 IP 字面量。
// 无法解析时返回原始输入的裁剪结果，便于日志排查。
func NormalizeIP(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}

	// 形如 "1.2.3.4:5678" 或 "[::1]:5678"
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	// 形如 "[::1]"
	value = strings.Trim(value, "[]")

	if ip := net.ParseIP(value); ip != nil {
		return ip.String()
	}
	return value
}

// RateLimitBucket 返回用于按客户端限流的分桶键。
//
// IPv4 直接使用地址本身；IPv6 则收敛到 /64 前缀。
//
// 这一点对 IPv6 是必需的：运营商通常给单个用户分配整个 /64（甚至 /56、/48），
// 攻击者在自己的 /64 内可以随意更换源地址。若按完整地址分桶，
// 每次请求都会落进新桶，按 IP 的限流形同虚设。
func RateLimitBucket(rawIP string) string {
	normalized := NormalizeIP(rawIP)
	if normalized == "" {
		return ""
	}

	ip := net.ParseIP(normalized)
	if ip == nil {
		// 解析不了就原样作为键，至少不会把不同来源混进同一个桶
		return normalized
	}

	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}

	// IPv6：掩到 /64
	masked := ip.Mask(net.CIDRMask(64, 128))
	if masked == nil {
		return ip.String()
	}
	return masked.String() + "/64"
}
