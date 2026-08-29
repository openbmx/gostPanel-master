package middleware

import (
	"fmt"
	"testing"
	"time"
)

// TestLimiter_BlocksAfterThreshold 基本阈值与封禁行为
func TestLimiter_BlocksAfterThreshold(t *testing.T) {
	l := newSlidingWindowLimiter(3, time.Minute, time.Minute, false)

	for i := 0; i < 3; i++ {
		if wait := l.blockedFor("1.1.1.1"); wait > 0 {
			t.Fatalf("第 %d 次尝试不应被阻断", i+1)
		}
		l.record("1.1.1.1")
	}

	if wait := l.blockedFor("1.1.1.1"); wait <= 0 {
		t.Error("达到阈值后应进入封禁期")
	}
	if wait := l.blockedFor("2.2.2.2"); wait > 0 {
		t.Error("封禁不应波及其他客户端")
	}
}

// TestLimiter_BlockExpiry 封禁期结束后自动解锁
func TestLimiter_BlockExpiry(t *testing.T) {
	l := newSlidingWindowLimiter(2, time.Minute, 50*time.Millisecond, false)

	l.record("1.1.1.1")
	l.record("1.1.1.1")
	if wait := l.blockedFor("1.1.1.1"); wait <= 0 {
		t.Fatal("应处于封禁状态")
	}

	time.Sleep(80 * time.Millisecond)
	if wait := l.blockedFor("1.1.1.1"); wait > 0 {
		t.Errorf("封禁期结束后应解锁，实际仍需等待 %v", wait)
	}
}

// TestLimiter_BlockNotExtendedByFurtherAttempts 封禁期内继续请求不应无限延长封禁
func TestLimiter_BlockNotExtendedByFurtherAttempts(t *testing.T) {
	l := newSlidingWindowLimiter(2, time.Minute, 200*time.Millisecond, false)

	l.record("1.1.1.1")
	l.record("1.1.1.1")
	first := l.blockedFor("1.1.1.1")

	for i := 0; i < 20; i++ {
		l.record("1.1.1.1")
	}
	after := l.blockedFor("1.1.1.1")

	if after > first {
		t.Errorf("封禁期被继续请求延长了: %v -> %v", first, after)
	}
}

// TestLimiter_CapacityBounded 回归：跟踪表必须有界。
// 限流中间件在业务鉴权之前运行，未认证请求同样会建立条目。
func TestLimiter_CapacityBounded(t *testing.T) {
	l := newSlidingWindowLimiter(600, time.Minute, time.Minute, false)

	for i := 0; i < maxTrackedClients*2; i++ {
		l.record(fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}

	l.mu.Lock()
	size := len(l.entries)
	l.mu.Unlock()

	if size > maxTrackedClients {
		t.Errorf("跟踪表超出上限: %d > %d", size, maxTrackedClients)
	}
}

// TestLimiter_OverflowDoesNotResetCounters 回归：表满时不得丢失已有计数。
//
// 早期实现在超限时整表重置，攻击者只要制造足够多的来源把表撑爆，
// 就能顺带把自己已累计的失败计数清零。现在改为溢出到共享桶。
func TestLimiter_OverflowDoesNotResetCounters(t *testing.T) {
	l := newSlidingWindowLimiter(3, time.Minute, time.Minute, false)

	// 攻击者先累计到封禁
	const attacker = "9.9.9.9"
	for i := 0; i < 3; i++ {
		l.record(attacker)
	}
	if l.blockedFor(attacker) <= 0 {
		t.Fatal("前置条件不成立：攻击者应已被封禁")
	}

	// 用大量新来源把跟踪表撑爆
	for i := 0; i < maxTrackedClients*2; i++ {
		l.record(fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}

	if l.blockedFor(attacker) <= 0 {
		t.Error("撑爆跟踪表后攻击者的封禁状态被清除了")
	}
}

// TestLimiter_OverflowBucketBlocks 表满后的新来源应受共享溢出桶约束，
// 而不是无条件放行。
func TestLimiter_OverflowBucketBlocks(t *testing.T) {
	l := newSlidingWindowLimiter(3, time.Minute, time.Minute, false)

	for i := 0; i < maxTrackedClients+50; i++ {
		l.record(fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff))
	}

	if wait := l.blockedFor("203.0.113.7"); wait <= 0 {
		t.Error("跟踪表满且溢出桶已触发阈值后，新来源应被阻断")
	}
}
