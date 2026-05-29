package repository

import (
	"fmt"
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

// FindByRuntimeNodeID 根据规则实际运行所在的入口节点查询规则
// 包含：
// 1. 直接绑定在该节点上的端口转发规则
// 2. 通过隧道在该节点入口运行的隧道转发规则
func (r *RuleRepository) FindByRuntimeNodeID(nodeID uint) ([]model.GostRule, error) {
	var rules []model.GostRule
	err := r.DB.Model(&model.GostRule{}).
		Distinct("rules.*").
		Joins("LEFT JOIN tunnels ON tunnels.id = rules.tunnel_id").
		Where("rules.node_id = ? OR tunnels.entry_node_id = ?", nodeID, nodeID).
		Find(&rules).Error
	return rules, err
}

// FindByTunnelID 根据隧道 ID 查询规则
func (r *RuleRepository) FindByTunnelID(tunnelID uint) ([]model.GostRule, error) {
	var rules []model.GostRule
	err := r.DB.Where("tunnel_id = ?", tunnelID).Find(&rules).Error
	return rules, err
}

// ExistsByPort 检查端口是否已被使用
// 这里使用“实际入口节点”语义：
// - 端口转发规则使用 rules.node_id
// - 隧道转发规则使用 tunnels.entry_node_id
func (r *RuleRepository) ExistsByPort(nodeID uint, port int, excludeID ...uint) (bool, error) {
	var count int64
	db := r.DB.Model(&model.GostRule{}).
		Joins("LEFT JOIN tunnels ON tunnels.id = rules.tunnel_id").
		Where("rules.listen_port = ?", port).
		Where("(rules.node_id = ? OR tunnels.entry_node_id = ?)", nodeID, nodeID)
	if len(excludeID) > 0 {
		db = db.Where("rules.id != ?", excludeID[0])
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

// ResetStatsCheckpoint 重置规则统计检查点（不清空已累计总量）
func (r *RuleRepository) ResetStatsCheckpoint(id uint) error {
	return r.UpdateFields(&model.GostRule{}, id, map[string]any{
		"last_reported_input_bytes":      0,
		"last_reported_output_bytes":     0,
		"last_reported_total_conns":      0,
		"last_reported_input_bytes_tcp":  0,
		"last_reported_output_bytes_tcp": 0,
		"last_reported_total_conns_tcp":  0,
		"last_reported_input_bytes_udp":  0,
		"last_reported_output_bytes_udp": 0,
		"last_reported_total_conns_udp":  0,
	})
}

// ResetStatsCheckpointByService 按服务维度重置规则统计检查点
func (r *RuleRepository) ResetStatsCheckpointByService(id uint, serviceName string) error {
	switch {
	case strings.HasSuffix(serviceName, "-tcp"):
		return r.UpdateFields(&model.GostRule{}, id, map[string]any{
			"last_reported_input_bytes_tcp":  0,
			"last_reported_output_bytes_tcp": 0,
			"last_reported_total_conns_tcp":  0,
		})
	case strings.HasSuffix(serviceName, "-udp"):
		return r.UpdateFields(&model.GostRule{}, id, map[string]any{
			"last_reported_input_bytes_udp":  0,
			"last_reported_output_bytes_udp": 0,
			"last_reported_total_conns_udp":  0,
		})
	default:
		return r.ResetStatsCheckpoint(id)
	}
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
// Gost observer 上报的是累计总量，需要计算增量后再累加。
// 这里使用乐观并发控制，避免同一条累计上报在并发/重试场景下被重复累计。
// 对于小于当前检查点的回退值，视为过期/乱序快照并忽略；
// 合法重启场景应通过显式 ResetStatsCheckpoint 将检查点归零。
// 返回本次增量值 (inputDelta, outputDelta, connsDelta)
func (r *RuleRepository) UpdateStats(id uint, serviceName string, reportedInputBytes, reportedOutputBytes, reportedTotalConns int64) (int64, int64, int64, error) {
	inputField := "last_reported_input_bytes"
	outputField := "last_reported_output_bytes"
	connsField := "last_reported_total_conns"
	protocolIsNew := false

	if strings.HasSuffix(serviceName, "-tcp") {
		inputField = "last_reported_input_bytes_tcp"
		outputField = "last_reported_output_bytes_tcp"
		connsField = "last_reported_total_conns_tcp"
		protocolIsNew = true
	} else if strings.HasSuffix(serviceName, "-udp") {
		inputField = "last_reported_input_bytes_udp"
		outputField = "last_reported_output_bytes_udp"
		connsField = "last_reported_total_conns_udp"
		protocolIsNew = true
	}

	for attempt := 0; attempt < 5; attempt++ {
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

		// 精确重复上报（累计值完全一致）直接去重，避免在并发/重试场景下重复累计。
		if reportedInputBytes == lastInput && reportedOutputBytes == lastOutput && reportedTotalConns == lastConns {
			return 0, 0, 0, nil
		}

		legacyHasData := rule.LastReportedInputBytes > 0 || rule.LastReportedOutputBytes > 0 || rule.LastReportedTotalConns > 0
		if protocolIsNew && lastInput == 0 && lastOutput == 0 && lastConns == 0 && legacyHasData {
			updates := map[string]interface{}{
				inputField:  reportedInputBytes,
				outputField: reportedOutputBytes,
				connsField:  reportedTotalConns,
			}
			result := r.DB.Model(&model.GostRule{}).
				Where("id = ?", id).
				Where(inputField+" = ? AND "+outputField+" = ? AND "+connsField+" = ?", lastInput, lastOutput, lastConns).
				Updates(updates)
			if result.Error != nil {
				return 0, 0, 0, result.Error
			}
			if result.RowsAffected == 0 {
				continue
			}
			return 0, 0, 0, nil
		}

		// Gost observer 工作在累计模式 (observer.resetTraffic=false)。
		// 当 Gost 进程或服务被重启时，累计计数器会归零，导致本次上报值小于检查点。
		// 这并非乱序快照，而是“计数器重置”，应把本次上报值作为新的增量重新开始累计，
		// 否则检查点会永远停留在旧的高位，规则流量将不再增长（历史 BUG：非计划重启后流量冻结）。
		inputDelta := reportedInputBytes - lastInput
		if inputDelta < 0 {
			inputDelta = reportedInputBytes
		}
		outputDelta := reportedOutputBytes - lastOutput
		if outputDelta < 0 {
			outputDelta = reportedOutputBytes
		}
		connsDelta := reportedTotalConns - lastConns
		if connsDelta < 0 {
			connsDelta = reportedTotalConns
		}

		updates := map[string]interface{}{
			inputField:  reportedInputBytes,
			outputField: reportedOutputBytes,
			connsField:  reportedTotalConns,
		}
		if inputDelta > 0 || outputDelta > 0 || connsDelta > 0 {
			updates["input_bytes"] = gorm.Expr("input_bytes + ?", inputDelta)
			updates["output_bytes"] = gorm.Expr("output_bytes + ?", outputDelta)
			updates["total_bytes"] = gorm.Expr("total_bytes + ?", inputDelta+outputDelta)
			updates["total_requests"] = gorm.Expr("total_requests + ?", connsDelta)
		}

		result := r.DB.Model(&model.GostRule{}).
			Where("id = ?", id).
			Where(inputField+" = ? AND "+outputField+" = ? AND "+connsField+" = ?", lastInput, lastOutput, lastConns).
			Updates(updates)
		if result.Error != nil {
			return 0, 0, 0, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}

		return inputDelta, outputDelta, connsDelta, nil
	}

	return 0, 0, 0, fmt.Errorf("更新规则统计失败: 并发冲突")
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
