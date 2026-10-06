package service

import (
	stderrors "errors"
	"fmt"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/internal/repository"
	"gost-panel/internal/utils"
	"gost-panel/pkg/gost"
	"gost-panel/pkg/logger"

	"gorm.io/gorm"
)

type TunnelService struct {
	tunnelRepo *repository.TunnelRepository
	nodeRepo   *repository.NodeRepository
	ruleRepo   *repository.RuleRepository
	logService *LogService
	sysRepo    *repository.SystemConfigRepository
}

func NewTunnelService(db *gorm.DB) *TunnelService {
	return &TunnelService{
		tunnelRepo: repository.NewTunnelRepository(db),
		nodeRepo:   repository.NewNodeRepository(db),
		ruleRepo:   repository.NewRuleRepository(db),
		logService: NewLogService(db),
		sysRepo:    repository.NewSystemConfigRepository(db),
	}
}

func (s *TunnelService) Create(req *dto.CreateTunnelReq, userID uint, username string, ip, userAgent string) (*model.GostTunnel, error) {
	entryNode, err := s.nodeRepo.FindByID(req.EntryNodeID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrEntryNodeNotFound
		}
		return nil, err
	}

	hops, err := s.normalizeCreateTunnelHops(req)
	if err != nil {
		return nil, err
	}
	exitNode, err := s.nodeRepo.FindByID(hops[len(hops)-1].NodeID)
	if err != nil {
		return nil, err
	}

	tunnel := &model.GostTunnel{
		Name:        req.Name,
		EntryNodeID: req.EntryNodeID,
		Remark:      req.Remark,
		Status:      model.TunnelStatusStopped,
	}
	if err = mirrorTunnelLastHop(tunnel, hops); err != nil {
		return nil, err
	}

	if err = s.tunnelRepo.Create(tunnel); err != nil {
		return nil, err
	}

	s.logService.Record(
		userID,
		username,
		model.ActionCreate,
		model.ResourceTypeTunnel,
		tunnel.ID,
		fmt.Sprintf("创建隧道: %s (%s -> %s)", tunnel.Name, entryNode.Name, exitNode.Name),
		ip,
		userAgent)

	logger.Infof("创建隧道成功: %s", tunnel.Name)
	return tunnel, nil
}

func (s *TunnelService) Update(id uint, req *dto.UpdateTunnelReq, userID uint, username string, ip, userAgent string) (*model.GostTunnel, error) {
	unlock := lockTunnel(id)
	defer unlock()

	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrTunnelNotFound
		}
		return nil, err
	}
	// error 状态的隧道仍被看门狗按旧配置重试，同样要先停止再编辑：
	// 创建接口遇到同名对象会直接跳过，不先清理的话新配置下发不下去
	if tunnel.Status.WantsRunning() {
		return nil, errors.ErrTunnelRunning
	}

	hops, err := s.normalizeUpdateTunnelHops(tunnel.EntryNodeID, req, tunnel.EffectiveHops())
	if err != nil {
		return nil, err
	}

	tunnel.Name = req.Name
	tunnel.Remark = req.Remark
	if err = mirrorTunnelLastHop(tunnel, hops); err != nil {
		return nil, err
	}

	if err = s.tunnelRepo.Update(tunnel); err != nil {
		return nil, err
	}

	s.logService.Record(
		userID,
		username,
		model.ActionUpdate,
		model.ResourceTypeTunnel,
		tunnel.ID,
		fmt.Sprintf("更新隧道: %s", tunnel.Name),
		ip,
		userAgent)

	return tunnel, nil
}

func (s *TunnelService) Delete(id uint, userID uint, username string, ip, userAgent string) error {
	unlock := lockTunnel(id)
	defer unlock()

	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrTunnelNotFound
		}
		return err
	}

	hasRules, err := s.tunnelRepo.HasRules(id)
	if err != nil {
		return err
	}
	if hasRules {
		return errors.ErrTunnelHasRules
	}

	if tunnel.Status.WantsRunning() {
		if err = s.stopCore(id, userID, username, ip, userAgent); err != nil {
			logger.Warnf("停止隧道失败: %v", err)
		}
	}

	if err = s.tunnelRepo.Delete(id); err != nil {
		return err
	}

	s.logService.Record(
		userID,
		username,
		model.ActionDelete,
		model.ResourceTypeTunnel,
		id,
		fmt.Sprintf("删除隧道: %s", tunnel.Name),
		ip,
		userAgent)

	logger.Infof("删除隧道成功: %s", tunnel.Name)
	return nil
}

func (s *TunnelService) GetByID(id uint) (*model.GostTunnel, error) {
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrTunnelNotFound
		}
		return nil, err
	}
	return tunnel, nil
}

func (s *TunnelService) List(req *dto.TunnelListReq) ([]model.GostTunnel, int64, error) {
	req.SetDefaults()

	opt := &repository.QueryOption{
		Conditions: make(map[string]any),
	}
	if req.NodeID == 0 {
		opt.Pagination = &repository.Pagination{
			Page:     req.Page,
			PageSize: req.PageSize,
		}
	}

	if req.Status != "" {
		opt.Conditions["status = ?"] = req.Status
	}
	if req.Keyword != "" {
		opt.Conditions["name LIKE ?"] = []interface{}{"%" + req.Keyword + "%"}
	}

	tunnels, total, err := s.tunnelRepo.List(opt)
	if err != nil || req.NodeID == 0 {
		return tunnels, total, err
	}

	filtered := make([]model.GostTunnel, 0, len(tunnels))
	for _, tunnel := range tunnels {
		if tunnel.UsesNode(req.NodeID) {
			filtered = append(filtered, tunnel)
		}
	}
	total = int64(len(filtered))
	start := (req.Page - 1) * req.PageSize
	if start >= len(filtered) {
		return []model.GostTunnel{}, total, nil
	}
	end := start + req.PageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	return filtered[start:end], total, nil
}

func (s *TunnelService) Start(id uint, userID uint, username string, ip, userAgent string) error {
	unlock := lockTunnel(id)
	defer unlock()

	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		return err
	}
	if tunnel.Status == model.TunnelStatusRunning {
		return nil
	}

	entryNode, err := s.nodeRepo.FindByID(tunnel.EntryNodeID)
	if err != nil {
		return errors.ErrEntryNodeNotFound
	}
	if entryNode.Status == model.NodeStatusOffline {
		return errors.ErrEntryNodeOffline
	}

	hops := tunnel.EffectiveHops()
	nodes := make(map[uint]*model.GostNode, len(hops))
	for _, hop := range hops {
		node, err := s.nodeRepo.FindByID(hop.NodeID)
		if err != nil {
			return errors.ErrExitNodeNotFound
		}
		if node.Status == model.NodeStatusOffline {
			return errors.ErrExitNodeOffline
		}
		nodes[hop.NodeID] = node
	}

	plan, err := buildTunnelRuntimePlan(tunnel, nodes)
	if err != nil {
		_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
		return err
	}

	// 回滚只撤销本次真正新建的对象。节点上原本就有的 relay/chain（例如重新启动一条
	// error 状态的隧道，或看门狗刚补建过）不能删：chain 被删会让挂在上面的规则绕过隧道直连目标
	createdRelays := make([]tunnelRelayPlan, 0, len(plan.Relays))
	for _, relay := range plan.Relays {
		node := nodes[relay.NodeID]
		client := utils.GetGostClient(node)
		var existed bool
		if existed, err = client.ServiceExists(relay.Service.Name); err != nil {
			s.rollbackTunnelStart(entryNode, nodes, "", createdRelays)
			_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
			return errors.ErrTunnelRelayCreateFailed
		}
		if relay.EnableStats {
			s.configureTunnelRelayObserver(client, relay.Service)
		}
		if err = client.CreateService(relay.Service); err != nil {
			s.rollbackTunnelStart(entryNode, nodes, "", createdRelays)
			_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
			return errors.ErrTunnelRelayCreateFailed
		}
		if !existed {
			createdRelays = append(createdRelays, relay)
		}
		if err = client.SaveConfig(); err != nil {
			s.rollbackTunnelStart(entryNode, nodes, "", createdRelays)
			_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
			return err
		}
	}

	entryClient := utils.GetGostClient(entryNode)
	chainExisted, err := entryClient.ChainExists(plan.Chain.Name)
	if err != nil {
		s.rollbackTunnelStart(entryNode, nodes, "", createdRelays)
		_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
		return errors.ErrTunnelChainCreateFailed
	}
	if err = entryClient.UpsertChain(plan.Chain); err != nil {
		s.rollbackTunnelStart(entryNode, nodes, "", createdRelays)
		_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
		return errors.ErrTunnelChainCreateFailed
	}
	if err = entryClient.SaveConfig(); err != nil {
		createdChain := ""
		if !chainExisted {
			createdChain = plan.Chain.Name
		}
		s.rollbackTunnelStart(entryNode, nodes, createdChain, createdRelays)
		_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
		return err
	}

	finalRelayName := plan.Relays[len(plan.Relays)-1].Service.Name
	_ = s.tunnelRepo.UpdateServiceInfo(id, finalRelayName, plan.Chain.Name)
	_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusRunning)

	s.logService.Record(
		userID,
		username,
		model.ActionStart,
		model.ResourceTypeTunnel,
		id,
		fmt.Sprintf("启动隧道: %s", tunnel.Name),
		ip,
		userAgent)

	logger.Infof("启动隧道成功: %s (Chain: %s)", tunnel.Name, plan.Chain.Name)
	return nil
}

func (s *TunnelService) Stop(id uint, userID uint, username string, ip, userAgent string) error {
	unlock := lockTunnel(id)
	defer unlock()
	return s.stopCore(id, userID, username, ip, userAgent)
}

// stopCore 停止隧道的无锁核心实现。调用方必须已持有该隧道的事务锁。
// 离线节点上的对象这里删不掉，它们重新上线后由看门狗按 stopped 清理。
func (s *TunnelService) stopCore(id uint, userID uint, username string, ip, userAgent string) error {
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		return err
	}
	// error 也要能停：它表示看门狗仍在重试恢复，用户点“停止”就是要终止重试并清理残留
	if !tunnel.Status.WantsRunning() {
		return nil
	}

	entryNode, _ := s.nodeRepo.FindByID(tunnel.EntryNodeID)
	hops := tunnel.EffectiveHops()
	nodes := make(map[uint]*model.GostNode, len(hops))
	for _, hop := range hops {
		if node, err := s.nodeRepo.FindByID(hop.NodeID); err == nil {
			nodes[hop.NodeID] = node
		}
	}

	plan, _ := buildTunnelRuntimePlan(tunnel, nodes)
	deleteSucceeded := true

	chainName := tunnel.ChainID
	if chainName == "" {
		chainName = fmt.Sprintf("tunnel-%d-chain", tunnel.ID)
	}
	if entryNode != nil && entryNode.Status == model.NodeStatusOnline {
		entryClient := utils.GetGostClient(entryNode)
		// 先暂停走这条隧道的规则再删 chain：chain 一删，引用它的规则服务会绕过隧道直连目标
		s.suspendTunnelRules(tunnel, entryClient, userID, username, ip, userAgent)
		if s.removeChainReferences(entryClient, chainName) {
			// 还有引用没清掉（或无法确认）：先不删 chain，交给看门狗在引用消失后清理
			deleteSucceeded = false
			logger.Warnf("隧道 %s 的 %s 仍被引用，暂不删除，由看门狗稍后清理", tunnel.Name, chainName)
		} else if err = entryClient.DeleteChain(chainName); err != nil {
			deleteSucceeded = false
			logger.Warnf("删除隧道 Chain 失败: %v", err)
		}
		if err = entryClient.SaveConfig(); err != nil {
			deleteSucceeded = false
			logger.Warnf("保存入口节点 Gost 配置失败: %v", err)
		}
	}

	if plan != nil {
		for _, relay := range plan.Relays {
			node := nodes[relay.NodeID]
			if node == nil || node.Status != model.NodeStatusOnline {
				continue
			}
			client := utils.GetGostClient(node)
			if err = client.DeleteService(relay.Service.Name); err != nil {
				deleteSucceeded = false
				logger.Warnf("删除隧道 Relay 服务失败: %v", err)
			}
			if err = client.SaveConfig(); err != nil {
				deleteSucceeded = false
				logger.Warnf("保存 hop 节点 Gost 配置失败: %v", err)
			}
		}
	} else if tunnel.ServiceID != "" {
		if node := nodes[tunnel.ExitNodeID]; node != nil && node.Status == model.NodeStatusOnline {
			client := utils.GetGostClient(node)
			if err = client.DeleteService(tunnel.ServiceID); err != nil {
				deleteSucceeded = false
			}
			if err = client.SaveConfig(); err != nil {
				deleteSucceeded = false
			}
		}
	}

	if deleteSucceeded {
		_ = s.tunnelRepo.ResetStatsCheckpoint(id)
	}
	_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusStopped)

	s.logService.Record(
		userID,
		username,
		model.ActionStop,
		model.ResourceTypeTunnel,
		id,
		fmt.Sprintf("停止隧道: %s", tunnel.Name),
		ip,
		userAgent)

	logger.Infof("停止隧道成功: %s", tunnel.Name)
	return nil
}

func (s *TunnelService) GetChainID(tunnelID uint) (string, error) {
	tunnel, err := s.tunnelRepo.FindByID(tunnelID)
	if err != nil {
		return "", err
	}
	return tunnel.ChainID, nil
}

func (s *TunnelService) GetEntryNodeID(tunnelID uint) (uint, error) {
	tunnel, err := s.tunnelRepo.FindByID(tunnelID)
	if err != nil {
		return 0, err
	}
	return tunnel.EntryNodeID, nil
}

func (s *TunnelService) configureTunnelRelayObserver(client *gost.Client, relaySvc *gost.ServiceConfig) {
	observerName, err := EnsureGlobalObserver(client, s.sysRepo)
	if err != nil || observerName == "" {
		return
	}
	relaySvc.Observer = observerName
	if relaySvc.Metadata == nil {
		relaySvc.Metadata = make(map[string]any)
	}
	relaySvc.Metadata["enableStats"] = true
	relaySvc.Metadata["observer.period"] = "5s"
	relaySvc.Metadata["observer.resetTraffic"] = false
}

// rollbackTunnelStart 撤销 Start 中途失败前新建的对象；createdChain 为空表示 chain 不是本次新建的，不能删
func (s *TunnelService) rollbackTunnelStart(entryNode *model.GostNode, nodes map[uint]*model.GostNode, createdChain string, relays []tunnelRelayPlan) {
	if createdChain != "" && entryNode != nil && entryNode.Status == model.NodeStatusOnline {
		entryClient := utils.GetGostClient(entryNode)
		_ = entryClient.DeleteChain(createdChain)
		_ = entryClient.SaveConfig()
	}
	for _, relay := range relays {
		node := nodes[relay.NodeID]
		if node == nil || node.Status != model.NodeStatusOnline {
			continue
		}
		client := utils.GetGostClient(node)
		_ = client.DeleteService(relay.Service.Name)
		_ = client.SaveConfig()
	}
}

// suspendTunnelRules 删除当前走这条隧道、应当运行的规则在入口节点上的服务，并把规则记为 error。
//
// 为什么要这样做：服务引用的 chain 不存在时，GOST 不报错，而是直接连接转发目标
// （3.2.6/3.3.0 实测），流量会绕过隧道。停隧道之前若不先处理规则，它们会一直直连。
// 记为 error 而不是 stopped：用户停的是隧道而不是规则，看门狗会把规则切到可用的
// 备选隧道，或在这条隧道重新启动后自动恢复。
func (s *TunnelService) suspendTunnelRules(tunnel *model.GostTunnel, client *gost.Client, userID uint, username, ip, userAgent string) {
	rules, err := s.ruleRepo.FindByTunnelID(tunnel.ID)
	if err != nil {
		logger.Warnf("查询隧道 %s 上的规则失败: %v", tunnel.Name, err)
		return
	}
	for _, r := range rules {
		if !r.Status.WantsRunning() {
			continue
		}
		// 规则正被其他操作处理时跳过：看门狗发现它的 chain 缺失后会在下一轮暂停它
		unlock, ok := tryLockByID(&ruleLocks, r.ID)
		if !ok {
			continue
		}
		rule, err := s.ruleRepo.FindByID(r.ID)
		if err != nil || !rule.Status.WantsRunning() || rule.TunnelID == nil || *rule.TunnelID != tunnel.ID {
			unlock()
			continue
		}

		names := []string{fmt.Sprintf("rule-%d", rule.ID)}
		if rule.ServiceID != "" && rule.ServiceID != names[0] {
			names = append(names, rule.ServiceID)
		}
		for _, base := range names {
			for _, name := range []string{base, base + "-tcp", base + "-udp"} {
				if err = client.DeleteService(name); err != nil {
					logger.Warnf("暂停规则 %s 时删除服务 %s 失败: %v", rule.Name, name, err)
				}
			}
		}
		_ = s.ruleRepo.UpdateStatus(rule.ID, model.RuleStatusError)
		unlock()

		s.logService.Record(userID, username, model.ActionStop, model.ResourceTypeRule, rule.ID,
			fmt.Sprintf("隧道 %s 已停止，暂停规则 %s 的转发（避免流量绕过隧道直连目标），隧道恢复或切到备选隧道后自动重启", tunnel.Name, rule.Name),
			ip, userAgent)
	}
}

// removeChainReferences 兜底：删掉入口节点上仍引用这条 chain 的其它规则服务。
// 正常情况下 suspendTunnelRules 已处理完；节点与数据库不一致时（例如之前某次切换只成功了一半）
// 还会有漏网的，chain 删除后它们会绕过隧道直连目标。它们的规则由看门狗按数据库状态恢复。
// 返回 true 表示仍有引用（删不掉、不是面板的服务，或读不到配置无法确认），此时不能删 chain。
func (s *TunnelService) removeChainReferences(client *gost.Client, chainName string) bool {
	cfg, err := client.GetConfig()
	if err != nil {
		logger.Warnf("读取入口节点配置失败，无法检查 %s 的引用: %v", chainName, err)
		return true
	}
	remaining := false
	for _, svc := range cfg.Services {
		if svc.Handler == nil || svc.Handler.Chain != chainName {
			continue
		}
		if !ruleServicePattern.MatchString(svc.Name) {
			remaining = true // 不是面板的服务，不替用户删
			continue
		}
		if err = client.DeleteService(svc.Name); err != nil {
			logger.Warnf("删除仍引用 %s 的规则服务 %s 失败: %v", chainName, svc.Name, err)
			remaining = true
		}
	}
	return remaining
}

// restoreRuntime 供看门狗使用：按运行计划补建节点上缺失或已失败的 relay 服务与入口 chain。
//
// 与 Start 不同，这里失败时不回滚：已经恢复的部分保留在节点上，剩下的交给下一轮重试。
// snapshots 是相关节点的最新运行时快照，不在其中的节点（离线或读取失败）本轮跳过。
// 返回本次实际恢复的对象，形如 "节点名: 对象名"。
func (s *TunnelService) restoreRuntime(tunnel *model.GostTunnel, plan *tunnelRuntimePlan, snapshots map[uint]*nodeSnapshot) ([]string, error) {
	var restored []string
	for _, relay := range plan.Relays {
		snap := snapshots[relay.NodeID]
		if snap == nil {
			continue
		}
		state, exists := snap.services[relay.Service.Name]
		if exists && state != serviceStateFailed {
			continue
		}
		if relay.EnableStats {
			s.configureTunnelRelayObserver(snap.client, relay.Service)
		}
		if exists {
			if err := snap.client.DeleteService(relay.Service.Name); err != nil {
				return restored, fmt.Errorf("节点 %s 删除失败的 %s: %w", snap.node.Name, relay.Service.Name, err)
			}
		}
		if err := snap.client.CreateService(relay.Service); err != nil {
			return restored, fmt.Errorf("节点 %s 创建 %s: %w", snap.node.Name, relay.Service.Name, err)
		}
		_ = snap.client.SaveConfig()
		restored = append(restored, fmt.Sprintf("%s: %s", snap.node.Name, relay.Service.Name))
	}

	if snap := snapshots[tunnel.EntryNodeID]; snap != nil && !snap.chains[plan.Chain.Name] {
		if err := snap.client.CreateChain(plan.Chain); err != nil {
			return restored, fmt.Errorf("节点 %s 创建 %s: %w", snap.node.Name, plan.Chain.Name, err)
		}
		_ = snap.client.SaveConfig()
		restored = append(restored, fmt.Sprintf("%s: %s", snap.node.Name, plan.Chain.Name))
	}

	if len(restored) > 0 {
		finalRelayName := plan.Relays[len(plan.Relays)-1].Service.Name
		if tunnel.ServiceID != finalRelayName || tunnel.ChainID != plan.Chain.Name {
			_ = s.tunnelRepo.UpdateServiceInfo(tunnel.ID, finalRelayName, plan.Chain.Name)
		}
	}
	return restored, nil
}
