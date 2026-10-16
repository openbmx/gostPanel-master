package service

import (
	"encoding/json"
	stderrors "errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
)

func isHopProtocolError(err error) bool {
	var biz *errors.BizError
	return stderrors.As(err, &biz) && biz.Code == errors.ErrTunnelHopProtocol.Code
}

// 请求体的 oneof 白名单与面板实际支持的协议必须一致：少了前端选得到却提交不了，多了能存却下发不出去
func TestTunnelTransportsMatchDTOProtocolWhitelist(t *testing.T) {
	want := make([]string, 0, len(tunnelTransports))
	for name := range tunnelTransports {
		want = append(want, name)
	}
	sort.Strings(want)

	for _, typ := range []reflect.Type{
		reflect.TypeOf(dto.TunnelHopReq{}),
		reflect.TypeOf(dto.CreateTunnelReq{}),
		reflect.TypeOf(dto.UpdateTunnelReq{}),
	} {
		field, _ := typ.FieldByName("Protocol")
		var got []string
		for _, rule := range strings.Split(field.Tag.Get("binding"), ",") {
			if values, ok := strings.CutPrefix(rule, "oneof="); ok {
				got = strings.Fields(values)
			}
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s.Protocol 的 oneof 与 tunnelTransports 不一致:\n got %v\nwant %v", typ.Name(), got, want)
		}
	}
}

func TestValidateTunnelHopProtocolPositions(t *testing.T) {
	// gost 3.2.6 实测：后续跳点经前一跳 relay 用 TCP 转接，多路复用类（h2/grpc/ssh/pht）也能走通
	for _, p := range []string{"tcp", "tls", "mtls", "ws", "wss", "mws", "mwss", "h2", "h2c", "grpc", "ssh", "mtcp", "ohttp", "pht", "phts"} {
		if err := validateTunnelHopProtocol(1, p); err != nil {
			t.Errorf("%s 应能用于后续跳点: %v", p, err)
		}
	}
	for _, p := range []string{"quic", "kcp"} {
		if err := validateTunnelHopProtocol(0, p); err != nil {
			t.Errorf("%s 应能用于第 1 跳: %v", p, err)
		}
		if err := validateTunnelHopProtocol(1, p); !isHopProtocolError(err) {
			t.Errorf("%s 用于第 2 跳应被拒绝，实际 %v", p, err)
		}
	}
	// 单跳实测也不通的通道不提供
	for _, p := range []string{"udp", "ftcp", "otls", "bogus"} {
		if err := validateTunnelHopProtocol(0, p); !isHopProtocolError(err) {
			t.Errorf("%s 应被拒绝，实际 %v", p, err)
		}
	}
}

func TestTunnelServiceCreateRejectsUDPTransportAfterFirstHop(t *testing.T) {
	db := newTunnelChainServiceTestDB(t)
	svc := NewTunnelService(db)
	entry := createTunnelChainNode(t, db, "entry")
	middle := createTunnelChainNode(t, db, "middle")
	exit := createTunnelChainNode(t, db, "exit")

	_, err := svc.Create(&dto.CreateTunnelReq{
		Name:        "kcp-second",
		EntryNodeID: entry.ID,
		Hops: []dto.TunnelHopReq{
			{NodeID: middle.ID, Protocol: "ws", RelayPort: 9001},
			{NodeID: exit.ID, Protocol: "kcp", RelayPort: 9002},
		},
	}, 1, "admin", "127.0.0.1", "test")
	if !isHopProtocolError(err) {
		t.Fatalf("第 2 跳用 KCP 应被拒绝，实际 %v", err)
	}

	if _, err = svc.Create(&dto.CreateTunnelReq{
		Name:        "kcp-first",
		EntryNodeID: entry.ID,
		Hops: []dto.TunnelHopReq{
			{NodeID: middle.ID, Protocol: "kcp", RelayPort: 9001},
			{NodeID: exit.ID, Protocol: "grpc", RelayPort: 9002},
		},
	}, 1, "admin", "127.0.0.1", "test"); err != nil {
		t.Fatalf("第 1 跳用 KCP、第 2 跳用 gRPC 应允许: %v", err)
	}
}

// 旧版本可能存下了不可用的配置（如第 2 跳 KCP），启动时要明确拒绝，而不是“启动成功”却不通
func TestTunnelStartRejectsStoredInvalidHopProtocol(t *testing.T) {
	db := newTunnelChainServiceTestDB(t)
	svc := NewTunnelService(db)
	entry := createTunnelChainNode(t, db, "entry")
	middle := createTunnelChainNode(t, db, "middle")
	exit := createTunnelChainNode(t, db, "exit")

	tunnel := &model.GostTunnel{
		Name:        "legacy-kcp-second",
		EntryNodeID: entry.ID,
		ExitNodeID:  exit.ID,
		Protocol:    "kcp",
		RelayPort:   9002,
		Status:      model.TunnelStatusStopped,
		Hops: []model.TunnelHop{
			{NodeID: middle.ID, Protocol: "ws", RelayPort: 9001},
			{NodeID: exit.ID, Protocol: "kcp", RelayPort: 9002},
		},
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}

	if err := svc.Start(tunnel.ID, 1, "admin", "127.0.0.1", "test"); !isHopProtocolError(err) {
		t.Fatalf("启动第 2 跳为 KCP 的隧道应被拒绝，实际 %v", err)
	}
	var reloaded model.GostTunnel
	if err := db.First(&reloaded, tunnel.ID).Error; err != nil {
		t.Fatalf("reload tunnel failed: %v", err)
	}
	if reloaded.Status != model.TunnelStatusStopped {
		t.Fatalf("配置不可用时不应改动状态，实际 %s", reloaded.Status)
	}
}

func TestTunnelRuntimePlanKCPMetadataIsIdenticalOnBothEnds(t *testing.T) {
	nodes := map[uint]*model.GostNode{2: {ID: 2, Name: "exit", Address: "10.0.0.2"}}
	tunnel := &model.GostTunnel{
		ID:          9,
		EntryNodeID: 1,
		Hops:        []model.TunnelHop{{NodeID: 2, Protocol: "kcp", RelayPort: 8443}},
	}
	plan, err := buildTunnelRuntimePlan(tunnel, nodes)
	if err != nil {
		t.Fatalf("build runtime plan failed: %v", err)
	}
	listener := plan.Relays[0].Service.Listener
	dialer := plan.Chain.Hops[0].Nodes[0].Dialer
	if listener.Type != "kcp" || dialer.Type != "kcp" {
		t.Fatalf("应下发 kcp listener/dialer，实际 %s/%s", listener.Type, dialer.Type)
	}
	if !reflect.DeepEqual(listener.Metadata, dialer.Metadata) {
		t.Fatalf("listener 与 dialer 元数据必须一致\nlistener %v\ndialer   %v", listener.Metadata, dialer.Metadata)
	}

	md := listener.Metadata
	// 密钥、压缩与之前不带元数据时一致，单端重建也能互通；模式改为 gost 声明的默认 fast
	if md["kcp.tcp"] != false || md["kcp.key"] != "" || md["kcp.mode"] != "fast" || md["kcp.nocomp"] != false {
		t.Errorf("顶层参数不对: %v", md)
	}
	// 顶层整数必须是字符串：经 API 下发后 JSON 数字在 gost 里是 float64，会被当成 0
	for _, k := range []string{"kcp.mtu", "kcp.sndwnd", "kcp.rcvwnd", "kcp.keepalive", "kcp.smuxver", "kcp.smuxbuf", "kcp.streambuf"} {
		if v, ok := md[k].(string); !ok || v == "" || v == "0" {
			t.Errorf("%s 应为非零的字符串，实际 %#v", k, md[k])
		}
	}
	// 带 config 才不会和同一节点上的其它 KCP 服务共用全局配置；这些字段只从 config 读取
	cfg, ok := md["config"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 config")
	}
	if cfg["datashard"] != 10 || cfg["parityshard"] != 3 {
		t.Errorf("FEC 应与之前一致（10:3）: %v", cfg)
	}
	if sockbuf, _ := cfg["sockbuf"].(int); sockbuf <= 0 {
		t.Errorf("sockbuf 缺失会让 gost 把收发缓冲设成 0: %v", cfg)
	}

	// 每次生成新的 map：listener 与 dialer 不能共用同一个实例
	listener.Metadata["probe"] = true
	if _, shared := dialer.Metadata["probe"]; shared {
		t.Fatal("listener 与 dialer 共用了同一个元数据 map")
	}

	// 其它协议不带元数据，下发的 JSON 与之前完全一样
	plain := &model.GostTunnel{ID: 9, EntryNodeID: 1, Hops: []model.TunnelHop{{NodeID: 2, Protocol: "grpc", RelayPort: 8443}}}
	plan, err = buildTunnelRuntimePlan(plain, nodes)
	if err != nil {
		t.Fatalf("build runtime plan failed: %v", err)
	}
	if plan.Relays[0].Service.Listener.Metadata != nil || plan.Chain.Hops[0].Nodes[0].Dialer.Metadata != nil {
		t.Fatalf("非 KCP 协议不应带元数据")
	}
	raw, _ := json.Marshal(plan.Chain)
	if strings.Contains(string(raw), "metadata") {
		t.Fatalf("非 KCP 协议的 chain JSON 不应出现 metadata: %s", raw)
	}
}
