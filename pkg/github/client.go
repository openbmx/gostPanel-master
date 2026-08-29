// Package github 提供访问 GitHub Releases 的最小客户端，供面板内在线更新使用。
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// apiHost GitHub API 主机。校验和与 release 元数据只从这里获取。
	apiHost = "api.github.com"

	// 允许下载发布产物的主机。
	// GitHub 的 releases/download 链接会 302 到 githubusercontent.com 下的
	// 资产主机（历史上是 objects.*，近期是 release-assets.*），
	// 因此按域后缀放行整个 githubusercontent.com。
	releaseHost = "github.com"
	assetHost   = "githubusercontent.com"

	apiTimeout      = 30 * time.Second
	downloadTimeout = 10 * time.Minute

	// maxChecksumBytes 校验和文件的大小上限。正常只有几百字节，
	// 这里给一个宽松但有限的上限，避免被超大响应拖垮。
	maxChecksumBytes = 1 << 20 // 1 MiB
)

// Release 表示一个 GitHub Release
type Release struct {
	TagName     string  `json:"tag_name"`
	Name        string  `json:"name"`
	Body        string  `json:"body"`
	PublishedAt string  `json:"published_at"`
	HTMLURL     string  `json:"html_url"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	Assets      []Asset `json:"assets"`
}

// Asset 表示 Release 中的一个产物
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Client GitHub Releases 客户端
type Client struct {
	apiClient      *http.Client
	downloadClient *http.Client

	// mirrorPrefix 可选的下载加速前缀（形如 https://ghfast.top/）。
	//
	// 安全：镜像只用于下载体积较大的二进制包，校验和文件始终直连 GitHub 获取。
	// 若两者都走同一个镜像，镜像方可以同时替换二进制与其校验和，
	// 校验就完全失去意义 —— 这是很多"支持加速"的更新器的实际漏洞。
	mirrorPrefix string
}

// NewClient 创建客户端。mirrorPrefix 为空表示不使用加速镜像。
func NewClient(mirrorPrefix string) *Client {
	prefix := normalizeMirrorPrefix(mirrorPrefix)

	// 安全：只校验初始 URL 是不够的。GitHub 的下载链接会 302 到资产主机，
	// 若不校验每一跳，一个被入侵/被劫持的响应就能把下载引到任意主机。
	// 这里对每次重定向重新执行同一套白名单校验。
	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("重定向次数过多")
		}
		return ValidateDownloadURL(req.URL.String(), prefix)
	}

	return &Client{
		apiClient: &http.Client{
			Timeout:       apiTimeout,
			CheckRedirect: checkRedirect,
		},
		downloadClient: &http.Client{
			Timeout:       downloadTimeout,
			CheckRedirect: checkRedirect,
		},
		mirrorPrefix: prefix,
	}
}

func normalizeMirrorPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	// 只接受 https，避免加速前缀把下载降级成明文
	if !strings.HasPrefix(prefix, "https://") {
		return ""
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}

// MirrorPrefix 返回生效的加速前缀（可能为空）
func (c *Client) MirrorPrefix() string { return c.mirrorPrefix }

func (c *Client) newAPIRequest(ctx context.Context, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gost-panel-updater")
	return req, nil
}

// FetchLatestRelease 获取最新的正式发布
func (c *Client) FetchLatestRelease(ctx context.Context, repo string) (*Release, error) {
	rawURL := fmt.Sprintf("https://%s/repos/%s/releases/latest", apiHost, repo)
	req, err := c.newAPIRequest(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()

	if err := checkAPIStatus(resp); err != nil {
		return nil, err
	}

	var release Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxChecksumBytes*8)).Decode(&release); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	return &release, nil
}

// FetchRecentReleases 获取最近的若干个发布（用于列出可回滚版本）
func (c *Client) FetchRecentReleases(ctx context.Context, repo string, perPage int) ([]*Release, error) {
	if perPage <= 0 {
		perPage = 10
	}
	if perPage > 100 {
		perPage = 100 // GitHub API 硬上限
	}

	rawURL := fmt.Sprintf("https://%s/repos/%s/releases?per_page=%d", apiHost, repo, perPage)
	req, err := c.newAPIRequest(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()

	if err := checkAPIStatus(resp); err != nil {
		return nil, err
	}

	var releases []*Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxChecksumBytes*64)).Decode(&releases); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	return releases, nil
}

// FetchChecksums 获取校验和文件内容。
// 安全：刻意不经过加速镜像，保证校验基准来自 GitHub 本身。
func (c *Client) FetchChecksums(ctx context.Context, rawURL string) ([]byte, error) {
	if err := ValidateDownloadURL(rawURL, ""); err != nil {
		return nil, err
	}

	req, err := c.newAPIRequest(ctx, rawURL)
	if err != nil {
		return nil, err
	}

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载校验和失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载校验和失败: HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(io.LimitReader(resp.Body, maxChecksumBytes))
}

// DownloadFile 校验来源后下载文件到 dest，并限制最大体积。
func (c *Client) DownloadFile(ctx context.Context, rawURL, dest string, maxSize int64) error {
	if err := ValidateDownloadURL(rawURL, c.mirrorPrefix); err != nil {
		return err
	}
	return c.downloadTo(ctx, rawURL, dest, maxSize)
}

// downloadTo 只负责传输与体积限制，不做来源校验。
// 拆开是为了能独立测试"服务端谎报 Content-Length 时仍受限"这条防线。
func (c *Client) downloadTo(ctx context.Context, rawURL, dest string, maxSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "gost-panel-updater")

	resp, err := c.downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}

	// 服务端声明的体积就已超限时直接放弃，省掉无谓的传输
	if resp.ContentLength > maxSize {
		return fmt.Errorf("文件过大: %d 字节（上限 %d）", resp.ContentLength, maxSize)
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	// 即使 Content-Length 缺失或谎报，也用 LimitReader 兜住实际写入量
	written, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxSize+1))
	closeErr := out.Close()

	if copyErr != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("下载失败: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return closeErr
	}
	if written > maxSize {
		_ = os.Remove(dest)
		return fmt.Errorf("下载内容超过上限 %d 字节", maxSize)
	}

	return nil
}

// MirrorURL 把 GitHub 下载地址转换为经过加速前缀的地址。
// 未配置镜像时原样返回。
func (c *Client) MirrorURL(rawURL string) string {
	if c.mirrorPrefix == "" {
		return rawURL
	}
	return c.mirrorPrefix + rawURL
}

// ValidateDownloadURL 校验下载地址是否可信。
//
// 安全：这是防止更新流程被引导到任意主机（SSRF / 投毒）的关键一环。
// 只允许 https，且主机必须是 GitHub 官方域名；
// 额外允许运维显式配置的加速镜像前缀 —— 镜像不可信也无妨，
// 因为二进制的 SHA256 基准始终来自直连 GitHub 获取的校验和文件。
func ValidateDownloadURL(rawURL, mirrorPrefix string) error {
	// 加速镜像的形态是 "https://mirror/https://github.com/..."，
	// 先剥掉前缀再校验其后的真实地址。
	if mirrorPrefix != "" && strings.HasPrefix(rawURL, mirrorPrefix) {
		rawURL = strings.TrimPrefix(rawURL, mirrorPrefix)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("无效的下载地址: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("下载地址必须使用 https: %s", parsed.Scheme)
	}
	// URL 中携带用户信息（https://user@host/）常被用来伪装主机
	if parsed.User != nil {
		return fmt.Errorf("下载地址不得包含用户信息")
	}

	host := strings.ToLower(parsed.Hostname())
	for _, allowed := range []string{apiHost, releaseHost, assetHost} {
		// 精确匹配或其子域；注意必须带点比较，
		// 否则 "github.com.evil.com" 会被误判为可信
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return nil
		}
	}
	return fmt.Errorf("下载地址主机不可信: %s", host)
}

func checkAPIStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("未找到发布信息（仓库不存在或尚无正式发布）")
	case http.StatusForbidden, http.StatusTooManyRequests:
		// 未认证调用 GitHub API 每小时 60 次，超限会返回 403
		return fmt.Errorf("GitHub API 访问受限（HTTP %d），请稍后再试", resp.StatusCode)
	default:
		return fmt.Errorf("GitHub API 返回 HTTP %d", resp.StatusCode)
	}
}
