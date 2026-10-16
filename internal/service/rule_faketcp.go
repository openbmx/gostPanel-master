package service

import (
	"fmt"

	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/internal/repository"
	"gost-panel/internal/utils"
	"gost-panel/pkg/gost"
)

// UDP 伪TCP直通：给游戏这类对延迟敏感、自己能容忍丢包的 UDP 流量用。
//
// 隧道的 relay 把 UDP 包封装进可靠的流里传输，丢一个包，后面的都要等重传；gost 的 ftcp 又承载不了 relay
// （实测单跳也不通）。所以开启后，规则的 UDP 子服务不走隧道 chain，而是走规则自己的链路：入口节点用
// forward 连接器 + ftcp 拨号器，把每个 UDP 包原样装进一个伪造的 TCP 报文，发到出口节点上规则专属的
// ftcp 服务，出口节点再一个包一个包地转发给目标。forward 连接器不携带目标地址，目标只能配在出口节点的
// 服务上，因此每条规则在出口节点上各占一个端口。TCP 子服务仍走隧道。
//
// 节点上的对象：入口节点上的 chain rule-<id>-ftcp-chain，出口节点上的服务 rule-<id>-ftcp。
// 多跳隧道时伪 TCP 从入口节点直连最后一跳，不经过中转跳点。

func ruleFakeTCPServiceName(ruleID uint) string { return fmt.Sprintf("rule-%d-ftcp", ruleID) }
func ruleFakeTCPChainName(ruleID uint) string   { return fmt.Sprintf("rule-%d-ftcp-chain", ruleID) }

// usesFakeTCP 规则的 UDP 是否走伪 TCP 直通
func usesFakeTCP(r *model.GostRule) bool {
	return r != nil && r.Type == model.RuleTypeTunnel && r.UDPFakeTCP
}

// ruleFakeTCPPort 出口节点上接收伪 TCP 的端口，未单独设置时与监听端口相同
func ruleFakeTCPPort(r *model.GostRule) int {
	if r.UDPFakeTCPPort > 0 {
		return r.UDPFakeTCPPort
	}
	return r.ListenPort
}

// fakeTCPExitNodeID 伪 TCP 的落点：隧道最后一跳所在的节点；无法确定时返回 0
func fakeTCPExitNodeID(t *model.GostTunnel) uint {
	if hop, ok := t.LastHop(); ok {
		return hop.NodeID
	}
	return 0
}

func buildFakeTCPExitService(r *model.GostRule) *gost.ServiceConfig {
	strategy := r.Strategy
	if strategy == "" || len(r.Targets) == 1 {
		strategy = "round"
	}
	return gost.BuildFakeTCPForwardService(ruleFakeTCPServiceName(r.ID), ruleFakeTCPPort(r), r.Targets, strategy)
}

func buildFakeTCPChain(r *model.GostRule, exitNode *model.GostNode) *gost.ChainConfig {
	return &gost.ChainConfig{
		Name: ruleFakeTCPChainName(r.ID),
		Hops: []*gost.HopConfig{{
			Name: "hop-0",
			Nodes: []*gost.NodeConfig{{
				Name: "ftcp-0",
				Addr: fmt.Sprintf("%s:%d", exitNode.Address, ruleFakeTCPPort(r)),
				// forward 连接器不加任何协议头，ftcp 拨号器一次写入对应一个伪 TCP 报文，UDP 包的边界得以保留
				Connector: &gost.ConnectorConfig{Type: "forward"},
				Dialer:    &gost.DialerConfig{Type: "ftcp"},
			}},
		}},
	}
}

// normalizeUDPFakeTCP 校验「UDP 伪TCP直通」配置，返回要保存的开关与端口，只对隧道转发生效。
// 端口不能与可能用到的隧道（主隧道与备选）出口的 relay 端口相同，否则两者在出口节点上抢同一个 TCP 端口。
func (s *RuleService) normalizeUDPFakeTCP(ruleType model.RuleType, enabled bool, port, listenPort int, tunnelIDs []uint) (bool, int, error) {
	if ruleType != model.RuleTypeTunnel || !enabled {
		return false, 0, nil
	}
	effective := port
	if effective == 0 {
		effective = listenPort
	}
	for _, id := range tunnelIDs {
		tunnel, err := s.tunnelRepo.FindByID(id)
		if err != nil {
			continue // 隧道是否存在由调用方校验
		}
		if hop, ok := tunnel.LastHop(); ok && hop.RelayPort == effective {
			return false, 0, errors.WithDetail(errors.ErrRuleUDPFakeTCP,
				fmt.Sprintf("出口端口 %d 与隧道 %s 出口的 Relay 端口相同，请换一个", effective, tunnel.Name))
		}
	}
	return true, port, nil
}

// deployFakeTCPPath 在出口节点上（重新）创建规则的伪 TCP 服务，并在入口节点上写入指向它的 chain。
// 必须在创建引用该 chain 的 UDP 子服务之前完成：chain 不存在时 GOST 会让 UDP 绕过出口直连目标。
func (s *RuleService) deployFakeTCPPath(entryClient *gost.Client, r *model.GostRule) error {
	if r.TunnelID == nil {
		return errors.ErrTunnelRequired
	}
	// 不能用预加载的 r.Tunnel：启动和切换隧道时 TunnelID 可能刚被改过
	tunnel, err := s.tunnelRepo.FindByID(*r.TunnelID)
	if err != nil {
		return errors.ErrTunnelNotFound
	}
	exitNode, err := s.nodeRepo.FindByID(fakeTCPExitNodeID(tunnel))
	if err != nil {
		return errors.ErrExitNodeNotFound
	}
	if exitNode.Status == model.NodeStatusOffline {
		return errors.ErrExitNodeOffline
	}

	exitClient := utils.GetGostClient(exitNode)
	svc := buildFakeTCPExitService(r)
	// 同名旧服务可能还是编辑前的端口或目标，而 CreateService 遇到同名会直接跳过，所以先删
	if err = exitClient.DeleteService(svc.Name); err != nil {
		return errors.WithDetail(errors.ErrRuleStartFailed,
			fmt.Sprintf("清理出口节点 %s 上的旧伪 TCP 服务失败: %v", exitNode.Name, err))
	}
	if err = exitClient.CreateService(svc); err != nil {
		return errors.WithDetail(errors.ErrRuleStartFailed,
			fmt.Sprintf("在出口节点 %s 上创建伪 TCP 服务失败（端口被占用，或 gost 未以 root 运行）: %v", exitNode.Name, err))
	}
	_ = exitClient.SaveConfig()

	if err = entryClient.UpsertChain(buildFakeTCPChain(r, exitNode)); err != nil {
		return errors.WithDetail(errors.ErrRuleChainCreateFailed, err.Error())
	}
	return nil
}

// removeRuleFakeTCPPath 删除规则在入口节点上的伪 TCP chain，以及它在隧道出口节点上的伪 TCP 服务。
// 调用方必须先删掉入口节点上引用该 chain 的 UDP 子服务，否则它会直连目标。
// 节点离线删不掉的由看门狗在节点上线后清理；返回遇到的第一个错误。
func removeRuleFakeTCPPath(entryClient *gost.Client, r *model.GostRule, tunnel *model.GostTunnel, nodeRepo *repository.NodeRepository) error {
	var firstErr error
	if err := entryClient.DeleteChain(ruleFakeTCPChainName(r.ID)); err != nil {
		firstErr = err
	}
	exitNode, err := nodeRepo.FindByID(fakeTCPExitNodeID(tunnel))
	if err != nil || exitNode.Status == model.NodeStatusOffline {
		return firstErr
	}
	exitClient := utils.GetGostClient(exitNode)
	if err = exitClient.DeleteService(ruleFakeTCPServiceName(r.ID)); err != nil && firstErr == nil {
		firstErr = err
	}
	_ = exitClient.SaveConfig()
	return firstErr
}

// fakeTCPServiceMissing 规则在出口节点上的伪 TCP 服务是否缺失或失败。
// 出口节点本轮没读到配置（离线或读取失败）时无从判断，按没问题处理。
func fakeTCPServiceMissing(r *model.GostRule, exitSnap *nodeSnapshot) bool {
	if !usesFakeTCP(r) || exitSnap == nil {
		return false
	}
	state, ok := exitSnap.services[ruleFakeTCPServiceName(r.ID)]
	return !ok || state == serviceStateFailed
}

// restoreFakeTCPPath 供看门狗使用：补建出口节点上缺失或失败的伪 TCP 服务，以及入口节点上缺失的伪 TCP chain。
// 补建出的 chain 会记入 entrySnap，随后恢复子服务时据此判断 UDP 子服务能否安全下发。
// exitSnap 为空表示出口节点本轮不可达，此时只处理入口节点。返回本次实际恢复的对象，形如 "节点名: 对象名"。
func (s *RuleService) restoreFakeTCPPath(r *model.GostRule, entrySnap, exitSnap *nodeSnapshot) ([]string, error) {
	if r.TunnelID == nil {
		return nil, errors.ErrTunnelRequired
	}
	tunnel, err := s.tunnelRepo.FindByID(*r.TunnelID)
	if err != nil {
		return nil, errors.ErrTunnelNotFound
	}
	exitNode, err := s.nodeRepo.FindByID(fakeTCPExitNodeID(tunnel))
	if err != nil {
		return nil, errors.ErrExitNodeNotFound
	}

	var restored []string
	if fakeTCPServiceMissing(r, exitSnap) {
		svc := buildFakeTCPExitService(r)
		if _, exists := exitSnap.services[svc.Name]; exists {
			// 失败的服务删不掉就不能再创建：创建会因同名服务还在而被跳过
			if err = exitSnap.client.DeleteService(svc.Name); err != nil {
				return restored, fmt.Errorf("节点 %s 删除失败的 %s: %w", exitNode.Name, svc.Name, err)
			}
		}
		if err = exitSnap.client.CreateService(svc); err != nil {
			return restored, fmt.Errorf("节点 %s 创建 %s: %w", exitNode.Name, svc.Name, err)
		}
		_ = exitSnap.client.SaveConfig()
		restored = append(restored, fmt.Sprintf("%s: %s", exitNode.Name, svc.Name))
	}

	chainName := ruleFakeTCPChainName(r.ID)
	if !entrySnap.chains[chainName] {
		if err = entrySnap.client.CreateChain(buildFakeTCPChain(r, exitNode)); err != nil {
			return restored, fmt.Errorf("节点 %s 创建 %s: %w", entrySnap.node.Name, chainName, err)
		}
		_ = entrySnap.client.SaveConfig()
		entrySnap.chains[chainName] = true
		restored = append(restored, fmt.Sprintf("%s: %s", entrySnap.node.Name, chainName))
	}
	return restored, nil
}
