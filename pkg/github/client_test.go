package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateDownloadURL 是本文件的重点：它是防止更新流程被引导到
// 任意主机（SSRF / 投毒）的关键一环。
func TestValidateDownloadURL(t *testing.T) {
	allowed := []string{
		"https://github.com/o/r/releases/download/v1.0.0/app.tar.gz",
		"https://api.github.com/repos/o/r/releases/latest",
		"https://objects.githubusercontent.com/xyz",
		"https://release-assets.githubusercontent.com/xyz",
	}
	for _, u := range allowed {
		if err := ValidateDownloadURL(u, ""); err != nil {
			t.Errorf("%q 应被允许，实际 %v", u, err)
		}
	}

	rejected := map[string]string{
		"http://github.com/o/r/a.tar.gz":        "明文 http 必须拒绝",
		"https://evil.com/a.tar.gz":             "非白名单主机必须拒绝",
		"https://github.com.evil.com/a":         "后缀伪装必须拒绝（github.com.evil.com）",
		"https://notgithub.com/a":               "相似域名必须拒绝",
		"https://evil.com/https://github.com/a": "路径里带白名单域名不算可信",
		"ftp://github.com/a":                    "非 https 协议必须拒绝",
		"https://github.com@evil.com/a":         "URL 用户信息伪装必须拒绝",
		"":                                      "空地址必须拒绝",
	}
	for u, reason := range rejected {
		if err := ValidateDownloadURL(u, ""); err == nil {
			t.Errorf("%s：%q 却被放行了", reason, u)
		}
	}
}

func TestValidateDownloadURL_WithMirror(t *testing.T) {
	const mirror = "https://ghfast.top/"

	// 镜像前缀 + 真实 GitHub 地址：允许
	ok := mirror + "https://github.com/o/r/releases/download/v1/app.tar.gz"
	if err := ValidateDownloadURL(ok, mirror); err != nil {
		t.Errorf("经过镜像的 GitHub 地址应被允许，实际 %v", err)
	}

	// 镜像前缀 + 非 GitHub 地址：仍须拒绝，
	// 否则配置了镜像就等于打开了任意地址下载
	bad := mirror + "https://evil.com/payload.tar.gz"
	if err := ValidateDownloadURL(bad, mirror); err == nil {
		t.Error("镜像后面挂非 GitHub 地址必须拒绝")
	}

	// 未配置镜像时，镜像形态的地址不应被接受
	if err := ValidateDownloadURL(ok, ""); err == nil {
		t.Error("未配置镜像时不应接受镜像形态地址")
	}
}

func TestNormalizeMirrorPrefix(t *testing.T) {
	cases := map[string]string{
		"https://ghfast.top":   "https://ghfast.top/",
		"https://ghfast.top/":  "https://ghfast.top/",
		" https://ghfast.top ": "https://ghfast.top/",
		// 明文镜像会把下载降级成 http，直接忽略
		"http://ghfast.top": "",
		"":                  "",
		"ghfast.top":        "",
	}
	for in, want := range cases {
		if got := normalizeMirrorPrefix(in); got != want {
			t.Errorf("normalizeMirrorPrefix(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestDownloadFile_EnforcesMaxSize 验证即使服务端谎报 Content-Length，
// 实际写入量仍被限制住（防下载炸弹）
func TestDownloadFile_EnforcesMaxSize(t *testing.T) {
	payload := strings.Repeat("A", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 故意不设置 Content-Length，让 LimitReader 成为唯一防线
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	c := NewClient("")
	dest := filepath.Join(t.TempDir(), "out.bin")

	// 绕过主机白名单直接测传输层限制
	err := c.downloadTo(context.Background(), srv.URL, dest, 100)
	if err == nil {
		t.Fatal("超过上限的下载必须失败")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("失败的下载不应留下残留文件")
	}
}

func TestDownloadFile_RejectsUntrustedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	c := NewClient("")
	dest := filepath.Join(t.TempDir(), "out.bin")

	// 走公开入口，必须被主机白名单挡下
	if err := c.DownloadFile(context.Background(), srv.URL, dest, 1<<20); err == nil {
		t.Fatal("非 GitHub 主机的下载必须被拒绝")
	}
}
