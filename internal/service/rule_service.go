package service

import (
	stderrors "errors"
	"fmt"
	"strings"
	"sync"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/internal/repository"
	"gost-panel/internal/utils"
	"gost-panel/pkg/gost"
	"gost-panel/pkg/logger"

	"gorm.io/gorm"
)

// RuleService provides rule management logic.
// Forward rules use NodeID, tunnel rules use TunnelID.
type RuleService struct {
	ruleRepo      *repository.RuleRepository
	nodeRepo      *repository.NodeRepository
	tunnelRepo    *repository.TunnelRepository
	sysRepo       *repository.SystemConfigRepository
	logService    *LogService
	tunnelService *TunnelService

	// ruleLocks 为每条规则提供独立的事务锁，串行化该规则的启动/停止/切换/故障转移操作，
	// 避免后台 AutoFailover 与用户手动操作并发修改同一规则时产生的状态错乱。
	ruleLocks sync.Map // map[uint]*sync.Mutex
}

// NewRuleService creates a rule service.
func NewRuleService(db *gorm.DB) *RuleService {
	return &RuleService{
		ruleRepo:      repository.NewRuleRepository(db),
		nodeRepo:      repository.NewNodeRepository(db),
		tunnelRepo:    repository.NewTunnelRepository(db),
		sysRepo:       repository.NewSystemConfigRepository(db),
		logService:    NewLogService(db),
		tunnelService: NewTunnelService(db),
	}
}

// lockRule 获取指定规则的事务锁，返回解锁函数。
func (s *RuleService) lockRule(id uint) func() {
	v, _ := s.ruleLocks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// tryLockRule 尝试获取规则事务锁，成功返回解锁函数与 true；
// 若锁被占用（说明该规则正被其他操作处理）则返回 false，调用方应跳过本次操作。
func (s *RuleService) tryLockRule(id uint) (func(), bool) {
	v, _ := s.ruleLocks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// Create creates a rule.
func (s *RuleService) Create(req *dto.CreateRuleReq, userID uint, username string, ip, userAgent string) (*model.GostRule, error) {
	var entryNodeID uint

	if req.Type == string(model.RuleTypeForward) {
		if req.NodeID == nil || *req.NodeID == 0 {
			return nil, errors.ErrNodeRequired
		}
		if _, err := s.nodeRepo.FindByID(*req.NodeID); err != nil {
			if stderrors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errors.ErrNodeNotFound
			}
			return nil, err
		}
		entryNodeID = *req.NodeID
	} else if req.Type == string(model.RuleTypeTunnel) {
		if req.TunnelID == nil || *req.TunnelID == 0 {
			return nil, errors.ErrTunnelRequired
		}
		primaryTunnel, err := s.tunnelRepo.FindByID(*req.TunnelID)
		if err != nil {
			if stderrors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errors.ErrTunnelNotFound
			}
			return nil, err
		}
		backupTunnelIDs, err := s.normalizeBackupTunnelIDs(req.TunnelID, req.BackupTunnelIDs)
		if err != nil {
			return nil, err
		}
		entryNodeID = primaryTunnel.EntryNodeID
		req.BackupTunnelIDs = backupTunnelIDs
	} else {
		return nil, errors.ErrRuleTypeInvalid
	}

	exists, err := s.ruleRepo.ExistsByPort(entryNodeID, req.ListenPort)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.ErrRulePortExists
	}

	rule := &model.GostRule{
		NodeID:          req.NodeID,
		TunnelID:        req.TunnelID,
		PrimaryTunnelID: req.TunnelID, // 记录用户指定的主链路，切换备选时不覆盖此字段
		BackupTunnelIDs: req.BackupTunnelIDs,
		Name:            req.Name,
		Type:            model.RuleType(req.Type),
		ListenPort:      req.ListenPort,
		Targets:         req.Targets,
		Strategy:        req.Strategy,
		EnableTLS:       req.EnableTLS,
		Remark:          req.Remark,
		Status:          model.RuleStatusStopped,
	}

	if err = s.ruleRepo.Create(rule); err != nil {
		return nil, err
	}

	s.logService.Record(
		userID,
		username,
		model.ActionCreate,
		model.ResourceTypeRule,
		rule.ID,
		fmt.Sprintf("创建规则: %s (类型: %s)", rule.Name, rule.Type),
		ip,
		userAgent,
	)

	logger.Infof("创建规则成功: %s (:%d)", rule.Name, rule.ListenPort)
	return rule, nil
}

// Update updates a rule.
func (s *RuleService) Update(id uint, req *dto.UpdateRuleReq, userID uint, username string, ip, userAgent string) (*model.GostRule, error) {
	unlock := s.lockRule(id)
	defer unlock()

	rule, err := s.ruleRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrRuleNotFound
		}
		return nil, err
	}

	prevRule := *rule
	prevTargets := append([]string(nil), rule.Targets...)
	prevBackups := append([]uint(nil), rule.BackupTunnelIDs...)
	prevRule.Targets = prevTargets
	prevRule.BackupTunnelIDs = prevBackups

	if rule.Type == model.RuleTypeTunnel && (req.TunnelID == nil || *req.TunnelID == 0) {
		return nil, errors.ErrTunnelRequired
	}

	entryNodeID := s.getEntryNodeID(rule)
	if rule.Type == model.RuleTypeTunnel {
		tunnel, err := s.tunnelRepo.FindByID(*req.TunnelID)
		if err != nil {
			if stderrors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errors.ErrTunnelNotFound
			}
			return nil, err
		}
		entryNodeID = tunnel.EntryNodeID
	}

	exists, err := s.ruleRepo.ExistsByPort(entryNodeID, req.ListenPort, id)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.ErrRulePortExists
	}

	if rule.Type == model.RuleTypeTunnel && rule.Status == model.RuleStatusRunning {
		if err = s.validateTunnelSwitchTarget(req.TunnelID); err != nil {
			return nil, err
		}
	}

	wasRunning := rule.Status == model.RuleStatusRunning
	if wasRunning {
		if err = s.stopCore(id, userID, username, ip, userAgent); err != nil {
			logger.Warnf("更新规则前停止失败: %v", err)
			return nil, err
		}
		rule.Status = model.RuleStatusStopped
	}

	rule.Name = req.Name
	rule.ListenPort = req.ListenPort
	rule.Targets = req.Targets
	rule.Strategy = req.Strategy
	rule.EnableTLS = req.EnableTLS
	rule.Remark = req.Remark

	if rule.Type == model.RuleTypeTunnel {
		rule.TunnelID = req.TunnelID
		rule.PrimaryTunnelID = req.TunnelID // 用户主动修改 → 同步更新主链路记录
		backupTunnelIDs, err := s.normalizeBackupTunnelIDs(req.TunnelID, req.BackupTunnelIDs)
		if err != nil {
			return nil, err
		}
		rule.BackupTunnelIDs = backupTunnelIDs
	} else {
		rule.TunnelID = nil
		rule.PrimaryTunnelID = nil
		rule.BackupTunnelIDs = nil
	}

	if err = s.ruleRepo.UpdateConfig(rule); err != nil {
		return nil, err
	}

	if wasRunning {
		if err = s.startCore(id, userID, username, ip, userAgent); err != nil {
			logger.Warnf("更新规则后重新启动失败: %v", err)

			rollbackRule := prevRule
			rollbackRule.Status = model.RuleStatusStopped
			if rbErr := s.ruleRepo.UpdateConfig(&rollbackRule); rbErr != nil {
				logger.Errorf("更新失败后回滚规则配置失败: %v", rbErr)
				return nil, err
			}
			if restartErr := s.startCore(id, userID, username, ip, userAgent); restartErr != nil {
				logger.Errorf("更新失败后恢复旧规则启动失败: %v", restartErr)
				return nil, err
			}
			return nil, err
		}
		rule.Status = model.RuleStatusRunning
	}

	s.logService.Record(
		userID,
		username,
		model.ActionUpdate,
		model.ResourceTypeRule,
		rule.ID,
		fmt.Sprintf("更新规则: %s", rule.Name),
		ip,
		userAgent,
	)

	return rule, nil
}

func (s *RuleService) validateTunnelSwitchTarget(tunnelID *uint) error {
	if tunnelID == nil || *tunnelID == 0 {
		return errors.ErrTunnelRequired
	}

	tunnel, err := s.tunnelRepo.FindByID(*tunnelID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrTunnelNotFound
		}
		return err
	}

	if tunnel.Status != model.TunnelStatusRunning {
		return errors.ErrTunnelNotRunning
	}
	if tunnel.ChainID == "" {
		return errors.ErrTunnelChainNotFound
	}

	entryNode, err := s.nodeRepo.FindByID(tunnel.EntryNodeID)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrNodeNotFound
		}
		return err
	}
	if entryNode.Status == model.NodeStatusOffline {
		return errors.ErrEntryNodeOffline
	}

	return nil
}

// Delete deletes a rule.
func (s *RuleService) Delete(id uint, userID uint, username string, ip, userAgent string) error {
	unlock := s.lockRule(id)
	defer unlock()

	rule, err := s.ruleRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrRuleNotFound
		}
		return err
	}

	if rule.Status == model.RuleStatusRunning {
		if err = s.stopCore(id, userID, username, ip, userAgent); err != nil {
			logger.Warnf("停止规则失败: %v", err)
		}
	}

	if err = s.ruleRepo.Delete(id); err != nil {
		return err
	}

	s.logService.Record(
		userID,
		username,
		model.ActionDelete,
		model.ResourceTypeRule,
		id,
		fmt.Sprintf("删除规则: %s", rule.Name),
		ip,
		userAgent,
	)

	logger.Infof("删除规则成功: %s", rule.Name)
	return nil
}

// GetByID gets rule details.
func (s *RuleService) GetByID(id uint) (*model.GostRule, error) {
	rule, err := s.ruleRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrRuleNotFound
		}
		return nil, err
	}
	return rule, nil
}

// List lists rules.
func (s *RuleService) List(req *dto.RuleListReq) ([]model.GostRule, int64, error) {
	req.SetDefaults()

	opt := &repository.QueryOption{
		Pagination: &repository.Pagination{
			Page:     req.Page,
			PageSize: req.PageSize,
		},
		Conditions: make(map[string]any),
	}

	if req.NodeID > 0 {
		opt.Conditions["node_id = ?"] = req.NodeID
	}
	if req.TunnelID > 0 {
		opt.Conditions["tunnel_id = ?"] = req.TunnelID
	}
	if req.Type != "" {
		opt.Conditions["type = ?"] = req.Type
	}
	if req.Status != "" {
		opt.Conditions["status = ?"] = req.Status
	}
	if req.Keyword != "" {
		opt.Conditions["name LIKE ?"] = []interface{}{"%" + req.Keyword + "%"}
	}

	return s.ruleRepo.List(opt)
}

// Start starts a rule.
func (s *RuleService) Start(id uint, userID uint, username string, ip, userAgent string) error {
	unlock := s.lockRule(id)
	defer unlock()
	return s.startCore(id, userID, username, ip, userAgent)
}

// startCore 启动规则的无锁核心实现。调用方必须已持有该规则的事务锁。
func (s *RuleService) startCore(id uint, userID uint, username string, ip, userAgent string) error {
	rule, err := s.ruleRepo.FindByID(id)
	if err != nil {
		return err
	}

	if rule.Status == model.RuleStatusRunning {
		return nil
	}

	if rule.Type == model.RuleTypeTunnel {
		selectedTunnel, err := s.selectAvailableTunnel(rule)
		if err != nil {
			return err
		}
		if rule.TunnelID == nil || *rule.TunnelID != selectedTunnel.ID {
			_ = s.ruleRepo.UpdateFields(&model.GostRule{}, rule.ID, map[string]any{"tunnel_id": selectedTunnel.ID})
			rule.TunnelID = &selectedTunnel.ID
		}
	}

	entryNodeID := s.getEntryNodeID(rule)
	node, err := s.nodeRepo.FindByID(entryNodeID)
	if err != nil {
		return errors.ErrNodeNotFound
	}
	if node.Status == model.NodeStatusOffline {
		return errors.ErrNodeOffline
	}

	client := utils.GetGostClient(node)
	serviceName := fmt.Sprintf("rule-%d", rule.ID)

	if rule.Type == model.RuleTypeTunnel {
		if err = s.startTunnelRule(rule, client, serviceName); err != nil {
			return err
		}
	} else {
		if err = s.startForwardRule(rule, client, serviceName); err != nil {
			return err
		}
	}

	s.logService.Record(
		userID,
		username,
		model.ActionStart,
		model.ResourceTypeRule,
		id,
		fmt.Sprintf("启动规则: %s", rule.Name),
		ip,
		userAgent,
	)

	logger.Infof("启动规则成功: %s", rule.Name)
	return nil
}

// AutoFailover checks running tunnel rules and switches to a usable backup tunnel.
func (s *RuleService) AutoFailover() {
	rules, _, err := s.ruleRepo.List(&repository.QueryOption{
		Conditions: map[string]any{
			"type = ?":   string(model.RuleTypeTunnel),
			"status = ?": string(model.RuleStatusRunning),
		},
	})
	if err != nil {
		logger.Warnf("获取隧道规则失败: %v", err)
		return
	}

	for i := range rules {
		ruleID := rules[i].ID
		s.failoverOne(ruleID)
	}
}

// failoverOne 对单条规则执行一次故障转移检查与切换。
// 通过 tryLockRule 串行化，避免与用户手动操作（启动/停止/切换/删除）并发；
// 若该规则正被其他操作处理，则本次跳过，等待下一轮（5s）重试。
func (s *RuleService) failoverOne(ruleID uint) {
	unlock, ok := s.tryLockRule(ruleID)
	if !ok {
		return
	}
	defer unlock()

	// 持锁后重新加载，确保基于最新状态决策（手动操作可能刚刚修改过该规则）。
	rule, err := s.ruleRepo.FindByID(ruleID)
	if err != nil {
		return
	}
	// 仅处理仍在运行中的隧道规则。
	if rule.Type != model.RuleTypeTunnel || rule.Status != model.RuleStatusRunning {
		return
	}

	selectedTunnel, err := s.selectAvailableTunnel(rule)
	if err != nil {
		return
	}
	if rule.TunnelID != nil && *rule.TunnelID == selectedTunnel.ID {
		return
	}

	if rule.TunnelID != nil && rule.PrimaryTunnelID != nil && *rule.TunnelID != *rule.PrimaryTunnelID {
		// 当前在备选上运行 → 切到了更优先的链路（主链路或更靠前的备选）
		logger.Infof("[Failover] 规则 %d (%s) 切换隧道: %d -> %d（原主链路: %d）",
			rule.ID, rule.Name, *rule.TunnelID, selectedTunnel.ID, *rule.PrimaryTunnelID)
	} else {
		logger.Infof("[Failover] 规则 %d (%s) 切换隧道: %v -> %d", rule.ID, rule.Name, rule.TunnelID, selectedTunnel.ID)
	}
	// 只更新 tunnel_id，不修改 primary_tunnel_id，保留用户的原始主链路选择
	_ = s.stopCore(rule.ID, 0, "system", "", "")
	_ = s.ruleRepo.UpdateFields(&model.GostRule{}, rule.ID, map[string]any{"tunnel_id": selectedTunnel.ID})
	if err := s.startCore(rule.ID, 0, "system", "", ""); err != nil {
		logger.Warnf("[Failover] 规则 %d (%s) 切换后启动失败: %v", rule.ID, rule.Name, err)
	}
}

// startForwardRule starts a direct forward rule.
func (s *RuleService) startForwardRule(rule *model.GostRule, client *gost.Client, serviceName string) error {
	return s.buildAndStartService(client, rule, serviceName, "")
}

// startTunnelRule starts a tunnel-based rule.
func (s *RuleService) startTunnelRule(rule *model.GostRule, client *gost.Client, serviceName string) error {
	if rule.TunnelID == nil {
		return errors.ErrTunnelRequired
	}

	tunnel, err := s.tunnelRepo.FindByID(*rule.TunnelID)
	if err != nil {
		return errors.ErrTunnelNotFound
	}

	if tunnel.Status != model.TunnelStatusRunning {
		return errors.ErrTunnelNotRunning
	}

	if tunnel.ChainID == "" {
		_ = s.ruleRepo.UpdateStatus(rule.ID, model.RuleStatusError)
		return errors.ErrTunnelChainNotFound
	}

	return s.buildAndStartService(client, rule, serviceName, tunnel.ChainID)
}

// Stop stops a rule.
func (s *RuleService) Stop(id uint, userID uint, username string, ip, userAgent string) error {
	unlock := s.lockRule(id)
	defer unlock()
	return s.stopCore(id, userID, username, ip, userAgent)
}

// stopCore 停止规则的无锁核心实现。调用方必须已持有该规则的事务锁。
func (s *RuleService) stopCore(id uint, userID uint, username string, ip, userAgent string) error {
	rule, err := s.ruleRepo.FindByID(id)
	if err != nil {
		return err
	}

	if rule.Status != model.RuleStatusRunning {
		return nil
	}

	entryNodeID := s.getEntryNodeID(rule)
	node, err := s.nodeRepo.FindByID(entryNodeID)
	if err != nil {
		_ = s.ruleRepo.UpdateStatus(id, model.RuleStatusStopped)
		return nil
	}

	if node.Status == model.NodeStatusOffline {
		_ = s.ruleRepo.UpdateStatus(id, model.RuleStatusStopped)
		return nil
	}

	client := utils.GetGostClient(node)

	serviceID := rule.ServiceID
	if serviceID == "" {
		serviceID = fmt.Sprintf("rule-%d", rule.ID)
	}

	serviceIDs := []string{serviceID}
	if !strings.HasSuffix(serviceID, "-tcp") && !strings.HasSuffix(serviceID, "-udp") {
		serviceIDs = append(serviceIDs, serviceID+"-tcp", serviceID+"-udp")
	}

	deleteSucceeded := true
	for _, serviceName := range serviceIDs {
		if err = client.DeleteService(serviceName); err != nil {
			deleteSucceeded = false
			logger.Warnf("删除 Gost 服务失败: %v", err)
		}
	}

	if err = client.SaveConfig(); err != nil {
		deleteSucceeded = false
		logger.Warnf("保存 Gost 配置失败: %v", err)
	}

	if deleteSucceeded {
		_ = s.ruleRepo.ResetStatsCheckpoint(id)
	}
	_ = s.ruleRepo.UpdateStatus(id, model.RuleStatusStopped)

	s.logService.Record(
		userID,
		username,
		model.ActionStop,
		model.ResourceTypeRule,
		id,
		fmt.Sprintf("停止规则: %s", rule.Name),
		ip,
		userAgent,
	)

	logger.Infof("停止规则成功: %s", rule.Name)
	return nil
}

func (s *RuleService) getEntryNodeID(rule *model.GostRule) uint {
	if rule.Type == model.RuleTypeTunnel && rule.TunnelID != nil {
		nodeID, err := s.tunnelService.GetEntryNodeID(*rule.TunnelID)
		if err == nil {
			return nodeID
		}
	}
	if rule.NodeID != nil {
		return *rule.NodeID
	}
	return 0
}

func (s *RuleService) normalizeBackupTunnelIDs(primaryID *uint, backupIDs []uint) ([]uint, error) {
	seen := make(map[uint]struct{})
	if primaryID != nil && *primaryID > 0 {
		seen[*primaryID] = struct{}{}
	}

	result := make([]uint, 0, len(backupIDs))
	for _, backupID := range backupIDs {
		if backupID == 0 {
			continue
		}
		if _, ok := seen[backupID]; ok {
			continue
		}
		if _, err := s.tunnelRepo.FindByID(backupID); err != nil {
			if stderrors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errors.ErrTunnelNotFound
			}
			return nil, err
		}
		seen[backupID] = struct{}{}
		result = append(result, backupID)
	}

	return result, nil
}

func (s *RuleService) selectAvailableTunnel(rule *model.GostRule) (*model.GostTunnel, error) {
	// 优先级：① 用户指定的主链路 → ② 当前生效链路（若与主链路不同）→ ③ 备选列表
	// 这样当主链路恢复时，下次巡检会自动切回主链路。
	seen := make(map[uint]bool)

	var candidates []*uint
	if rule.PrimaryTunnelID != nil {
		candidates = append(candidates, rule.PrimaryTunnelID)
		seen[*rule.PrimaryTunnelID] = true
	}
	if rule.TunnelID != nil && !seen[*rule.TunnelID] {
		candidates = append(candidates, rule.TunnelID)
		seen[*rule.TunnelID] = true
	}
	for i := range rule.BackupTunnelIDs {
		id := rule.BackupTunnelIDs[i]
		if !seen[id] {
			candidates = append(candidates, &id)
			seen[id] = true
		}
	}

	for _, tunnelID := range candidates {
		tunnel, err := s.tunnelRepo.FindByID(*tunnelID)
		if err != nil {
			continue
		}
		if s.isTunnelUsable(tunnel) {
			return tunnel, nil
		}
	}

	return nil, errors.ErrTunnelFailoverUnavailable
}

func (s *RuleService) isTunnelUsable(tunnel *model.GostTunnel) bool {
	if tunnel == nil || tunnel.Status != model.TunnelStatusRunning || tunnel.ChainID == "" {
		return false
	}
	entryNode, err := s.nodeRepo.FindByID(tunnel.EntryNodeID)
	if err != nil {
		return false
	}
	return entryNode.Status != model.NodeStatusOffline
}

// setupRuleObserver configures the rule observer.
func (s *RuleService) setupRuleObserver(client *gost.Client, rule *model.GostRule, svc *gost.ServiceConfig) error {
	observerName, err := EnsureGlobalObserver(client, s.sysRepo)
	if err != nil {
		return err
	}

	_ = s.ruleRepo.UpdateObserverID(rule.ID, observerName)

	if observerName != "" {
		svc.Observer = observerName
		if svc.Metadata == nil {
			svc.Metadata = make(map[string]any)
		}
		svc.Metadata["enableStats"] = true
		svc.Metadata["observer.period"] = "5s"
		svc.Metadata["observer.resetTraffic"] = false
	}
	return nil
}

// buildAndStartService builds and starts a Gost service.
func (s *RuleService) buildAndStartService(client *gost.Client, rule *model.GostRule, serviceName string, chainID string) error {
	targets := rule.Targets
	strategy := rule.Strategy
	if strategy == "" || len(targets) == 1 {
		strategy = "round"
	}

	// 先清理同名旧服务，确保启动幂等：
	// gost 的 CreateService 对已存在的同名服务会直接跳过，
	// 若不先删除，切换隧道时会沿用旧链路（切换无效），
	// 失败回滚时也会因端口/服务残留而无法重新拉起。
	s.deleteRuleServices(client, serviceName)

	services := gost.BuildFullForwardService(serviceName, rule.ListenPort, targets, strategy)

	if chainID != "" {
		for _, svc := range services {
			svc.Handler.Chain = chainID
		}
	}

	for _, svc := range services {
		if err := s.setupRuleObserver(client, rule, svc); err != nil {
			s.deleteRuleServices(client, serviceName)
			_ = client.SaveConfig()
			return err
		}
		if err := client.CreateService(svc); err != nil {
			// 清理本次可能已部分创建的服务，避免端口残留导致后续启动/回滚失败
			s.deleteRuleServices(client, serviceName)
			_ = client.SaveConfig()
			_ = s.ruleRepo.UpdateStatus(rule.ID, model.RuleStatusError)
			return errors.ErrRuleStartFailed
		}
	}

	_ = client.SaveConfig()
	_ = s.ruleRepo.UpdateStatus(rule.ID, model.RuleStatusRunning)
	_ = s.ruleRepo.UpdateServiceID(rule.ID, serviceName)

	return nil
}

// deleteRuleServices 删除某条规则在节点上的全部 gost 服务（含 -tcp/-udp 变体）。
func (s *RuleService) deleteRuleServices(client *gost.Client, serviceName string) {
	names := []string{serviceName}
	if !strings.HasSuffix(serviceName, "-tcp") && !strings.HasSuffix(serviceName, "-udp") {
		names = append(names, serviceName+"-tcp", serviceName+"-udp")
	}
	for _, name := range names {
		if err := client.DeleteService(name); err != nil {
			logger.Warnf("清理 Gost 服务 %s 失败: %v", name, err)
		}
	}
}
