package repository

import (
	"gost-panel/internal/model"
	"strings"

	"gorm.io/gorm"
)

// RuleRepository 规则仓库
type RuleRepository struct {
	*BaseRepository
}

// NewRuleRepository 创建规则仓库
func NewRuleRepository(db *gorm.DB) *RuleRepository {
	return &RuleRepository{
		BaseRepository: NewBaseRepository(db),
	}
}

// Create 创建规则
func (r *RuleRepository) Create(rule *model.GostRule) error {
	return r.DB.Create(rule).Error
}

// Update 更新规则
func (r *RuleRepository) Update(rule *model.GostRule) error {
	return r.DB.Save(rule).Error
}

// Delete 删除规则
func (r *RuleRepository) Delete(id uint) error {
	return r.DB.Delete(&model.GostRule{}, id).Error
}

// FindByID 根据 ID 查询规则
func (r *RuleRepository) FindByID(id uint) (*model.GostRule, error) {
	var rule model.GostRule
	err := r.DB.Preload("Node").Preload("Tunnel").Preload("Tunnel.EntryNode").Preload("Tunnel.ExitNode").First(&rule, id).Error
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// List 查询规则列表
func (r *RuleRepository) List(opt *QueryOption) ([]model.GostRule, int64, error) {
	var rules []model.GostRule
	var total int64

	db := r.DB.Model(&model.GostRule{})

	// 应用条件过滤
	db = ApplyConditions(db, opt)

	// 统计总数（包含过滤条件）
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 预加载节点和隧道
	db = db.Preload("Node").Preload("Tunnel").Preload("Tunnel.EntryNode").Preload("Tunnel.ExitNode")

	// 默认按创建时间倒序
	if opt == nil || len(opt.Orders) == 0 {
		db = db.Order("created_at DESC")
	}

	// 应用分页
	db = ApplyPagination(db, opt)

	if err := db.Find(&rules).Error; err != nil {
		return nil, 0, err
	}

	return rules, total, nil
}

// FindByNodeID 根据节点 ID 查询规则
func (r *RuleRepository) FindByNodeID(nodeID uint) ([]model.GostRule, error) {
	var rules []model.GostRule
	err := r.DB.Where("node_id = ?", nodeID).Find(&rules).Error
	return rules, err
}

// FindByTunnelID 根据隧道 ID 查询规则
func (r *RuleRepository) FindByTunnelID(tunnelID uint) ([]model.GostRule, error) {
	var rules []model.GostRule
	err := r.DB.Where("tunnel_id = ?", tunnelID).Find(&rules).Error
	return rules, err
}

// ExistsByPort 检查端口是否已被使用
func (r *RuleRepository) ExistsByPort(nodeID uint, port int, excludeID ...uint) (bool, error) {
	var count int64
	db := r.DB.Model(&model.GostRule{}).
		Where("node_id = ? AND listen_port = ?", nodeID, port)
	if len(excludeID) > 0 {
		db = db.Where("id != ?", excludeID[0])
	}
	err := db.Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// UpdateStatus 更新规则状态
func (r *RuleRepository) UpdateStatus(id uint, status model.RuleStatus) error {
	return r.UpdateField(&model.GostRule{}, id, "status", status)
}

// UpdateServiceID 更新服务 ID
func (r *RuleRepository) UpdateServiceID(id uint, serviceID string) error {
	return r.UpdateField(&model.GostRule{}, id, "service_id", serviceID)
}

// UpdateServiceInfo 更新服务信息 (包含 ChainID)
func (r *RuleRepository) UpdateServiceInfo(id uint, serviceID, chainID string) error {
	return r.UpdateFields(&model.GostRule{}, id, map[string]interface{}{
		"service_id": serviceID,
		"chain_id":   chainID,
	})
}

// UpdateObserverID 更新观察器 ID
func (r *RuleRepository) UpdateObserverID(id uint, observerID string) error {
	return r.UpdateField(&model.GostRule{}, id, "observer_id", observerID)
}

// CountByNodeID 按节点统计数量
func (r *RuleRepository) CountByNodeID(nodeID uint) (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostRule{}).Where("node_id = ?", nodeID).Count(&count).Error
	return count, err
}

// CountAll 统计总数
func (r *RuleRepository) CountAll() (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostRule{}).Count(&count).Error
	return count, err
}

// CountByStatus 按状态统计
func (r *RuleRepository) CountByStatus(status model.RuleStatus) (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostRule{}).Where("status = ?", status).Count(&count).Error
	return count, err
}

// CountByType 按类型统计
func (r *RuleRepository) CountByType(ruleType model.RuleType) (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostRule{}).Where("type = ?", ruleType).Count(&count).Error
	return count, err
}

// StopByNodeID 停止指定节点的所有规则
func (r *RuleRepository) StopByNodeID(nodeID uint) error {
	return r.DB.Model(&model.GostRule{}).
		Where("node_id = ? AND status = ?", nodeID, model.RuleStatusRunning).
		Update("status", model.RuleStatusStopped).Error
}

// UpdateStats 更新流量统计（计算增量）
// Gost observer 上报的是累计总量，需要计算增量后再累加
// 返回本次增量值 (inputDelta, outputDelta, connsDelta)
func (r *RuleRepository) UpdateStats(id uint, serviceName string, reportedInputBytes, reportedOutputBytes, reportedTotalConns int64) (int64, int64, int64, error) {
	// 选择对应协议的累计字段
	inputField := "last_reported_input_bytes"
	outputField := "last_reported_output_bytes"
	connsField := "last_reported_total_conns"

	if strings.HasSuffix(serviceName, "-tcp") {
		inputField = "last_reported_input_bytes_tcp"
		outputField = "last_reported_output_bytes_tcp"
		connsField = "last_reported_total_conns_tcp"
	} else if strings.HasSuffix(serviceName, "-udp") {
		inputField = "last_reported_input_bytes_udp"
		outputField = "last_reported_output_bytes_udp"
		connsField = "last_reported_total_conns_udp"
	}

	// 先查询当前值
	var rule model.GostRule
	if err := r.DB.Select(
		"id",
		inputField,
		outputField,
		connsField,
		"last_reported_input_bytes",
		"last_reported_output_bytes",
		"last_reported_total_conns",
	).
		Where("id = ?", id).First(&rule).Error; err != nil {
		return 0, 0, 0, err
	}

	var lastInput, lastOutput, lastConns int64
	switch {
	case strings.HasSuffix(serviceName, "-tcp"):
		lastInput = rule.LastReportedInputBytesTCP
		lastOutput = rule.LastReportedOutputBytesTCP
		lastConns = rule.LastReportedTotalConnsTCP
	case strings.HasSuffix(serviceName, "-udp"):
		lastInput = rule.LastReportedInputBytesUDP
		lastOutput = rule.LastReportedOutputBytesUDP
		lastConns = rule.LastReportedTotalConnsUDP
	default:
		lastInput = rule.LastReportedInputBytes
		lastOutput = rule.LastReportedOutputBytes
		lastConns = rule.LastReportedTotalConns
	}

	legacyHasData := rule.LastReportedInputBytes > 0 || rule.LastReportedOutputBytes > 0 || rule.LastReportedTotalConns > 0
	protocolIsNew := strings.HasSuffix(serviceName, "-tcp") || strings.HasSuffix(serviceName, "-udp")
	if protocolIsNew && lastInput == 0 && lastOutput == 0 && lastConns == 0 && legacyHasData {
		updates := map[string]interface{}{
			inputField:  reportedInputBytes,
			outputField: reportedOutputBytes,
			connsField:  reportedTotalConns,
		}
		if err := r.DB.Model(&model.GostRule{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return 0, 0, 0, err
		}
		return 0, 0, 0, nil
	}

	// 计算增量（如果是第一次上报或重启后，上报值可能小于上次值，此时重置为上报值）
	var inputDelta, outputDelta, connsDelta int64
	if reportedInputBytes >= lastInput {
		inputDelta = reportedInputBytes - lastInput
	} else {
		// Gost 重启后计数器重置，直接使用新值作为增量
		inputDelta = reportedInputBytes
	}

	if reportedOutputBytes >= lastOutput {
		outputDelta = reportedOutputBytes - lastOutput
	} else {
		outputDelta = reportedOutputBytes
	}

	if reportedTotalConns >= lastConns {
		connsDelta = reportedTotalConns - lastConns
	} else {
		connsDelta = reportedTotalConns
	}

	updates := map[string]interface{}{
		inputField:  reportedInputBytes,
		outputField: reportedOutputBytes,
		connsField:  reportedTotalConns,
	}

	// 只有增量大于0时才更新（避免无效更新）
	if inputDelta > 0 || outputDelta > 0 || connsDelta > 0 {
		updates["input_bytes"] = gorm.Expr("input_bytes + ?", inputDelta)
		updates["output_bytes"] = gorm.Expr("output_bytes + ?", outputDelta)
		updates["total_bytes"] = gorm.Expr("total_bytes + ?", inputDelta+outputDelta)
		updates["total_requests"] = gorm.Expr("total_requests + ?", connsDelta)
	}

	if err := r.DB.Model(&model.GostRule{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return 0, 0, 0, err
	}

	return inputDelta, outputDelta, connsDelta, nil
}

// StopByTunnelIDs 停止指定隧道列表关联的所有规则
func (r *RuleRepository) StopByTunnelIDs(tunnelIDs []uint) error {
	if len(tunnelIDs) == 0 {
		return nil
	}
	return r.DB.Model(&model.GostRule{}).
		Where("tunnel_id IN ? AND status = ?", tunnelIDs, model.RuleStatusRunning).
		Update("status", model.RuleStatusStopped).Error
}
