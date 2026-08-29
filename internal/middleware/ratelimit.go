package middleware

import (
	"strconv"
	"sync"
	"time"

	"gost-panel/pkg/logger"
	"gost-panel/pkg/response"

	"github.com/gin-gonic/gin"
)

// 登录防护参数。
//
// 相比旧实现的两点关键改动：
//  1. 只统计「失败」的尝试。旧实现把成功登录也计入窗口，正常使用反而更容易触顶，
//     而攻击者的失败尝试与合法用户共用同一个额度。
//  2. 阈值收紧到 5 次 / 5 分钟。旧实现是 10 次 / 分钟（= 600 次/小时），
//     对 bcrypt 校验来说仍然是相当可观的爆破速率。
//
// 前提：ClientIP() 必须可信。见 main.go 中的 SetTrustedProxies —— 若信任了
// 任意代理，攻击者只需轮换 X-Forwarded-For 即可让本限流器完全失效。
const (
	loginMaxFailures = 5
	loginWindow      = 5 * time.Minute
)

// maxTrackedIPs 同时跟踪的来源 IP 上限。
//
// 限流中间件在业务鉴权之前执行，所以未通过鉴权的请求同样会在 map 中建立条目。
// 没有上限时，一次分布式请求（每个 IP 都是真实完成 TCP 握手的主机）就能让
// 这张表持续增长到窗口结束 —— 限流器本身反而成了内存放大器。
// 5000 远超任何真实部署的节点数量，同时把最坏内存占用限制在可接受范围。
const maxTrackedIPs = 5000

// slidingWindowLimiter 基于客户端 IP 的滑动窗口计数器
type slidingWindowLimiter struct {
	mu       sync.Mutex
	hits     map[string][]time.Time
	window   time.Duration
	max      int
	lastSwwp time.Time

	// alertOnBurst 为 true 时，累计计数达到阈值会打印疑似分布式爆破的告警。
	// 只对登录接口启用 —— 观察器上报是正常高频流量，若共用会把日志刷满误报。
	alertOnBurst bool
	// 全局计数，用于识别分布式爆破。
	// 只告警不拦截：全局阈值一旦拦截，攻击者就能用它把合法管理员一起锁在门外。
	globalHits []time.Time
}

func newSlidingWindowLimiter(max int, window time.Duration, alertOnBurst bool) *slidingWindowLimiter {
	return &slidingWindowLimiter{
		hits:         make(map[string][]time.Time),
		window:       window,
		max:          max,
		alertOnBurst: alertOnBurst,
	}
}

// blockedFor 返回该 IP 还需等待多久；返回 0 表示当前允许尝试。
func (l *slidingWindowLimiter) blockedFor(ip string) time.Duration {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	recent := l.recentLocked(ip, now)
	if len(recent) < l.max {
		return 0
	}
	// 最早的那次失败滑出窗口后即可解锁
	return l.window - now.Sub(recent[0])
}

// record 记录一次计入配额的请求
func (l *slidingWindowLimiter) record(ip string) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.enforceCapacityLocked(ip, now)
	l.hits[ip] = append(l.recentLocked(ip, now), now)

	if !l.alertOnBurst {
		return
	}

	// 全局观测（仅登录接口）
	cutoff := now.Add(-l.window)
	kept := l.globalHits[:0]
	for _, t := range l.globalHits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.globalHits = append(kept, now)

	if n := len(l.globalHits); n > 0 && n%50 == 0 {
		logger.Warnf("安全告警：最近 %s 内累计 %d 次登录失败，涉及 %d 个来源 IP，疑似分布式爆破",
			l.window, n, len(l.hits))
	}
}

// recentLocked 返回窗口内的时间戳（调用方需持锁）
func (l *slidingWindowLimiter) recentLocked(ip string, now time.Time) []time.Time {
	cutoff := now.Add(-l.window)
	src := l.hits[ip]
	kept := src[:0]
	for _, t := range src {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.hits[ip] = kept
	return kept
}

// enforceCapacityLocked 在为新 IP 建立条目前保证表容量有界（调用方需持锁）。
//
// 先强制清理一次过期条目；若仍然超限，说明正在经历远超正常规模的分布式请求
// —— 此时按 IP 限流本就已经失去意义，直接整表重置以保证内存有界。
// 重置是 O(n) 的，不会像"淘汰最旧的一批"那样在攻击期间引入排序开销。
func (l *slidingWindowLimiter) enforceCapacityLocked(ip string, now time.Time) {
	if _, exists := l.hits[ip]; exists || len(l.hits) < maxTrackedIPs {
		return
	}

	l.forceSweepLocked(now)
	if len(l.hits) < maxTrackedIPs {
		return
	}

	logger.Warnf("限流器跟踪的来源 IP 超过 %d 个，疑似大规模分布式请求，已重置计数表", maxTrackedIPs)
	l.hits = make(map[string][]time.Time)
}

// forceSweepLocked 立即清理所有过期条目（调用方需持锁）
func (l *slidingWindowLimiter) forceSweepLocked(now time.Time) {
	for k, ts := range l.hits {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > l.window {
			delete(l.hits, k)
		}
	}
	l.lastSwwp = now
}

// sweepLocked 周期性清理过期 key，避免长期运行导致 map 无限膨胀（调用方需持锁）
func (l *slidingWindowLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSwwp) <= l.window {
		return
	}
	l.forceSweepLocked(now)
}

// LoginRateLimit 登录接口防爆破中间件。
// 先判断是否已被锁定，放行后根据响应状态码决定是否计入失败次数。
func LoginRateLimit() gin.HandlerFunc {
	limiter := newSlidingWindowLimiter(loginMaxFailures, loginWindow, true)

	return func(c *gin.Context) {
		ip := c.ClientIP()

		if wait := limiter.blockedFor(ip); wait > 0 {
			seconds := int(wait.Seconds()) + 1
			c.Header("Retry-After", strconv.Itoa(seconds))
			logger.Warnf("登录尝试过于频繁已被限流: ip=%s 剩余 %ds", ip, seconds)
			response.Error(c, 429, 42900, "登录失败次数过多，请 "+strconv.Itoa(seconds)+" 秒后再试")
			c.Abort()
			return
		}

		c.Next()

		// 只有失败才计数。成功登录（2xx）不消耗额度，避免正常使用被误伤。
		if c.Writer.Status() >= 400 {
			limiter.record(ip)
		}
	}
}

// observerMaxPerMinute 单个节点每分钟允许的上报次数上限。
// GOST 的 http observer 插件按 observer.period（本项目为 5s）批量上报，
// 即每个节点约 12 次/分钟。600 是极宽松的上限，仅作为令牌泄露后的兜底。
const observerMaxPerMinute = 600

// ObserverRateLimit 观察器上报接口的限流。
// 该接口由节点高频调用，阈值需要足够宽松；目的只是给一个写入端点兜底，
// 防止上报令牌泄露后被无限制刷。
// 注意 alertOnBurst=false：上报是正常高频流量，不能触发"疑似爆破"告警。
func ObserverRateLimit() gin.HandlerFunc {
	limiter := newSlidingWindowLimiter(observerMaxPerMinute, time.Minute, false)

	return func(c *gin.Context) {
		ip := c.ClientIP()
		if wait := limiter.blockedFor(ip); wait > 0 {
			c.Header("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			logger.Warnf("观察器上报超过速率上限已被限流: ip=%s", ip)
			c.AbortWithStatus(429)
			return
		}
		// 这里所有请求都计数（不区分成败），纯粹做速率上限
		limiter.record(ip)
		c.Next()
	}
}
