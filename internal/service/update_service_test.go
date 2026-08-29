package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"gost-panel/internal/config"
	"gost-panel/internal/errors"
	"gost-panel/pkg/github"
)

// ---------------------------------------------------------------------------
// 版本号
// ---------------------------------------------------------------------------

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},
		// 字符串比较会把 1.2.10 判成小于 1.2.9，必须按数字比
		{"1.2.9", "1.2.10", -1},
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.2", "1.2.0", 0},
		{"1.2.3-rc1", "1.2.3", 0},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q,%q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestIsSemver(t *testing.T) {
	valid := []string{"1.2.3", "v1.2.3", "0.0.1", "1.2", "1.2.3-rc1", "1.2.3+build5"}
	invalid := []string{"", "dev", "latest", "v", "abc", "1.2.3.4", "1..3"}

	for _, v := range valid {
		if !isSemver(v) {
			t.Errorf("isSemver(%q) 应为 true", v)
		}
	}
	for _, v := range invalid {
		if isSemver(v) {
			t.Errorf("isSemver(%q) 应为 false", v)
		}
	}
}

// ---------------------------------------------------------------------------
// 校验和
// ---------------------------------------------------------------------------

func writeTempFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	return p
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	content := []byte("panel binary payload")
	path := writeTempFile(t, dir, "asset.tar.gz", content)
	good := sha256Hex(content)

	t.Run("匹配", func(t *testing.T) {
		checksums := []byte(good + "  asset.tar.gz\n")
		if err := verifyChecksum(path, "asset.tar.gz", checksums); err != nil {
			t.Errorf("应校验通过，实际 %v", err)
		}
	})

	t.Run("二进制模式星号前缀", func(t *testing.T) {
		checksums := []byte(good + " *asset.tar.gz\n")
		if err := verifyChecksum(path, "asset.tar.gz", checksums); err != nil {
			t.Errorf("应兼容 sha256sum 的 * 前缀，实际 %v", err)
		}
	})

	t.Run("不匹配必须失败", func(t *testing.T) {
		checksums := []byte(strings.Repeat("0", 64) + "  asset.tar.gz\n")
		err := verifyChecksum(path, "asset.tar.gz", checksums)
		if err == nil {
			t.Fatal("校验和不匹配时必须失败")
		}
		var biz *errors.BizError
		if !asBizError(err, &biz) || biz.Code != errors.ErrChecksumMismatch.Code {
			t.Errorf("期望 ErrChecksumMismatch，实际 %v", err)
		}
	})

	t.Run("无对应条目必须失败", func(t *testing.T) {
		// 关键回归：缺失校验和不能等同于校验通过
		checksums := []byte(good + "  other-asset.tar.gz\n")
		err := verifyChecksum(path, "asset.tar.gz", checksums)
		if err == nil {
			t.Fatal("校验和文件中无对应条目时必须失败")
		}
		var biz *errors.BizError
		if !asBizError(err, &biz) || biz.Code != errors.ErrChecksumMissing.Code {
			t.Errorf("期望 ErrChecksumMissing，实际 %v", err)
		}
	})

	t.Run("空校验和文件必须失败", func(t *testing.T) {
		if err := verifyChecksum(path, "asset.tar.gz", nil); err == nil {
			t.Error("空校验和文件必须失败")
		}
	})
}

func asBizError(err error, target **errors.BizError) bool {
	biz, ok := err.(*errors.BizError)
	if ok {
		*target = biz
	}
	return ok
}

// ---------------------------------------------------------------------------
// 解包
// ---------------------------------------------------------------------------

type tarEntry struct {
	name    string
	content string
	typ     byte
}

func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o755,
			Size:     int64(len(e.content)),
			Typeflag: typ,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("写 tar 头失败: %v", err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatalf("写 tar 内容失败: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary_OnlyBinary(t *testing.T) {
	dir := t.TempDir()
	// 真实发布包的结构：二进制 + config/config.yaml
	archive := buildTarGz(t, []tarEntry{
		{name: "config/config.yaml", content: "jwt:\n  secret: USER_SECRET\n"},
		{name: "gost-panel-linux-amd64", content: "BINARY-CONTENT"},
	})
	archivePath := writeTempFile(t, dir, "release.tar.gz", archive)

	dest := filepath.Join(dir, "out")
	if err := extractBinary(archivePath, dest); err != nil {
		t.Fatalf("解包失败: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BINARY-CONTENT" {
		t.Errorf("提取到的内容不对: %q", got)
	}

	// 关键回归：发布包里带着 config/config.yaml，
	// 若被一并解包会直接覆盖用户配置（含 JWT 密钥）
	if _, err := os.Stat(filepath.Join(dir, "config")); err == nil {
		t.Error("config/ 目录被解包了，会覆盖用户配置")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); err == nil {
		t.Error("config.yaml 被解包了，会覆盖用户配置")
	}
}

// TestExtractBinary_GoReleaserLayout 锁定 GoReleaser 实际产出的包内布局。
//
// 与手工打包时代的差异：二进制在包内叫 gost-panel（不带平台后缀），
// 且多了 LICENSE / README.md。发布流程一旦改动打包方式，
// 这条用例会先于线上升级失败暴露出来。
func TestExtractBinary_GoReleaserLayout(t *testing.T) {
	dir := t.TempDir()
	archive := buildTarGz(t, []tarEntry{
		{name: "LICENSE", content: "MIT"},
		{name: "README.md", content: "# Gost Panel"},
		{name: "config/config.yaml", content: "jwt:\n  secret: USER_SECRET\n"},
		{name: "gost-panel", content: "GORELEASER-BINARY"},
	})
	archivePath := writeTempFile(t, dir, "gost-panel-linux-amd64.tar.gz", archive)

	dest := filepath.Join(dir, "out")
	if err := extractBinary(archivePath, dest); err != nil {
		t.Fatalf("解包 GoReleaser 产物失败: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "GORELEASER-BINARY" {
		t.Errorf("提取到的内容不对: %q", got)
	}
	// 同样不能碰用户配置
	if _, err := os.Stat(filepath.Join(dir, "config")); err == nil {
		t.Error("config/ 目录被解包了，会覆盖用户配置")
	}
}

// TestSelectAssets_MatchesReleaseNaming 锁定发布产物的命名约定。
//
// GoReleaser 的默认模板是 name_version_os_arch（下划线），本项目刻意保留了
// 历史的 name-os-arch 形式：改名会同时打断安装/升级脚本拼接的下载地址
// 和这里的资产匹配，而旧脚本仍在用户机器上运行。
func TestSelectAssets_MatchesReleaseNaming(t *testing.T) {
	assetName := fmt.Sprintf("gost-panel-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		assetName = fmt.Sprintf("gost-panel-%s-%s.zip", runtime.GOOS, runtime.GOARCH)
	}

	assets := []github.Asset{
		{Name: "gost-panel-linux-amd64.tar.gz"},
		{Name: "gost-panel-linux-arm64.tar.gz"},
		{Name: "gost-panel-darwin-amd64.tar.gz"},
		{Name: "gost-panel-darwin-arm64.tar.gz"},
		{Name: "gost-panel-windows-amd64.zip"},
		{Name: "checksums.txt"},
	}

	archive, checksum := selectAssets(assets)
	if archive == nil {
		t.Fatalf("未能为 %s/%s 匹配到发布产物", runtime.GOOS, runtime.GOARCH)
	}
	if archive.Name != assetName {
		t.Errorf("匹配到 %q，期望 %q", archive.Name, assetName)
	}
	if checksum == nil || checksum.Name != "checksums.txt" {
		t.Error("未能匹配到 checksums.txt")
	}
}

func TestExtractBinary_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := buildTarGz(t, []tarEntry{
		{name: "../../etc/gost-panel", content: "EVIL"},
	})
	archivePath := writeTempFile(t, dir, "evil.tar.gz", archive)

	err := extractBinary(archivePath, filepath.Join(dir, "out"))
	if err == nil {
		t.Fatal("含 .. 的条目必须被拒绝")
	}
	if !strings.Contains(err.Error(), "不安全的路径") {
		t.Errorf("错误信息应指明路径不安全，实际: %v", err)
	}
}

func TestExtractBinary_RejectsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	archive := buildTarGz(t, []tarEntry{
		{name: "/usr/local/bin/gost-panel", content: "EVIL"},
	})
	archivePath := writeTempFile(t, dir, "evil.tar.gz", archive)

	if err := extractBinary(archivePath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("绝对路径条目必须被拒绝")
	}
}

func TestExtractBinary_NoBinaryFound(t *testing.T) {
	dir := t.TempDir()
	archive := buildTarGz(t, []tarEntry{
		{name: "config/config.yaml", content: "x"},
		{name: "README.md", content: "y"},
	})
	archivePath := writeTempFile(t, dir, "nobin.tar.gz", archive)

	if err := extractBinary(archivePath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("压缩包内无二进制时必须报错")
	}
}

func TestIsPanelBinaryEntry(t *testing.T) {
	yes := []string{"gost-panel", "gost-panel-linux-amd64", "gost-panel.exe", "gost-panel-windows-amd64.exe", "./gost-panel-linux-arm64"}
	no := []string{"config/config.yaml", "gost-panel.db", "gost-panel.yaml", "README.md", "gostpanel"}

	for _, n := range yes {
		if !isPanelBinaryEntry(n) {
			t.Errorf("%q 应被识别为面板二进制", n)
		}
	}
	for _, n := range no {
		if isPanelBinaryEntry(n) {
			t.Errorf("%q 不应被识别为面板二进制", n)
		}
	}
}

// ---------------------------------------------------------------------------
// 二进制替换
// ---------------------------------------------------------------------------

func TestSwapBinary(t *testing.T) {
	dir := t.TempDir()
	exePath := writeTempFile(t, dir, "gost-panel", []byte("OLD"))
	newPath := writeTempFile(t, dir, "gost-panel.new", []byte("NEW"))

	if err := swapBinary(exePath, newPath); err != nil {
		t.Fatalf("替换失败: %v", err)
	}

	cur, _ := os.ReadFile(exePath)
	if string(cur) != "NEW" {
		t.Errorf("二进制未被替换: %q", cur)
	}
	backup, err := os.ReadFile(exePath + ".backup")
	if err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	if string(backup) != "OLD" {
		t.Errorf("备份内容不对: %q", backup)
	}
}

// ---------------------------------------------------------------------------
// 环境前置检查
// ---------------------------------------------------------------------------

func TestCheckUpdatable_RejectsNonReleaseBuild(t *testing.T) {
	svc := NewUpdateService(&config.UpdateConfig{Enabled: true, Repo: "o/r"}, "dev")
	ok, reason := svc.checkUpdatable()
	if ok {
		t.Fatal("dev 构建不应允许在线更新")
	}
	if !strings.Contains(reason, "正式发布构建") {
		t.Errorf("原因应说明非发布构建，实际: %s", reason)
	}
}

func TestCheckUpdatable_RespectsDisabled(t *testing.T) {
	svc := NewUpdateService(&config.UpdateConfig{Enabled: false, Repo: "o/r"}, "1.0.0")
	ok, reason := svc.checkUpdatable()
	if ok {
		t.Fatal("配置关闭时不应允许在线更新")
	}
	if !strings.Contains(reason, "关闭") {
		t.Errorf("原因应说明已关闭，实际: %s", reason)
	}
}

func TestPerformUpdate_RejectedWhenNotUpdatable(t *testing.T) {
	svc := NewUpdateService(&config.UpdateConfig{Enabled: true, Repo: "o/r"}, "dev")
	if _, err := svc.PerformUpdate(context.Background()); err == nil {
		t.Fatal("不满足前置条件时必须拒绝更新")
	}
}

// ---------------------------------------------------------------------------
// 端到端：假 GitHub
// ---------------------------------------------------------------------------

// fakeClient 模拟 GitHub 客户端，返回构造好的 release 与产物
type fakeClient struct {
	latest    *github.Release
	recent    []*github.Release
	archive   []byte
	checksums []byte

	mu            sync.Mutex
	downloadCalls int
}

func (f *fakeClient) FetchLatestRelease(context.Context, string) (*github.Release, error) {
	return f.latest, nil
}

func (f *fakeClient) FetchRecentReleases(context.Context, string, int) ([]*github.Release, error) {
	return f.recent, nil
}

func (f *fakeClient) FetchChecksums(context.Context, string) ([]byte, error) {
	return f.checksums, nil
}

func (f *fakeClient) DownloadFile(_ context.Context, _, dest string, _ int64) error {
	f.mu.Lock()
	f.downloadCalls++
	f.mu.Unlock()
	return os.WriteFile(dest, f.archive, 0o600)
}

func (f *fakeClient) MirrorURL(rawURL string) string { return rawURL }

// newE2EService 构造一个把"当前二进制"指向临时文件的服务实例
func newE2EService(t *testing.T, currentVersion string, archiveContent string, corruptChecksum bool) (*UpdateService, string, *fakeClient) {
	t.Helper()

	dir := t.TempDir()
	exePath := writeTempFile(t, dir, "gost-panel", []byte("OLD-BINARY"))

	assetName := fmt.Sprintf("gost-panel-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := buildTarGz(t, []tarEntry{
		{name: "config/config.yaml", content: "user config"},
		{name: fmt.Sprintf("gost-panel-%s-%s", runtime.GOOS, runtime.GOARCH), content: archiveContent},
	})

	sum := sha256Hex(archive)
	if corruptChecksum {
		sum = strings.Repeat("a", 64)
	}
	checksums := []byte(sum + "  " + assetName + "\n")

	client := &fakeClient{
		latest: &github.Release{
			TagName:     "v9.9.9",
			Name:        "v9.9.9",
			Body:        "changelog",
			PublishedAt: "2026-01-01T00:00:00Z",
			Assets: []github.Asset{
				{Name: assetName, BrowserDownloadURL: "https://github.com/o/r/releases/download/v9.9.9/" + assetName},
				{Name: "checksums.txt", BrowserDownloadURL: "https://github.com/o/r/releases/download/v9.9.9/checksums.txt"},
			},
		},
		archive:   archive,
		checksums: checksums,
	}

	svc := &UpdateService{
		client:         client,
		repo:           "o/r",
		enabled:        true,
		currentVersion: currentVersion,
		// 测试里把可执行文件定位覆写到临时文件，避免动到真正的测试二进制
		resolveExe: func() (string, error) { return exePath, nil },
	}
	return svc, exePath, client
}

func TestPerformUpdate_EndToEnd(t *testing.T) {
	svc, exePath, _ := newE2EService(t, "1.0.0", "NEW-BINARY", false)

	newVersion, err := svc.PerformUpdate(context.Background())
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if newVersion != "9.9.9" {
		t.Errorf("期望更新到 9.9.9，实际 %s", newVersion)
	}

	cur, _ := os.ReadFile(exePath)
	if string(cur) != "NEW-BINARY" {
		t.Errorf("二进制未被替换: %q", cur)
	}
	backup, err := os.ReadFile(exePath + ".backup")
	if err != nil || string(backup) != "OLD-BINARY" {
		t.Errorf("备份不正确: %q (%v)", backup, err)
	}
}

func TestPerformUpdate_ChecksumMismatchLeavesBinaryUntouched(t *testing.T) {
	// 最关键的一条：校验失败时绝不能动到正在运行的二进制
	svc, exePath, _ := newE2EService(t, "1.0.0", "EVIL-BINARY", true)

	if _, err := svc.PerformUpdate(context.Background()); err == nil {
		t.Fatal("校验和不匹配时必须失败")
	}

	cur, _ := os.ReadFile(exePath)
	if string(cur) != "OLD-BINARY" {
		t.Errorf("校验失败后二进制被改动了: %q", cur)
	}
	if _, err := os.Stat(exePath + ".backup"); err == nil {
		t.Error("校验失败时不应产生备份文件")
	}
}

func TestPerformUpdate_AlreadyLatest(t *testing.T) {
	svc, exePath, _ := newE2EService(t, "9.9.9", "NEW-BINARY", false)

	_, err := svc.PerformUpdate(context.Background())
	if err != errors.ErrNoUpdateAvailable {
		t.Fatalf("期望 ErrNoUpdateAvailable，实际 %v", err)
	}
	cur, _ := os.ReadFile(exePath)
	if string(cur) != "OLD-BINARY" {
		t.Error("已是最新版时不应改动二进制")
	}
}

func TestRollbackToBackup(t *testing.T) {
	svc, exePath, _ := newE2EService(t, "1.0.0", "NEW-BINARY", false)

	if _, err := svc.PerformUpdate(context.Background()); err != nil {
		t.Fatalf("前置更新失败: %v", err)
	}
	if err := svc.RollbackToBackup(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}

	cur, _ := os.ReadFile(exePath)
	if string(cur) != "OLD-BINARY" {
		t.Errorf("回滚后应恢复旧二进制，实际 %q", cur)
	}
	// 新二进制成为备份，允许再切回去
	backup, _ := os.ReadFile(exePath + ".backup")
	if string(backup) != "NEW-BINARY" {
		t.Errorf("回滚后备份应为被替换掉的版本，实际 %q", backup)
	}
}

func TestRollbackToBackup_NoBackup(t *testing.T) {
	svc, _, _ := newE2EService(t, "1.0.0", "NEW-BINARY", false)
	if err := svc.RollbackToBackup(); err != errors.ErrNoBackupAvailable {
		t.Fatalf("期望 ErrNoBackupAvailable，实际 %v", err)
	}
}

func TestRollbackToVersion_RejectsUnlistedVersion(t *testing.T) {
	svc, exePath, _ := newE2EService(t, "5.0.0", "NEW-BINARY", false)
	// recent 为空 => 没有任何可回滚版本
	err := svc.RollbackToVersion(context.Background(), "1.2.3")
	if err != errors.ErrRollbackVersionNotAllowed {
		t.Fatalf("期望 ErrRollbackVersionNotAllowed，实际 %v", err)
	}
	cur, _ := os.ReadFile(exePath)
	if string(cur) != "OLD-BINARY" {
		t.Error("拒绝的回滚不应改动二进制")
	}
}

func TestPerformUpdate_ConcurrentIsRejected(t *testing.T) {
	svc, _, _ := newE2EService(t, "1.0.0", "NEW-BINARY", false)

	// 占住锁，模拟一个正在进行的更新
	svc.running.Lock()
	defer svc.running.Unlock()

	if _, err := svc.PerformUpdate(context.Background()); err != errors.ErrUpdateInProgress {
		t.Fatalf("并发更新应返回 ErrUpdateInProgress，实际 %v", err)
	}
}

func TestCheckUpdate_UsesCache(t *testing.T) {
	svc, _, client := newE2EService(t, "1.0.0", "NEW-BINARY", false)

	first, err := svc.CheckUpdate(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached {
		t.Error("首次检查不应来自缓存")
	}
	if !first.HasUpdate {
		t.Error("1.0.0 -> 9.9.9 应判定为有更新")
	}

	second, err := svc.CheckUpdate(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached {
		t.Error("第二次检查应命中缓存（GitHub 未认证 API 每小时仅 60 次）")
	}
	_ = client
}
