package service

import (
	stderrors "errors"
	"fmt"
	"strings"

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

// lockRule 获取指定规则的事务锁（包级别，见 locks.go），返回解锁函数。
// 串行化该规则的启动/停止/切换/故障转移/看门狗恢复，避免后台任务与用户手动操作
// 并发修改同一规则时产生的状态错乱。
func (s *RuleService) lockRule(id uint) func() {
	return lockByID(&ruleLocks, id)
}

// tryLockRule 尝试获取规则事务锁，成功返回解锁函数与 true；
// 若锁被占用（说明该规则正被其他操作处理）则返回 false，调用方应跳过本次操作。
func (s *RuleService) tryLockRule(id uint) (func(), bool) {
	return tryLockByID(&ruleLocks, id)
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

	// error 同样表示“应当运行”（看门狗正在重试），编辑后要按新配置重新拉起。
	prevStatus := rule.Status
	wasRunning := prevStatus.WantsRunning()
	if wasRunning {
		// 必须先在节点上删掉旧服务。入口节点离线时删不掉：旧配置会留在节点上继续转发，
		// 看门狗还会把它当成已恢复，新配置永远不会生效，所以直接拒绝。
		node, err := s.nodeRepo.FindByID(s.getEntryNodeID(rule))
		if err != nil {
			return nil, errors.ErrNodeNotFound
		}
		if node.Status == model.NodeStatusOffline {
			return nil, errors.ErrNodeOffline
		}
	}
	// 异常中的规则不校验目标隧道是否可用：它本就在等隧道恢复，校验会让改名之类的编辑也无法保存
	if rule.Type == model.RuleTypeTunnel && prevStatus == model.RuleStatusRunning {
		if err = s.validateTunnelSwitchTarget(req.TunnelID); err != nil {
			return nil, err
		}
	}
	var backupTunnelIDs []uint
	if rule.Type == model.RuleTypeTunnel {
		backupTunnelIDs, err = s.normalizeBackupTunnelIDs(req.TunnelID, req.BackupTunnelIDs)
		if err != nil {
			return nil, err
		}
	}

	if wasRunning {
		if err = s.stopCore(id, userID, username, ip, userAgent); err != nil {
			logger.Warnf("更新规则前停止失败: %v", err)
			return nil, err
		}
		// stopCore 删不掉服务时只记日志、照样把状态写成 stopped：对“停止”来说残留交给看门狗清理即可，
		// 对“编辑”不行 —— 旧服务会继续按旧端口/旧目标转发，看门狗还会把它当成新配置已经生效
		if err = s.ensureRuleServicesRemoved(rule); err != nil {
			_ = s.ruleRepo.UpdateStatus(id, prevStatus)
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

			if prevStatus == model.RuleStatusError {
				// 原本就在等待恢复的规则：保留新配置并维持 error，交给看门狗继续重试
				_ = s.ruleRepo.UpdateStatus(id, model.RuleStatusError)
				rule.Status = model.RuleStatusError
			} else {
				rollbackRule := prevRule
				rollbackRule.Status = model.RuleStatusStopped
				if rbErr := s.ruleRepo.UpdateConfig(&rollbackRule); rbErr != nil {
					logger.Errorf("更新失败后回滚规则配置失败: %v", rbErr)
					_ = s.ruleRepo.UpdateStatus(id, model.RuleStatusError)
					return nil, err
				}
				if restartErr := s.startCore(id, userID, username, ip, userAgent); restartErr != nil {
					logger.Errorf("更新失败后恢复旧规则启动失败: %v", restartErr)
					// 用户只是想改配置，不是想停掉它：记为 error 保留“应当运行”的意图，
					// 否则看门狗会按 stopped 把它当残留清掉
					_ = s.ruleRepo.UpdateStatus(id, model.RuleStatusError)
				}
				return nil, err
			}
		} else {
			rule.Status = model.RuleStatusRunning
		}
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

	if !s.isTunnelUsable(tunnel) {
		return errors.ErrTunnelFailoverUnavailable
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

	if rule.Status.WantsRunning() {
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
		Conditions: make(map[string]any),
	}
	if req.NodeID == 0 {
		opt.Pagination = &repository.Pagination{
			Page:     req.Page,
			PageSize: req.PageSize,
		}
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

	rules, total, err := s.ruleRepo.List(opt)
	if err != nil || req.NodeID == 0 {
		return rules, total, err
	}

	filtered := make([]model.GostRule, 0, len(rules))
	for i := range rules {
		if s.ruleUsesRuntimeNode(&rules[i], req.NodeID) {
			filtered = append(filtered, rules[i])
		}
	}
	total = int64(len(filtered))
	start := (req.Page - 1) * req.PageSize
	if start >= len(filtered) {
		return []model.GostRule{}, total, nil
	}
	end := start + req.PageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	return filtered[start:end], total, nil
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
		candidates := s.availableTunnels(rule)
		if len(candidates) == 0 {
			return errors.ErrTunnelFailoverUnavailable
		}
		// 与故障转移一致：按优先级取第一条链路确实在入口节点上的隧道。
		// 只看数据库会选中“运行中”但链路已丢的隧道，启动失败、还把规则挪了过去
		selectedTunnel := s.firstRoutableTunnel(candidates)
		if selectedTunnel == nil {
			return errors.ErrTunnelChainNotFound
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

	candidates := s.availableTunnels(rule)
	if len(candidates) == 0 || (rule.TunnelID != nil && *rule.TunnelID == candidates[0].ID) {
		return
	}
	// 要切换了：先确认目标隧道的链路确实在入口节点上。否则拆掉正常工作的服务之后，
	// 新服务会挂在不存在的链路上 —— 链路缺失时 GOST 会让流量绕过隧道直连目标
	selectedTunnel := s.firstRoutableTunnel(candidates)
	if selectedTunnel == nil || (rule.TunnelID != nil && *rule.TunnelID == selectedTunnel.ID) {
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
		// stopCore 已把状态写成 stopped；这里改回 error，保留“应当运行”的意图，
		// 交给看门狗继续重试，而不是让一次切换失败把规则永久停掉。
		_ = s.ruleRepo.UpdateStatus(rule.ID, model.RuleStatusError)
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

	// 链不在入口节点上时，GOST 会让流量直连目标、绕过隧道，宁可启动失败也不能下发
	if ok, err := client.ChainExists(tunnel.ChainID); err != nil || !ok {
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

	// error 也要能停：它表示看门狗仍在重试恢复，用户点“停止”就是要终止重试并清理节点上的残留
	if !rule.Status.WantsRunning() {
		return nil
	}

	entryNodeID := s.getEntryNodeID(rule)
	node, err := s.nodeRepo.FindByID(entryNodeID)
	if err != nil {
		_ = s.ruleRepo.UpdateStatus(id, model.RuleStatusStopped)
		return nil
	}

	if node.Status == model.NodeStatusOffline {
		// 节点离线时删不掉服务；它重新上线后，看门狗会按 stopped 清理残留
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

func (s *RuleService) ruleUsesRuntimeNode(rule *model.GostRule, nodeID uint) bool {
	if rule == nil || nodeID == 0 {
		return false
	}
	if rule.Type == model.RuleTypeTunnel {
		if rule.Tunnel != nil && rule.Tunnel.EntryNodeID == nodeID {
			return true
		}
		if rule.TunnelID != nil {
			entryNodeID, err := s.tunnelService.GetEntryNodeID(*rule.TunnelID)
			return err == nil && entryNodeID == nodeID
		}
		return false
	}
	return rule.NodeID != nil && *rule.NodeID == nodeID
}

func (s *RuleService) normalizeBackupTunnelIDs(primaryID *uint, backupIDs []uint) ([]uint, error) {
	seen := make(map[uint]struct{})
	var primaryEntryNodeID uint
	if primaryID != nil && *primaryID > 0 {
		seen[*primaryID] = struct{}{}
		primaryTunnel, err := s.tunnelRepo.FindByID(*primaryID)
		if err != nil {
			if stderrors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errors.ErrTunnelNotFound
			}
			return nil, err
		}
		primaryEntryNodeID = primaryTunnel.EntryNodeID
	}

	result := make([]uint, 0, len(backupIDs))
	for _, backupID := range backupIDs {
		if backupID == 0 {
			continue
		}
		if _, ok := seen[backupID]; ok {
			continue
		}
		backupTunnel, err := s.tunnelRepo.FindByID(backupID)
		if err != nil {
			if stderrors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errors.ErrTunnelNotFound
			}
			return nil, err
		}
		if primaryEntryNodeID > 0 && backupTunnel.EntryNodeID != primaryEntryNodeID {
			return nil, errors.ErrTunnelEntryMismatch
		}
		seen[backupID] = struct{}{}
		result = append(result, backupID)
	}

	return result, nil
}

func (s *RuleService) selectAvailableTunnel(rule *model.GostRule) (*model.GostTunnel, error) {
	tunnels := s.availableTunnels(rule)
	if len(tunnels) == 0 {
		return nil, errors.ErrTunnelFailoverUnavailable
	}
	return tunnels[0], nil
}

// firstRoutableTunnel 返回候选中第一条链路确实存在于入口节点上的隧道（候选共用同一入口节点）。
// 数据库里“运行中”不代表节点上的链路还在（例如入口节点刚重启、看门狗尚未补建），
// 而规则挂到不存在的链路上时，GOST 会让流量绕过隧道直连目标。
func (s *RuleService) firstRoutableTunnel(candidates []*model.GostTunnel) *model.GostTunnel {
	if len(candidates) == 0 {
		return nil
	}
	node, err := s.nodeRepo.FindByID(candidates[0].EntryNodeID)
	if err != nil {
		return nil
	}
	client := utils.GetGostClient(node)
	for _, tunnel := range candidates {
		if ok, err := client.ChainExists(tunnel.ChainID); err == nil && ok {
			return tunnel
		}
	}
	return nil
}

// availableTunnels 按优先级返回规则当前可用的全部隧道。
func (s *RuleService) availableTunnels(rule *model.GostRule) []*model.GostTunnel {
	// 优先级：① 用户指定的主链路 → ② 当前生效链路（若与主链路不同）→ ③ 备选列表
	// 这样当主链路恢复时，下次巡检会自动切回主链路。
	seen := make(map[uint]bool)

	var expectedEntryNodeID uint
	var candidates []*uint
	if rule.PrimaryTunnelID != nil {
		candidates = append(candidates, rule.PrimaryTunnelID)
		seen[*rule.PrimaryTunnelID] = true
		if primary, err := s.tunnelRepo.FindByID(*rule.PrimaryTunnelID); err == nil {
			expectedEntryNodeID = primary.EntryNodeID
		}
	}
	if expectedEntryNodeID == 0 && rule.TunnelID != nil {
		if current, err := s.tunnelRepo.FindByID(*rule.TunnelID); err == nil {
			expectedEntryNodeID = current.EntryNodeID
		}
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

	var usable []*model.GostTunnel
	for _, tunnelID := range candidates {
		tunnel, err := s.tunnelRepo.FindByID(*tunnelID)
		if err != nil {
			continue
		}
		if expectedEntryNodeID > 0 && tunnel.EntryNodeID != expectedEntryNodeID {
			continue
		}
		if s.isTunnelUsable(tunnel) {
			usable = append(usable, tunnel)
		}
	}
	return usable
}

func (s *RuleService) isTunnelUsable(tunnel *model.GostTunnel) bool {
	if tunnel == nil || tunnel.Status != model.TunnelStatusRunning || tunnel.ChainID == "" {
		return false
	}
	entryNode, err := s.nodeRepo.FindByID(tunnel.EntryNodeID)
	if err != nil {
		return false
	}
	if entryNode.Status == model.NodeStatusOffline {
		return false
	}

	for _, hop := range tunnel.EffectiveHops() {
		node, err := s.nodeRepo.FindByID(hop.NodeID)
		if err != nil || node.Status == model.NodeStatusOffline {
			return false
		}
	}
	return true
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

// buildRuleServices 构建规则在入口节点上的 gost 服务定义（TCP+UDP 两个子服务），
// 并挂上隧道链路与流量观察器。启动与看门狗恢复共用，保证两条路径下发的配置一致。
func (s *RuleService) buildRuleServices(client *gost.Client, rule *model.GostRule, serviceName string, chainID string) ([]*gost.ServiceConfig, error) {
	targets := rule.Targets
	strategy := rule.Strategy
	if strategy == "" || len(targets) == 1 {
		strategy = "round"
	}

	services := gost.BuildFullForwardService(serviceName, rule.ListenPort, targets, strategy)
	for _, svc := range services {
		if chainID != "" {
			svc.Handler.Chain = chainID
		}
		if err := s.setupRuleObserver(client, rule, svc); err != nil {
			return nil, err
		}
	}
	return services, nil
}

// buildAndStartService builds and starts a Gost service.
func (s *RuleService) buildAndStartService(client *gost.Client, rule *model.GostRule, serviceName string, chainID string) error {
	// 先清理同名旧服务，确保启动幂等：
	// gost 的 CreateService 对已存在的同名服务会直接跳过，
	// 若不先删除，切换隧道时会沿用旧链路（切换无效），
	// 失败回滚时也会因端口/服务残留而无法重新拉起。
	// 删不掉就必须中止：继续往下走，创建会因旧服务还在而被跳过，
	// 旧服务连同它引用的旧链路原样保留，切换看似成功实则没有生效。
	if err := s.deleteRuleServices(client, serviceName); err != nil {
		return errors.WithDetail(errors.ErrRuleStartFailed, "清理旧服务失败: "+err.Error())
	}

	services, err := s.buildRuleServices(client, rule, serviceName, chainID)
	if err != nil {
		_ = client.SaveConfig()
		return err
	}

	for _, svc := range services {
		if err := client.CreateService(svc); err != nil {
			// 清理本次可能已部分创建的服务，避免端口残留导致后续启动/回滚失败
			_ = s.deleteRuleServices(client, serviceName)
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

// restoreRuleServices 供看门狗使用：只重建缺失、已失败或挂错链路的子服务，健康的子服务保持不动。
//
// 不能复用 buildAndStartService 的“先全删再全建”：例如 UDP 子服务因端口被占而反复失败时，
// 每次重试都连带重建 TCP 子服务，会周期性地打断 TCP 上的存量连接。
// 各子服务独立处理，一个失败不影响另一个；返回本次实际重建的服务名与遇到的第一个错误。
func (s *RuleService) restoreRuleServices(client *gost.Client, rule *model.GostRule, chainID string, snap *nodeSnapshot) ([]string, error) {
	serviceName := fmt.Sprintf("rule-%d", rule.ID)
	var (
		restored []string
		firstErr error
	)
	// 引用了不存在 chain 的子服务正在绕过隧道直连目标，先删掉，不能让它等后面构建配置
	// （构建要下发观察器，节点抖动或未配置面板地址时会失败）
	removed := make(map[string]bool)
	for _, name := range []string{serviceName, serviceName + "-tcp", serviceName + "-udp"} {
		if !snap.leaks(name) {
			continue
		}
		if err := client.DeleteService(name); err != nil {
			firstErr = err
			continue
		}
		removed[name] = true
	}

	services, err := s.buildRuleServices(client, rule, serviceName, chainID)
	if err != nil {
		if len(removed) > 0 {
			_ = client.SaveConfig()
		}
		return nil, err
	}

	// 不带协议后缀的同名服务不属于现在的 TCP+UDP 结构，会和子服务抢端口，先删掉
	if _, ok := snap.services[serviceName]; ok && !removed[serviceName] {
		if err = client.DeleteService(serviceName); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, svc := range services {
		if snap.serviceHealthy(svc.Name, chainID) {
			continue
		}
		if _, exists := snap.services[svc.Name]; exists && !removed[svc.Name] {
			// 删不掉就不能再创建：创建会因旧服务还在而被跳过，旧配置原样保留
			if err = client.DeleteService(svc.Name); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		if err = client.CreateService(svc); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		restored = append(restored, svc.Name)
	}

	if len(restored) > 0 {
		_ = client.SaveConfig()
		_ = s.ruleRepo.UpdateServiceID(rule.ID, serviceName)
	}
	return restored, firstErr
}

// ensureRuleServicesRemoved 确认规则在入口节点上的服务都已删除；仍存在或无法确认时返回错误
func (s *RuleService) ensureRuleServicesRemoved(rule *model.GostRule) error {
	node, err := s.nodeRepo.FindByID(s.getEntryNodeID(rule))
	if err != nil {
		return errors.ErrNodeNotFound
	}
	client := utils.GetGostClient(node)
	base := fmt.Sprintf("rule-%d", rule.ID)
	for _, name := range []string{base, base + "-tcp", base + "-udp"} {
		exists, err := client.ServiceExists(name)
		if err != nil || exists {
			return errors.WithDetail(errors.ErrOperationFailed, "节点上的旧服务未能删除，请稍后重试")
		}
	}
	return nil
}

// deleteRuleServices 删除某条规则在节点上的全部 gost 服务（含 -tcp/-udp 变体），返回遇到的第一个错误。
func (s *RuleService) deleteRuleServices(client *gost.Client, serviceName string) error {
	names := []string{serviceName}
	if !strings.HasSuffix(serviceName, "-tcp") && !strings.HasSuffix(serviceName, "-udp") {
		names = append(names, serviceName+"-tcp", serviceName+"-udp")
	}
	var firstErr error
	for _, name := range names {
		if err := client.DeleteService(name); err != nil {
			logger.Warnf("清理 Gost 服务 %s 失败: %v", name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
