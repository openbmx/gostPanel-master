package service

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gost-panel/internal/errors"
	"gost-panel/internal/model"
)

// tunnelTransport 描述隧道跳点可选的数据通道，即跳点上 relay 服务的 listener 与入口 chain 节点的 dialer。
type tunnelTransport struct {
	// gostType 下发给 gost 的 listener/dialer 类型
	gostType string
	// firstHopOnly 只能用作链路第 1 跳。后续跳点要经前一跳的 relay 用 TCP 转接过去，
	// 只监听 UDP 的通道接不上（gost 3.2.6 实测）。
	firstHopOnly bool
	// metadata 非空时生成 listener 与 dialer 共用的元数据，两端必须一致
	metadata func() map[string]any
}

// tunnelTransports 是隧道跳点支持的协议，键为面板里的协议名。
//
// gost 3.2.6 实测不能承载 relay 隧道、因此不提供的：
//   - udp、ftcp：relay 协议跑在数据报通道上，单跳也不通
//   - otls：单次写入达到 16KB（一条 TLS 记录的上限）时连接卡死，大流量 TCP 转发会中断
var tunnelTransports = map[string]tunnelTransport{
	"tcp":   {gostType: "tcp"},
	"tls":   {gostType: "tls"},
	"mtls":  {gostType: "mtls"},
	"ws":    {gostType: "ws"},
	"wss":   {gostType: "wss"},
	"mws":   {gostType: "mws"},
	"mwss":  {gostType: "mwss"},
	"h2":    {gostType: "h2"},
	"h2c":   {gostType: "h2c"},
	"grpc":  {gostType: "grpc"},
	"ssh":   {gostType: "ssh"},
	"mtcp":  {gostType: "mtcp"},
	"ohttp": {gostType: "ohttp"},
	"pht":   {gostType: "pht"},
	"phts":  {gostType: "phts"},
	"quic":  {gostType: "quic", firstHopOnly: true},
	"kcp":   {gostType: "kcp", firstHopOnly: true, metadata: kcpMetadata},
}

const (
	kcpMTU       = 1350
	kcpWindow    = 1024
	kcpSockBuf   = 4 * 1024 * 1024
	kcpKeepAlive = 10
)

// kcpMetadata 让 kcp 按 gost 声明的默认参数（fast 模式、1024 窗口、FEC 10:3）运行。
// 之前不带元数据下发，实际跑的是 32 包窗口、开拥塞控制、无快速重传。
// 密钥、加密、FEC、压缩、smux 版本与之前一致，已在运行的隧道只重建一端也能互通。
//
// gost 3.2.6 解析 KCP 元数据有两个坑，这里都要绕开：
//   - 不带 config 时，同一进程里所有 KCP 服务共用并改写同一份全局默认配置；带了 config 之后，
//     key、crypt、mode、mtu、sndwnd、rcvwnd、keepalive、smuxver、smuxbuf、streambuf、nocomp、tcp
//     又会被顶层的 kcp.* 无条件覆盖，缺的就成了零值。所以两处都要给全。
//   - 经 API 下发的 JSON 数字到 gost 里是 float64，取整数时不认（按 0 处理），顶层整数只能写成字符串。
func kcpMetadata() map[string]any {
	return map[string]any{
		"config": map[string]any{
			"key":         "",
			"crypt":       "aes",
			"mode":        "fast",
			"mtu":         kcpMTU,
			"sndwnd":      kcpWindow,
			"rcvwnd":      kcpWindow,
			"datashard":   10,
			"parityshard": 3,
			"nocomp":      false,
			"acknodelay":  false,
			"sockbuf":     kcpSockBuf,
			"smuxver":     1,
			"smuxbuf":     kcpSockBuf,
			"streambuf":   kcpSockBuf / 2,
			"keepalive":   kcpKeepAlive,
			"tcp":         false,
		},
		"kcp.key":       "",
		"kcp.crypt":     "aes",
		"kcp.mode":      "fast",
		"kcp.mtu":       strconv.Itoa(kcpMTU),
		"kcp.sndwnd":    strconv.Itoa(kcpWindow),
		"kcp.rcvwnd":    strconv.Itoa(kcpWindow),
		"kcp.keepalive": strconv.Itoa(kcpKeepAlive),
		"kcp.smuxver":   "1",
		"kcp.smuxbuf":   strconv.Itoa(kcpSockBuf),
		"kcp.streambuf": strconv.Itoa(kcpSockBuf / 2),
		"kcp.nocomp":    false,
		"kcp.tcp":       false,
	}
}

// transportMetadata 返回跳点通道的 listener/dialer 元数据，不需要时为 nil
func transportMetadata(t tunnelTransport) map[string]any {
	if t.metadata == nil {
		return nil
	}
	return t.metadata()
}

// lookupTunnelTransport 按协议名查找通道，未知协议回退为原样下发，交给 gost 处理
func lookupTunnelTransport(protocol string) tunnelTransport {
	if t, ok := tunnelTransports[protocol]; ok {
		return t
	}
	return tunnelTransport{gostType: protocol}
}

// validateTunnelHopProtocol 检查第 index 跳（从 0 起）的协议是否受支持、能否放在这个位置
func validateTunnelHopProtocol(index int, protocol string) error {
	t, ok := tunnelTransports[protocol]
	if !ok {
		return errors.WithDetail(errors.ErrTunnelHopProtocol,
			fmt.Sprintf("第 %d 跳的 %q 不受支持，可选：%s", index+1, protocol, supportedTunnelProtocolList()))
	}
	if t.firstHopOnly && index > 0 {
		return errors.WithDetail(errors.ErrTunnelHopProtocol,
			fmt.Sprintf("第 %d 跳的 %s 只能用于第 1 跳", index+1, protocol))
	}
	return nil
}

// validateTunnelHopProtocols 逐跳检查整条链路
func validateTunnelHopProtocols(hops []model.TunnelHop) error {
	for i, hop := range hops {
		if err := validateTunnelHopProtocol(i, hop.Protocol); err != nil {
			return err
		}
	}
	return nil
}

func supportedTunnelProtocolList() string {
	names := make([]string, 0, len(tunnelTransports))
	for name := range tunnelTransports {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}
