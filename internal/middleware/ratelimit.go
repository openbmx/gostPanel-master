package middleware

import (
	"sync"
	"time"

	"gost-panel/pkg/response"

	"github.com/gin-gonic/gin"
)

// loginRateLimiter 基于客户端 IP 的滑动窗口限流器，用于缓解登录接口的暴力破解。
type loginRateLimiter struct {
	mu       sync.Mutex
	hits     map[string][]time.Time
	window   time.Duration
	max      int
	lastSwwp time.Time
}

func newLoginRateLimiter(max int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{
		hits:   make(map[string][]time.Time),
		window: window,
		max:    max,
	}
}

// allow 判断指定 IP 在窗口内是否仍可继续尝试，并记录本次尝试。
func (l *loginRateLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// 周期性清理过期 key，避免长期运行导致 map 无限膨胀。
	if now.Sub(l.lastSwwp) > l.window {
		for k, ts := range l.hits {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > l.window {
				delete(l.hits, k)
			}
		}
		l.lastSwwp = now
	}

	cutoff := now.Add(-l.window)
	recent := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= l.max {
		l.hits[ip] = recent
		return false
	}

	l.hits[ip] = append(recent, now)
	return true
}

// LoginRateLimit 返回登录接口限流中间件：默认每个 IP 在 1 分钟内最多 10 次尝试。
func LoginRateLimit() gin.HandlerFunc {
	limiter := newLoginRateLimiter(10, time.Minute)
	return func(c *gin.Context) {
		if !limiter.allow(c.ClientIP()) {
			response.Error(c, 429, 42900, "登录尝试过于频繁，请稍后再试")
			c.Abort()
			return
		}
		c.Next()
	}
}
