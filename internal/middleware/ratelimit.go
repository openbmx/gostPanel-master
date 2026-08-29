package middleware

import (
	"strconv"
	"sync"
	"time"

	"gost-panel/internal/utils"
	"gost-panel/pkg/logger"
	"gost-panel/pkg/response"

	"github.com/gin-gonic/gin"
)

// 登录防护参数。
//
// 语义为「窗口内累计 N 次失败 → 封禁 M 秒」，而非纯滑动窗口：
// 封禁期是显式的，便于向用户给出准确的 Retry-After，也更容易配置和解释。
//
// 相比最初的实现还有两点改动：
//   - 只统计失败尝试。把成功登录也计入窗口会让正常使用更容易触顶，
//     而攻击者的失败尝试反而与合法用户共用同一份额度。
//   - 阈值由 10 次/分钟收紧到 5 次/5 分钟。
//
// 前提：客户端 IP 必须可信。见 main.go 中的 SetTrustedProxies —— 若信任了
// 任意代理，攻击者只需轮换 X-Forwarded-For 即可让本限流器完全失效。
const (
	loginMaxFailures = 5
	loginWindow      = 5 * time.Minute
	loginBlock       = 5 * time.Minute
)

// observer 上报限流参数。
// GOST 的 http observer 插件按 observer.period（本项目为 5s）上报，
// 每个服务约 12 次/分钟；一个挂了几十条规则的节点可以达到数百次/分钟。
// 这里给一个宽松上限，仅作为上报令牌泄露后的兜底。
const (
	observerMaxPerMinute = 600
	observerWindow       = time.Minute
	observerBlock        = time.Minute
)

// maxTrackedClients 同时跟踪的客户端上限。
//
// 限流中间件在业务鉴权之前执行，未通过鉴权的请求同样会建立条目。
// 没有上限时，一次分布式请求就能让这张表持续增长 —— 限流器本身反而
// 成了内存放大器。
const maxTrackedClients = 5000

// limiterEntry 单个客户端的失败计数与封禁状态
type limiterEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

// slidingWindowLimiter 基于客户端分桶键的失败计数 + 封禁限流器
type slidingWindowLimiter struct {
	mu        sync.Mutex
	entries   map[string]*limiterEntry
	window    time.Duration
	block     time.Duration
	threshold int
	lastSweep time.Time

	// overflow 是跟踪表满之后的共享兜底桶。
	//
	// 早期实现在超限时整表重置，这留下一个可利用点：攻击者只要制造出
	// 足够多的来源把表撑爆，就能顺带把自己已累计的失败计数清零。
	// 改为溢出到一个全局桶后，超出容量的请求仍然被统一计数与封禁，
	// 计数不会因为表满而丢失。
	overflow limiterEntry

	// alertOnBurst 为 true 时，累计失败达到阈值会打印疑似分布式爆破的告警。
	// 只对登录接口启用 —— 观察器上报是正常高频流量，共用会把日志刷满误报。
	alertOnBurst bool
	globalHits   []time.Time
}

func newSlidingWindowLimiter(threshold int, window, block time.Duration, alertOnBurst bool) *slidingWindowLimiter {
	return &slidingWindowLimiter{
		entries:      make(map[string]*limiterEntry),
		window:       window,
		block:        block,
		threshold:    threshold,
		alertOnBurst: alertOnBurst,
	}
}

// blockedFor 返回该客户端还需等待多久；返回 0 表示当前允许请求。
func (l *slidingWindowLimiter) blockedFor(key string) time.Duration {
	if key == "" {
		return 0
	}

	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	if entry, ok := l.entries[key]; ok {
		if remain := entry.blockedUntil.Sub(now); remain > 0 {
			return remain
		}
		return 0
	}

	// 该键尚未被跟踪：若表已满，则受共享溢出桶约束
	if len(l.entries) >= maxTrackedClients {
		if remain := l.overflow.blockedUntil.Sub(now); remain > 0 {
			return remain
		}
	}
	return 0
}

// record 记录一次失败（或一次计入配额的请求）
func (l *slidingWindowLimiter) record(key string) {
	if key == "" {
		return
	}

	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.entries[key]
	if !ok {
		if len(l.entries) >= maxTrackedClients {
			// 表已满：清一次过期条目，仍然满则计入共享溢出桶
			l.sweepExpiredLocked(now)
		}
		if len(l.entries) >= maxTrackedClients {
			l.bumpLocked(&l.overflow, now)
			l.noteBurstLocked(now)
			return
		}
		entry = &limiterEntry{windowStart: now}
		l.entries[key] = entry
	}

	l.bumpLocked(entry, now)
	l.noteBurstLocked(now)
}

// bumpLocked 推进一个计数条目（调用方需持锁）
func (l *slidingWindowLimiter) bumpLocked(entry *limiterEntry, now time.Time) {
	// 封禁期内不再累加，避免持续请求无限延长封禁
	if entry.blockedUntil.After(now) {
		return
	}
	// 窗口已滑出（或时钟回拨）则重新开窗
	if entry.windowStart.After(now) || !now.Before(entry.windowStart.Add(l.window)) {
		entry.windowStart = now
		entry.failures = 0
	}

	entry.failures++
	if entry.failures >= l.threshold {
		entry.failures = 0
		entry.blockedUntil = now.Add(l.block)
		entry.windowStart = entry.blockedUntil
	}
}

// noteBurstLocked 观测全局失败速率，只告警不拦截 ——
// 全局阈值一旦拦截，攻击者就能用它把合法管理员一起锁在门外。
func (l *slidingWindowLimiter) noteBurstLocked(now time.Time) {
	if !l.alertOnBurst {
		return
	}

	cutoff := now.Add(-l.window)
	kept := l.globalHits[:0]
	for _, t := range l.globalHits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.globalHits = append(kept, now)

	if n := len(l.globalHits); n > 0 && n%50 == 0 {
		logger.Warnf("安全告警：最近 %s 内累计 %d 次登录失败，涉及 %d 个来源，疑似分布式爆破",
			l.window, n, len(l.entries))
	}
}

// entryExpiredLocked 判断条目是否可回收（调用方需持锁）
func (l *slidingWindowLimiter) entryExpiredLocked(entry *limiterEntry, now time.Time) bool {
	if entry.blockedUntil.After(now) {
		return false
	}
	return !now.Before(entry.windowStart.Add(l.window))
}

// sweepExpiredLocked 立即回收所有过期条目（调用方需持锁）
func (l *slidingWindowLimiter) sweepExpiredLocked(now time.Time) {
	for k, entry := range l.entries {
		if l.entryExpiredLocked(entry, now) {
			delete(l.entries, k)
		}
	}
	l.lastSweep = now
}

// sweepLocked 周期性回收，避免长期运行导致 map 无限膨胀（调用方需持锁）
func (l *slidingWindowLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) <= l.window {
		return
	}
	l.sweepExpiredLocked(now)
}

// clientBucket 返回限流使用的客户端分桶键。
// IPv6 收敛到 /64，避免攻击者在自己的前缀内轮换地址绕过限流。
func clientBucket(c *gin.Context) string {
	return utils.RateLimitBucket(c.ClientIP())
}

func retryAfterSeconds(d time.Duration) int {
	seconds := int(d / time.Second)
	if d%time.Second > 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// LoginRateLimit 登录接口防爆破中间件。
// 先判断是否处于封禁期，放行后根据响应状态码决定是否计入失败。
func LoginRateLimit() gin.HandlerFunc {
	limiter := newSlidingWindowLimiter(loginMaxFailures, loginWindow, loginBlock, true)

	return func(c *gin.Context) {
		key := clientBucket(c)

		if wait := limiter.blockedFor(key); wait > 0 {
			seconds := retryAfterSeconds(wait)
			c.Header("Retry-After", strconv.Itoa(seconds))
			logger.Warnf("登录尝试过于频繁已被限流: client=%s 剩余 %ds", key, seconds)
			response.Error(c, 429, 42900, "登录失败次数过多，请 "+strconv.Itoa(seconds)+" 秒后再试")
			c.Abort()
			return
		}

		c.Next()

		// 只有失败才计数。成功登录（2xx）不消耗额度，避免正常使用被误伤。
		if c.Writer.Status() >= 400 {
			limiter.record(key)
		}
	}
}

// ObserverRateLimit 观察器上报接口的限流。
// 该接口由节点高频调用，阈值需要足够宽松；目的只是给一个写入端点兜底，
// 防止上报令牌泄露后被无限制刷。
// 注意 alertOnBurst=false：上报是正常高频流量，不能触发"疑似爆破"告警。
func ObserverRateLimit() gin.HandlerFunc {
	limiter := newSlidingWindowLimiter(observerMaxPerMinute, observerWindow, observerBlock, false)

	return func(c *gin.Context) {
		key := clientBucket(c)

		if wait := limiter.blockedFor(key); wait > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			logger.Warnf("观察器上报超过速率上限已被限流: client=%s", key)
			c.AbortWithStatus(429)
			return
		}

		// 这里所有请求都计数（不区分成败），纯粹做速率上限
		limiter.record(key)
		c.Next()
	}
}
