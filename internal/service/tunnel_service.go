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
	logService *LogService
	sysRepo    *repository.SystemConfigRepository
}

func NewTunnelService(db *gorm.DB) *TunnelService {
	return &TunnelService{
		tunnelRepo: repository.NewTunnelRepository(db),
		nodeRepo:   repository.NewNodeRepository(db),
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
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrTunnelNotFound
		}
		return nil, err
	}
	if tunnel.Status == model.TunnelStatusRunning {
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

	if tunnel.Status == model.TunnelStatusRunning {
		if err = s.Stop(id, userID, username, ip, userAgent); err != nil {
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

	createdRelays := make([]tunnelRelayPlan, 0, len(plan.Relays))
	for _, relay := range plan.Relays {
		node := nodes[relay.NodeID]
		client := utils.GetGostClient(node)
		if relay.EnableStats {
			s.configureTunnelRelayObserver(client, relay.Service)
		}
		if err = client.CreateService(relay.Service); err != nil {
			s.rollbackTunnelStart(entryNode, nodes, plan.Chain.Name, createdRelays)
			_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
			return errors.ErrTunnelRelayCreateFailed
		}
		if err = client.SaveConfig(); err != nil {
			s.rollbackTunnelStart(entryNode, nodes, plan.Chain.Name, append(createdRelays, relay))
			_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
			return err
		}
		createdRelays = append(createdRelays, relay)
	}

	entryClient := utils.GetGostClient(entryNode)
	if err = entryClient.CreateChain(plan.Chain); err != nil {
		s.rollbackTunnelStart(entryNode, nodes, plan.Chain.Name, createdRelays)
		_ = s.tunnelRepo.UpdateStatus(id, model.TunnelStatusError)
		return errors.ErrTunnelChainCreateFailed
	}
	if err = entryClient.SaveConfig(); err != nil {
		s.rollbackTunnelStart(entryNode, nodes, plan.Chain.Name, createdRelays)
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
	tunnel, err := s.tunnelRepo.FindByID(id)
	if err != nil {
		return err
	}
	if tunnel.Status != model.TunnelStatusRunning {
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
		if err = entryClient.DeleteChain(chainName); err != nil {
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

func (s *TunnelService) rollbackTunnelStart(entryNode *model.GostNode, nodes map[uint]*model.GostNode, chainName string, relays []tunnelRelayPlan) {
	if entryNode != nil && entryNode.Status == model.NodeStatusOnline {
		entryClient := utils.GetGostClient(entryNode)
		_ = entryClient.DeleteChain(chainName)
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
