package utils

import "testing"

func TestNormalizeIP(t *testing.T) {
	cases := map[string]string{
		"  1.2.3.4  ":        "1.2.3.4",
		"1.2.3.4:8080":       "1.2.3.4",
		"[2001:db8::1]:8080": "2001:db8::1",
		"[2001:db8::1]":      "2001:db8::1",
		"2001:DB8::1":        "2001:db8::1",
		"":                   "",
		"not-an-ip":          "not-an-ip",
	}

	for in, want := range cases {
		if got := NormalizeIP(in); got != want {
			t.Errorf("NormalizeIP(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestRateLimitBucket_IPv6CollapsesTo64 是本文件的重点。
//
// 运营商通常给单个用户分配整个 /64（甚至更大），攻击者可以在自己的前缀内
// 随意更换源地址。若按完整 IPv6 地址分桶，每次请求都落进新桶，按 IP 的
// 限流将完全失效。
func TestRateLimitBucket_IPv6CollapsesTo64(t *testing.T) {
	sameSubnet := []string{
		"2001:db8:1234:5678::1",
		"2001:db8:1234:5678::dead:beef",
		"2001:db8:1234:5678:aaaa:bbbb:cccc:dddd",
		"[2001:db8:1234:5678::99]:443",
	}

	first := RateLimitBucket(sameSubnet[0])
	if first == "" {
		t.Fatal("分桶键不应为空")
	}
	for _, addr := range sameSubnet[1:] {
		if got := RateLimitBucket(addr); got != first {
			t.Errorf("同一 /64 内的 %s 应落入同一桶 %q，实际 %q", addr, first, got)
		}
	}

	// 不同 /64 必须分开
	other := RateLimitBucket("2001:db8:1234:9999::1")
	if other == first {
		t.Errorf("不同 /64 不应共用同一个桶: %q", other)
	}
}

func TestRateLimitBucket_IPv4UsesFullAddress(t *testing.T) {
	if got := RateLimitBucket("1.2.3.4:9999"); got != "1.2.3.4" {
		t.Errorf("IPv4 应使用完整地址，实际 %q", got)
	}
	if RateLimitBucket("1.2.3.4") == RateLimitBucket("1.2.3.5") {
		t.Error("不同 IPv4 地址不应共用同一个桶")
	}
}

func TestRateLimitBucket_EdgeCases(t *testing.T) {
	if got := RateLimitBucket(""); got != "" {
		t.Errorf("空输入应返回空，实际 %q", got)
	}
	// 无法解析时原样作为键，至少不会把不同来源混进同一个桶
	if RateLimitBucket("garbage-a") == RateLimitBucket("garbage-b") {
		t.Error("不可解析的不同输入不应共用同一个桶")
	}
	// IPv4-mapped IPv6 应按 IPv4 处理
	if got := RateLimitBucket("::ffff:1.2.3.4"); got != "1.2.3.4" {
		t.Errorf("IPv4-mapped 地址应归一到 IPv4，实际 %q", got)
	}
}
