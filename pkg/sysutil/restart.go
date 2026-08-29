// Package sysutil 提供进程级的系统工具。
package sysutil

import (
	"os"
	"runtime"
	"time"

	"gost-panel/pkg/logger"
)

// RestartSupported 返回当前平台能否通过退出实现自动重启。
//
// 该机制依赖进程管理器把退出的服务重新拉起：
//   - Linux + systemd：安装脚本生成的单元含 Restart=always
//   - Docker：compose 中的 restart: unless-stopped
//
// Windows 没有等价的默认机制，退出后不会自动恢复，因此不提供该能力。
func RestartSupported() bool {
	return runtime.GOOS == "linux"
}

// RestartAsync 通过优雅退出触发服务重启。
//
// 刻意不去调用 systemctl：那需要面板进程具备 root 或 polkit 授权，
// 与上一轮的降权改造相冲突。让进程正常退出、由 systemd 按 Restart=always
// 拉起，既不需要任何额外权限，也是社区通行做法。
//
// 延迟是为了让 HTTP 响应先发出去 —— 否则客户端只会看到连接被重置，
// 无法区分"重启成功"与"面板崩了"。
func RestartAsync(delay time.Duration) {
	if !RestartSupported() {
		logger.Warnf("当前平台（%s）不支持自动重启，请手动重启面板服务", runtime.GOOS)
		return
	}

	logger.Warnf("面板将在 %s 后退出，由进程管理器自动拉起", delay)
	go func() {
		time.Sleep(delay)
		// 走 os.Exit 而非 panic：这是预期内的正常退出，
		// 不应触发 Recovery 中间件或在日志里留下堆栈。
		os.Exit(0)
	}()
}
