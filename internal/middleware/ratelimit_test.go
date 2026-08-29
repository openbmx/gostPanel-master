package middleware

import (
	"fmt"
	"testing"
	"time"
)

// TestSlidingWindowLimiter_BlocksAfterMax 基本阈值行为
func TestSlidingWindowLimiter_BlocksAfterMax(t *testing.T) {
	l := newSlidingWindowLimiter(3, time.Minute, false)

	for i := 0; i < 3; i++ {
		if wait := l.blockedFor("1.1.1.1"); wait > 0 {
			t.Fatalf("第 %d 次尝试不应被阻断", i+1)
		}
		l.record("1.1.1.1")
	}

	if wait := l.blockedFor("1.1.1.1"); wait <= 0 {
		t.Error("超过阈值后应被阻断")
	}
	// 其他 IP 不受影响
	if wait := l.blockedFor("2.2.2.2"); wait > 0 {
		t.Error("阻断不应波及其他 IP")
	}
}

// TestSlidingWindowLimiter_WindowExpiry 窗口滑出后自动解锁
func TestSlidingWindowLimiter_WindowExpiry(t *testing.T) {
	l := newSlidingWindowLimiter(2, 50*time.Millisecond, false)

	l.record("1.1.1.1")
	l.record("1.1.1.1")
	if wait := l.blockedFor("1.1.1.1"); wait <= 0 {
		t.Fatal("应处于阻断状态")
	}

	time.Sleep(80 * time.Millisecond)
	if wait := l.blockedFor("1.1.1.1"); wait > 0 {
		t.Errorf("窗口过期后应解锁，实际仍需等待 %v", wait)
	}
}

// TestSlidingWindowLimiter_CapacityBounded 回归：跟踪表必须有界。
//
// 限流中间件在业务鉴权之前运行，未认证请求同样会建立条目。
// 没有容量上限时，分布式请求会让这张表无限增长。
func TestSlidingWindowLimiter_CapacityBounded(t *testing.T) {
	l := newSlidingWindowLimiter(600, time.Minute, false)

	for i := 0; i < maxTrackedIPs*2; i++ {
		l.record(fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}

	l.mu.Lock()
	size := len(l.hits)
	l.mu.Unlock()

	if size > maxTrackedIPs {
		t.Errorf("跟踪表超出上限: %d > %d", size, maxTrackedIPs)
	}
}

// TestSlidingWindowLimiter_PerIPSliceBounded 单个 IP 的时间戳切片不应超过阈值，
// 否则高频接口（观察器上报）的内存占用会被放大。
func TestSlidingWindowLimiter_PerIPSliceBounded(t *testing.T) {
	const max = 5
	l := newSlidingWindowLimiter(max, time.Minute, false)

	// 模拟中间件行为：被阻断就不再记录
	for i := 0; i < 100; i++ {
		if wait := l.blockedFor("1.1.1.1"); wait > 0 {
			continue
		}
		l.record("1.1.1.1")
	}

	l.mu.Lock()
	n := len(l.hits["1.1.1.1"])
	l.mu.Unlock()

	if n > max {
		t.Errorf("单 IP 时间戳数量 %d 超过阈值 %d", n, max)
	}
}
