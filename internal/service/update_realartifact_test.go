package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gost-panel/pkg/github"
)

// TestRealReleaseArtifact 用 GitHub 上真实发布的产物跑一遍校验与解包。
//
// 这条链路上的每一环单测都覆盖了，但它们用的都是构造出来的压缩包。
// 真正会悄悄断掉的是「发布流程改了打包方式，而更新器还按老格式解析」——
// 只有拿真实产物跑才能发现。
//
// 默认跳过（需要网络）。手动执行：
//
//	go test ./internal/service/ -run TestRealReleaseArtifact -tags= -v \
//	  -args -real-release=v1.1.0-rc1
//
// 或设置环境变量 GOSTPANEL_TEST_RELEASE=v1.1.0-rc1。
func TestRealReleaseArtifact(t *testing.T) {
	tag := os.Getenv("GOSTPANEL_TEST_RELEASE")
	if tag == "" {
		t.Skip("未设置 GOSTPANEL_TEST_RELEASE，跳过（该用例需要访问网络）")
	}

	ctx := context.Background()
	client := github.NewClient("")

	releases, err := client.FetchRecentReleases(ctx, "openbmx/gostPanel-master", 20)
	if err != nil {
		t.Fatalf("拉取发布列表失败: %v", err)
	}

	var target *github.Release
	for _, r := range releases {
		if r.TagName == tag {
			target = r
			break
		}
	}
	if target == nil {
		t.Fatalf("未找到发布 %s", tag)
	}
	t.Logf("发布 %s，预发布=%v，产物 %d 个", target.TagName, target.Prerelease, len(target.Assets))

	assets := make([]github.Asset, len(target.Assets))
	copy(assets, target.Assets)

	archive, checksum := selectAssets(assets)
	if archive == nil {
		t.Fatalf("未能为当前平台匹配到产物")
	}
	if checksum == nil {
		t.Fatal("发布中缺少 checksums.txt")
	}
	t.Logf("匹配到产物: %s", archive.Name)

	dir := t.TempDir()

	checksums, err := client.FetchChecksums(ctx, checksum.BrowserDownloadURL)
	if err != nil {
		t.Fatalf("下载 checksums.txt 失败: %v", err)
	}

	archivePath := filepath.Join(dir, archive.Name)
	if err := client.DownloadFile(ctx, archive.BrowserDownloadURL, archivePath, maxDownloadSize); err != nil {
		t.Fatalf("下载产物失败: %v", err)
	}

	if err := verifyChecksum(archivePath, archive.Name, checksums); err != nil {
		t.Fatalf("真实产物校验失败: %v", err)
	}
	t.Log("SHA256 校验通过")

	binPath := filepath.Join(dir, "extracted")
	if err := extractBinary(archivePath, binPath); err != nil {
		t.Fatalf("从真实产物中提取二进制失败: %v", err)
	}

	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 1<<20 {
		t.Errorf("提取出的二进制过小（%d 字节），可能取错了文件", info.Size())
	}
	t.Logf("提取成功，二进制大小 %.1f MB", float64(info.Size())/(1<<20))

	// 发布包里带着 config/config.yaml，绝不能被一并解包 ——
	// 那会覆盖用户已配置的 JWT 密钥与管理员设置
	for _, leaked := range []string{"config", "config.yaml", "LICENSE", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, leaked)); err == nil {
			t.Errorf("%s 被解包了，只应提取二进制", leaked)
		}
	}
}
