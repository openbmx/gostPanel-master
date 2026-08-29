package service

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gost-panel/internal/config"
	"gost-panel/internal/errors"
	"gost-panel/pkg/github"
	"gost-panel/pkg/logger"
)

const (
	// binaryName 发布产物中二进制的基础名。
	// 压缩包内的实际文件名带平台后缀，如 gost-panel-linux-amd64。
	binaryName = "gost-panel"

	// checksumAssetName 校验和文件在 release 中的名字
	checksumAssetName = "checksums.txt"

	// maxDownloadSize 单个产物的下载上限。面板二进制约 20-30 MB，
	// 100 MiB 已是极宽松的上限。
	maxDownloadSize = 100 << 20

	// updateCacheTTL 检查更新结果的缓存时长。
	// GitHub 未认证 API 每小时仅 60 次，必须缓存，否则多开几个页面就会触发限流。
	updateCacheTTL = 20 * time.Minute

	// maxRollbackVersions 最多提供多少个可回滚的历史版本
	maxRollbackVersions = 5
	// rollbackFetchPageSize 多取一些，过滤掉预发布/草稿/当前版本后仍够用
	rollbackFetchPageSize = 20
)

// releaseClient 抽象出 GitHub 客户端，便于测试注入假实现
type releaseClient interface {
	FetchLatestRelease(ctx context.Context, repo string) (*github.Release, error)
	FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*github.Release, error)
	FetchChecksums(ctx context.Context, rawURL string) ([]byte, error)
	DownloadFile(ctx context.Context, rawURL, dest string, maxSize int64) error
	MirrorURL(rawURL string) string
}

// UpdateInfo 检查更新的结果
type UpdateInfo struct {
	CurrentVersion string       `json:"current_version"`
	LatestVersion  string       `json:"latest_version"`
	HasUpdate      bool         `json:"has_update"`
	Release        *ReleaseInfo `json:"release,omitempty"`
	Cached         bool         `json:"cached"`
	Warning        string       `json:"warning,omitempty"`

	// Updatable 当前环境是否支持面板内更新；false 时 Reason 说明原因
	Updatable bool   `json:"updatable"`
	Reason    string `json:"reason,omitempty"`
	// CanRollback 是否存在可回滚的备份文件
	CanRollback bool `json:"can_rollback"`
	// InProgress 是否有更新正在进行
	InProgress bool `json:"in_progress"`
}

// ReleaseInfo 发布详情
type ReleaseInfo struct {
	Version     string `json:"version"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

// RollbackVersion 可回滚的历史版本
type RollbackVersion struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

// UpdateService 面板内在线更新服务
type UpdateService struct {
	client         releaseClient
	repo           string
	enabled        bool
	currentVersion string

	// running 保证同一时刻只有一个更新在执行。
	// 两个并发的二进制替换会互相破坏备份链，必须串行。
	running  sync.Mutex
	inFlight bool
	stateMu  sync.RWMutex

	cacheMu  sync.Mutex
	cached   *UpdateInfo
	cachedAt time.Time

	// resolveExe 定位当前正在运行的二进制。抽成字段是为了让测试能指向
	// 临时文件 —— 否则测试会真的去替换 go test 生成的测试二进制。
	resolveExe func() (string, error)
}

// NewUpdateService 创建更新服务
func NewUpdateService(cfg *config.UpdateConfig, currentVersion string) *UpdateService {
	return &UpdateService{
		client:         github.NewClient(cfg.MirrorPrefix),
		repo:           cfg.Repo,
		enabled:        cfg.Enabled,
		currentVersion: strings.TrimPrefix(strings.TrimSpace(currentVersion), "v"),
		resolveExe:     resolveExecutable,
	}
}

// exePath 返回当前二进制路径，测试可通过 resolveExe 覆写
func (s *UpdateService) exePath() (string, error) {
	if s.resolveExe != nil {
		return s.resolveExe()
	}
	return resolveExecutable()
}

// CurrentVersion 返回当前运行的版本号
func (s *UpdateService) CurrentVersion() string { return s.currentVersion }

// ---------------------------------------------------------------------------
// 环境前置检查
// ---------------------------------------------------------------------------

// checkUpdatable 判断当前环境是否支持面板内更新。
// 返回的原因会直接展示给管理员，因此要给出可执行的下一步。
func (s *UpdateService) checkUpdatable() (bool, string) {
	if !s.enabled {
		return false, "在线更新已在配置中关闭（update.enabled=false）"
	}

	// Docker 镜像不可变：即便替换了容器内的二进制，重建容器就会回退。
	// 正确做法是拉新镜像。
	if isRunningInDocker() {
		return false, "检测到 Docker 环境。容器内更新会在重建容器后失效，请使用：docker compose pull && docker compose up -d"
	}

	// 本地 make build 得到的版本是 dev，无法与发布版本比较，
	// 也说明这个二进制不是从 release 来的。
	if !isSemver(s.currentVersion) {
		return false, fmt.Sprintf("当前版本为 %q，不是正式发布构建，无法判断更新目标。请通过 Releases 或安装脚本部署后再使用在线更新", s.currentVersion)
	}

	exePath, err := s.exePath()
	if err != nil {
		return false, "无法定位当前程序路径：" + err.Error()
	}
	// 替换二进制需要写入其所在目录（rename 操作作用于目录项）
	if err := checkDirWritable(filepath.Dir(exePath)); err != nil {
		return false, fmt.Sprintf("程序所在目录 %s 不可写，无法自更新。请执行升级脚本，或按新版布局重装（二进制应位于 /opt/gost-panel）", filepath.Dir(exePath))
	}

	return true, ""
}

func isRunningInDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	// 容器运行时也可能没有 /.dockerenv，退而查 cgroup
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	content := string(data)
	return strings.Contains(content, "docker") || strings.Contains(content, "containerd") || strings.Contains(content, "kubepods")
}

// checkDirWritable 通过实际创建临时文件判断目录可写。
// 只看权限位在 root、ACL、只读挂载等情况下都会误判。
func checkDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".gost-panel-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func resolveExecutable() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	// 解开符号链接，确保替换的是真实文件而不是链接本身
	resolved, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// ---------------------------------------------------------------------------
// 检查更新
// ---------------------------------------------------------------------------

// CheckUpdate 检查是否有可用更新。force 为 true 时跳过缓存。
func (s *UpdateService) CheckUpdate(ctx context.Context, force bool) (*UpdateInfo, error) {
	updatable, reason := s.checkUpdatable()

	decorate := func(info *UpdateInfo) *UpdateInfo {
		info.Updatable = updatable
		info.Reason = reason
		info.CanRollback = s.hasBackup()
		info.InProgress = s.isInFlight()
		return info
	}

	if !force {
		if cached := s.readCache(); cached != nil {
			return decorate(cached), nil
		}
	}

	release, err := s.client.FetchLatestRelease(ctx, s.repo)
	if err != nil {
		logger.Warnf("检查更新失败: %v", err)
		// GitHub 不可达时回落到缓存，附带警告而不是直接报错
		if cached := s.readCache(); cached != nil {
			cached.Warning = "使用缓存数据：" + err.Error()
			return decorate(cached), nil
		}
		return decorate(&UpdateInfo{
			CurrentVersion: s.currentVersion,
			LatestVersion:  s.currentVersion,
			HasUpdate:      false,
			Warning:        err.Error(),
		}), nil
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	info := &UpdateInfo{
		CurrentVersion: s.currentVersion,
		LatestVersion:  latest,
		HasUpdate:      isSemver(s.currentVersion) && compareVersions(s.currentVersion, latest) < 0,
		Release: &ReleaseInfo{
			Version:     latest,
			Name:        release.Name,
			Body:        release.Body,
			PublishedAt: release.PublishedAt,
			HTMLURL:     release.HTMLURL,
		},
	}

	s.writeCache(info)
	return decorate(info), nil
}

func (s *UpdateService) readCache() *UpdateInfo {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	if s.cached == nil || time.Since(s.cachedAt) > updateCacheTTL {
		return nil
	}
	clone := *s.cached
	clone.Cached = true
	return &clone
}

func (s *UpdateService) writeCache(info *UpdateInfo) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	clone := *info
	s.cached = &clone
	s.cachedAt = time.Now()
}

func (s *UpdateService) isInFlight() bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.inFlight
}

func (s *UpdateService) setInFlight(v bool) {
	s.stateMu.Lock()
	s.inFlight = v
	s.stateMu.Unlock()
}

// ---------------------------------------------------------------------------
// 执行更新
// ---------------------------------------------------------------------------

// PerformUpdate 升级到最新版本
func (s *UpdateService) PerformUpdate(ctx context.Context) (string, error) {
	if updatable, reason := s.checkUpdatable(); !updatable {
		return "", errors.WithDetail(errors.ErrUpdateNotSupported, reason)
	}

	if !s.running.TryLock() {
		return "", errors.ErrUpdateInProgress
	}
	defer s.running.Unlock()
	s.setInFlight(true)
	defer s.setInFlight(false)

	release, err := s.client.FetchLatestRelease(ctx, s.repo)
	if err != nil {
		return "", errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}

	target := strings.TrimPrefix(release.TagName, "v")
	if compareVersions(s.currentVersion, target) >= 0 {
		return "", errors.ErrNoUpdateAvailable
	}

	if err := s.applyRelease(ctx, release); err != nil {
		return "", err
	}

	logger.Warnf("面板已更新: %s -> %s，等待重启生效", s.currentVersion, target)
	s.invalidateCache()
	return target, nil
}

// ListRollbackVersions 列出可回滚的历史版本（严格早于当前版本，最新在前）
func (s *UpdateService) ListRollbackVersions(ctx context.Context) ([]RollbackVersion, error) {
	candidates, err := s.fetchRollbackCandidates(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]RollbackVersion, 0, len(candidates))
	for _, r := range candidates {
		out = append(out, RollbackVersion{
			Version:     strings.TrimPrefix(r.TagName, "v"),
			PublishedAt: r.PublishedAt,
			HTMLURL:     r.HTMLURL,
		})
	}
	return out, nil
}

// RollbackToVersion 回滚到指定的历史版本。
// 安全：目标版本必须出现在 ListRollbackVersions 的结果中，
// 否则任意版本字符串都会变成一个可被引导的下载目标。
func (s *UpdateService) RollbackToVersion(ctx context.Context, version string) error {
	if updatable, reason := s.checkUpdatable(); !updatable {
		return errors.WithDetail(errors.ErrUpdateNotSupported, reason)
	}

	target := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if target == "" {
		return errors.ErrRollbackVersionNotAllowed
	}

	if !s.running.TryLock() {
		return errors.ErrUpdateInProgress
	}
	defer s.running.Unlock()
	s.setInFlight(true)
	defer s.setInFlight(false)

	candidates, err := s.fetchRollbackCandidates(ctx)
	if err != nil {
		return err
	}

	var match *github.Release
	for _, r := range candidates {
		if strings.TrimPrefix(r.TagName, "v") == target {
			match = r
			break
		}
	}
	if match == nil {
		return errors.ErrRollbackVersionNotAllowed
	}

	if err := s.applyRelease(ctx, match); err != nil {
		return err
	}

	logger.Warnf("面板已回滚: %s -> %s，等待重启生效", s.currentVersion, target)
	s.invalidateCache()
	return nil
}

// RollbackToBackup 恢复上一次更新前保留的二进制备份
func (s *UpdateService) RollbackToBackup() error {
	exePath, err := s.exePath()
	if err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}

	backupPath := exePath + ".backup"
	if _, err := os.Stat(backupPath); err != nil {
		return errors.ErrNoBackupAvailable
	}

	if !s.running.TryLock() {
		return errors.ErrUpdateInProgress
	}
	defer s.running.Unlock()

	// 把当前二进制换到一边，失败时还能换回来
	staged := exePath + ".rollback-tmp"
	_ = os.Remove(staged)
	if err := os.Rename(exePath, staged); err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, "移出当前二进制失败: "+err.Error())
	}
	if err := os.Rename(backupPath, exePath); err != nil {
		if restoreErr := os.Rename(staged, exePath); restoreErr != nil {
			return errors.WithDetail(errors.ErrUpdateFailed,
				fmt.Sprintf("回滚失败且未能恢复原二进制: %v（恢复错误: %v）", err, restoreErr))
		}
		return errors.WithDetail(errors.ErrUpdateFailed, "回滚失败，已恢复原二进制: "+err.Error())
	}

	// 原二进制成为新的备份，允许再次来回切换
	_ = os.Rename(staged, backupPath)

	logger.Warnf("面板已回滚到备份版本，等待重启生效")
	s.invalidateCache()
	return nil
}

func (s *UpdateService) invalidateCache() {
	s.cacheMu.Lock()
	s.cached = nil
	s.cacheMu.Unlock()
}

func (s *UpdateService) hasBackup() bool {
	exePath, err := s.exePath()
	if err != nil {
		return false
	}
	_, err = os.Stat(exePath + ".backup")
	return err == nil
}

func (s *UpdateService) fetchRollbackCandidates(ctx context.Context) ([]*github.Release, error) {
	releases, err := s.client.FetchRecentReleases(ctx, s.repo, rollbackFetchPageSize)
	if err != nil {
		return nil, errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}

	seen := make(map[string]bool, len(releases))
	candidates := make([]*github.Release, 0, maxRollbackVersions)
	for _, r := range releases {
		if r == nil || r.Draft || r.Prerelease {
			continue
		}
		v := strings.TrimPrefix(r.TagName, "v")
		if v == "" || seen[v] || !isSemver(v) {
			continue
		}
		// 只保留严格早于当前版本的（同时排除了当前版本自身）
		if compareVersions(v, s.currentVersion) >= 0 {
			continue
		}
		seen[v] = true
		candidates = append(candidates, r)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return compareVersions(
			strings.TrimPrefix(candidates[i].TagName, "v"),
			strings.TrimPrefix(candidates[j].TagName, "v"),
		) > 0
	})

	if len(candidates) > maxRollbackVersions {
		candidates = candidates[:maxRollbackVersions]
	}
	return candidates, nil
}

// ---------------------------------------------------------------------------
// 下载、校验、替换
// ---------------------------------------------------------------------------

// applyRelease 下载指定 release 的当前平台产物，校验后原子替换正在运行的二进制。
func (s *UpdateService) applyRelease(ctx context.Context, release *github.Release) error {
	archiveAsset, checksumAsset := selectAssets(release.Assets)
	if archiveAsset == nil {
		return errors.WithDetail(errors.ErrUpdateFailed,
			fmt.Sprintf("该版本没有适配 %s/%s 的发布产物", runtime.GOOS, runtime.GOARCH))
	}
	// 安全：没有校验和就没有完整性保证，宁可拒绝更新也不执行来路不明的二进制。
	// sub2api 在这种情况下会跳过校验，这里刻意不这么做。
	if checksumAsset == nil {
		return errors.WithDetail(errors.ErrChecksumMissing,
			fmt.Sprintf("版本 %s 的发布中缺少 %s，无法验证完整性", release.TagName, checksumAssetName))
	}

	exePath, err := s.exePath()
	if err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}
	exeDir := filepath.Dir(exePath)

	// 临时目录必须与二进制同目录：os.Rename 只有在同一文件系统内才是原子的，
	// 跨设备会直接失败。
	tempDir, err := os.MkdirTemp(exeDir, ".gost-panel-update-*")
	if err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, "创建临时目录失败: "+err.Error())
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// 校验和直连 GitHub 获取，不经过加速镜像
	checksums, err := s.client.FetchChecksums(ctx, checksumAsset.BrowserDownloadURL)
	if err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}

	archivePath := filepath.Join(tempDir, archiveAsset.Name)
	downloadURL := s.client.MirrorURL(archiveAsset.BrowserDownloadURL)
	if err := s.client.DownloadFile(ctx, downloadURL, archivePath, maxDownloadSize); err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}

	if err := verifyChecksum(archivePath, archiveAsset.Name, checksums); err != nil {
		return err
	}

	newBinary := filepath.Join(tempDir, binaryName+".new")
	if err := extractBinary(archivePath, newBinary); err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}
	// 二进制必须可执行；0750 让属主（服务账号）可执行、同组可读执行，
	// 不对其他用户开放。
	// #nosec G302 -- 可执行文件必须带执行位，无法使用 0600
	if err := os.Chmod(newBinary, 0o750); err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, "设置执行权限失败: "+err.Error())
	}

	return swapBinary(exePath, newBinary)
}

// swapBinary 用 rename 两步完成原子替换，中途失败可恢复。
func swapBinary(exePath, newBinary string) error {
	backupPath := exePath + ".backup"
	_ = os.Remove(backupPath)

	// 第一步：现有二进制让位为备份
	if err := os.Rename(exePath, backupPath); err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, "备份当前二进制失败: "+err.Error())
	}

	// 第二步：新二进制就位。失败则把备份换回去，保证服务下次仍能启动。
	if err := os.Rename(newBinary, exePath); err != nil {
		if restoreErr := os.Rename(backupPath, exePath); restoreErr != nil {
			return errors.WithDetail(errors.ErrUpdateFailed,
				fmt.Sprintf("替换失败且未能恢复: %v（恢复错误: %v）", err, restoreErr))
		}
		return errors.WithDetail(errors.ErrUpdateFailed, "替换失败，已恢复原二进制: "+err.Error())
	}

	return nil
}

// selectAssets 从 release 产物中挑出当前平台的压缩包与校验和文件
func selectAssets(assets []github.Asset) (archive, checksum *github.Asset) {
	// 发布产物形如 gost-panel-linux-amd64.tar.gz / gost-panel-windows-amd64.zip
	platform := fmt.Sprintf("-%s-%s", runtime.GOOS, runtime.GOARCH)

	for i := range assets {
		a := &assets[i]
		if a.Name == checksumAssetName {
			checksum = a
			continue
		}
		if !strings.Contains(a.Name, platform) {
			continue
		}
		if strings.HasSuffix(a.Name, ".tar.gz") || strings.HasSuffix(a.Name, ".zip") {
			archive = a
		}
	}
	return archive, checksum
}

// verifyChecksum 用 checksums.txt 中的条目校验下载文件。
// 找不到对应条目一律视为失败 —— 缺失校验和不能等同于校验通过。
func verifyChecksum(filePath, assetName string, checksums []byte) error {
	f, err := os.Open(filePath)
	if err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return errors.WithDetail(errors.ErrUpdateFailed, err.Error())
	}
	actual := hex.EncodeToString(h.Sum(nil))

	scanner := bufio.NewScanner(strings.NewReader(string(checksums)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		// sha256sum 的输出中二进制模式会带 "*" 前缀
		name := strings.TrimPrefix(fields[1], "*")
		if name != assetName {
			continue
		}
		if strings.EqualFold(fields[0], actual) {
			return nil
		}
		return errors.WithDetail(errors.ErrChecksumMismatch,
			fmt.Sprintf("%s 校验和不匹配（期望 %s，实际 %s）", assetName, fields[0], actual))
	}

	return errors.WithDetail(errors.ErrChecksumMissing,
		fmt.Sprintf("校验和文件中没有 %s 的条目", assetName))
}

// extractBinary 从压缩包中提取面板二进制。
//
// 安全要点：
//   - 只提取名字以 gost-panel 开头的常规文件，压缩包里同时含有 config/config.yaml，
//     误解包会直接覆盖用户的配置（含 JWT 密钥与管理员设置）
//   - 拒绝任何含 ".." 或绝对路径的条目，防目录穿越
//   - 用 LimitReader 限制解压后体积，防解压炸弹
func extractBinary(archivePath, destPath string) error {
	if strings.HasSuffix(archivePath, ".zip") {
		return extractFromZip(archivePath, destPath)
	}
	return extractFromTarGz(archivePath, destPath)
}

func isPanelBinaryEntry(name string) bool {
	base := path.Base(filepath.ToSlash(name))
	if !strings.HasPrefix(base, binaryName) {
		return false
	}
	// 排除 gost-panel.db、gost-panel.yaml 之类的同前缀文件
	rest := strings.TrimPrefix(base, binaryName)
	return rest == "" || rest == ".exe" || strings.HasPrefix(rest, "-")
}

func hasUnsafePath(name string) bool {
	clean := filepath.ToSlash(name)
	return strings.Contains(clean, "..") || strings.HasPrefix(clean, "/") || filepath.IsAbs(clean)
}

func extractFromTarGz(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取压缩包失败: %w", err)
		}
		if hasUnsafePath(hdr.Name) {
			return fmt.Errorf("压缩包含不安全的路径: %s", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg || !isPanelBinaryEntry(hdr.Name) {
			continue
		}
		if hdr.Size > maxDownloadSize {
			return fmt.Errorf("压缩包内二进制过大: %d 字节", hdr.Size)
		}
		return writeLimited(tr, destPath)
	}
	return fmt.Errorf("压缩包中未找到 %s 可执行文件", binaryName)
}

func extractFromZip(archivePath, destPath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}
	defer func() { _ = zr.Close() }()

	for _, entry := range zr.File {
		if hasUnsafePath(entry.Name) {
			return fmt.Errorf("压缩包含不安全的路径: %s", entry.Name)
		}
		if entry.FileInfo().IsDir() || !isPanelBinaryEntry(entry.Name) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeLimited(rc, destPath)
		_ = rc.Close()
		return err
	}
	return fmt.Errorf("压缩包中未找到 %s 可执行文件", binaryName)
}

// writeLimited 写出内容并限制上限，超限视为解压炸弹
func writeLimited(r io.Reader, destPath string) error {
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	written, copyErr := io.Copy(out, io.LimitReader(r, maxDownloadSize+1))
	closeErr := out.Close()

	if copyErr != nil {
		_ = os.Remove(destPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destPath)
		return closeErr
	}
	if written > maxDownloadSize {
		_ = os.Remove(destPath)
		return fmt.Errorf("解压内容超过上限 %d 字节", maxDownloadSize)
	}
	if written == 0 {
		_ = os.Remove(destPath)
		return fmt.Errorf("解压得到的二进制为空")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 版本号
// ---------------------------------------------------------------------------

// isSemver 判断是否为 x.y.z 形式（允许省略后段，如 1.2）
func isSemver(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return false
	}
	// 去掉预发布/构建元数据后缀
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// compareVersions 比较两个语义化版本。a<b 返回 -1，a>b 返回 1，相等返回 0。
func compareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}

	out := [3]int{}
	parts := strings.Split(v, ".")
	// 先截断再遍历：让下标上界对静态分析也是显然的
	if len(parts) > len(out) {
		parts = parts[:len(out)]
	}
	for i := range parts {
		if n, err := strconv.Atoi(parts[i]); err == nil {
			out[i] = n
		}
	}
	return out
}
