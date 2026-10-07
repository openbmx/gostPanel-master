package repository

import (
	"fmt"
	"gost-panel/internal/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TunnelRepository 隧道仓库
type TunnelRepository struct {
	*BaseRepository
}

// NewTunnelRepository 创建隧道仓库
func NewTunnelRepository(db *gorm.DB) *TunnelRepository {
	return &TunnelRepository{
		BaseRepository: NewBaseRepository(db),
	}
}

// Create 创建隧道
func (r *TunnelRepository) Create(tunnel *model.GostTunnel) error {
	return r.DB.Create(tunnel).Error
}

// Update 更新隧道
// 注意：必须 Omit 关联，否则 GORM 会因 belongs-to 关联（EntryNode/ExitNode）的自动保存，
// 用预加载的旧关联对象主键反写回外键（entry_node_id/exit_node_id），导致修改 hops 切换出口节点后
// exit_node_id 仍指向旧节点：流量统计记到旧出口节点，旧出口节点也因“仍被使用”而无法删除。
func (r *TunnelRepository) Update(tunnel *model.GostTunnel) error {
	return r.DB.Omit(clause.Associations).Save(tunnel).Error
}

// SyncExitNodeWithLastHop 修复历史数据：让 exit_node_id 与 hops 的最后一跳一致，返回校正的条数。
//
// Update 修复之前，编辑隧道换出口节点会被关联回写成旧节点（见上）。新的编辑不会再出错，
// 但已经写坏的记录不会自己恢复：列表里的“出口”、流量统计的归属、删除旧节点时的占用检查
// 都还在用错误的值。启动时按 hops 校正一次，结果幂等。
func (r *TunnelRepository) SyncExitNodeWithLastHop() (int, error) {
	var tunnels []model.GostTunnel
	if err := r.DB.Find(&tunnels).Error; err != nil {
		return 0, err
	}
	fixed := 0
	for _, t := range tunnels {
		if len(t.Hops) == 0 {
			continue // 旧版单跳隧道没有 hops，出口就是 exit_node_id 本身
		}
		hops := t.EffectiveHops()
		if len(hops) == 0 {
			continue
		}
		last := hops[len(hops)-1].NodeID
		if t.ExitNodeID == last {
			continue
		}
		// 只改这一列，不碰流量统计等其它字段，也不刷新 updated_at
		if err := r.DB.Model(&model.GostTunnel{}).Where("id = ?", t.ID).UpdateColumn("exit_node_id", last).Error; err != nil {
			return fixed, err
		}
		fixed++
	}
	return fixed, nil
}

// Delete 删除隧道
func (r *TunnelRepository) Delete(id uint) error {
	return r.DB.Delete(&model.GostTunnel{}, id).Error
}

// FindByID 根据 ID 查询隧道（包含关联节点）
func (r *TunnelRepository) FindByID(id uint) (*model.GostTunnel, error) {
	var tunnel model.GostTunnel
	err := r.DB.Preload("EntryNode").Preload("ExitNode").First(&tunnel, id).Error
	if err != nil {
		return nil, err
	}
	return &tunnel, nil
}

// List 查询隧道列表
func (r *TunnelRepository) List(opt *QueryOption) ([]model.GostTunnel, int64, error) {
	var tunnels []model.GostTunnel
	var total int64

	db := r.DB.Model(&model.GostTunnel{})

	// 应用条件过滤
	db = ApplyConditions(db, opt)

	// 统计总数（包含过滤条件）
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 预加载节点和关联规则
	db = db.Preload("EntryNode").Preload("ExitNode")

	// 默认按创建时间倒序
	if opt == nil || len(opt.Orders) == 0 {
		db = db.Order("created_at DESC")
	}

	// 应用分页
	db = ApplyPagination(db, opt)

	if err := db.Find(&tunnels).Error; err != nil {
		return nil, 0, err
	}

	return tunnels, total, nil
}

// ListDeletedIDs 返回已删除（软删除）的隧道 ID，供看门狗识别并清理节点上的残留 relay/chain
func (r *TunnelRepository) ListDeletedIDs() ([]uint, error) {
	var ids []uint
	err := r.DB.Unscoped().Model(&model.GostTunnel{}).Where("deleted_at IS NOT NULL").Pluck("id", &ids).Error
	return ids, err
}

// IsDeleted 判断隧道是否已被删除（软删除）；从未存在过的 ID 返回 false
func (r *TunnelRepository) IsDeleted(id uint) (bool, error) {
	var count int64
	err := r.DB.Unscoped().Model(&model.GostTunnel{}).Where("id = ? AND deleted_at IS NOT NULL", id).Count(&count).Error
	return count > 0, err
}

// UpdateStatus 更新隧道状态
func (r *TunnelRepository) UpdateStatus(id uint, status model.TunnelStatus) error {
	return r.UpdateField(&model.GostTunnel{}, id, "status", status)
}

// CompareAndSetStatus 仅当当前状态仍为 from 时才改为 to，返回是否生效。
// 不持锁的后台任务（看门狗）校正状态时必须用它，否则会覆盖用户刚刚做出的停止/编辑。
func (r *TunnelRepository) CompareAndSetStatus(id uint, from, to model.TunnelStatus) (bool, error) {
	res := r.DB.Model(&model.GostTunnel{}).Where("id = ? AND status = ?", id, from).Update("status", to)
	return res.RowsAffected > 0, res.Error
}

// CountAll 统计总数
func (r *TunnelRepository) CountAll() (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostTunnel{}).Count(&count).Error
	return count, err
}

// FindByNodeID 查找节点相关的隧道
func (r *TunnelRepository) FindByNodeID(nodeID uint) ([]model.GostTunnel, error) {
	var tunnels []model.GostTunnel
	if err := r.DB.Find(&tunnels).Error; err != nil {
		return nil, err
	}

	result := make([]model.GostTunnel, 0, len(tunnels))
	for _, tunnel := range tunnels {
		if tunnel.UsesNode(nodeID) {
			result = append(result, tunnel)
		}
	}
	return result, nil
}

// StopByNodeID 停止与该节点相关的所有隧道
func (r *TunnelRepository) StopByNodeID(nodeID uint) error {
	tunnels, err := r.FindByNodeID(nodeID)
	if err != nil {
		return err
	}

	ids := make([]uint, 0, len(tunnels))
	for _, tunnel := range tunnels {
		if tunnel.Status == model.TunnelStatusRunning {
			ids = append(ids, tunnel.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}

	return r.DB.Model(&model.GostTunnel{}).
		Where("id IN ?", ids).
		Update("status", model.TunnelStatusStopped).Error
}

// HasRulesUsingTunnel 检查是否有规则正在使用该隧道
func (r *TunnelRepository) HasRulesUsingTunnel(tunnelID uint) (bool, error) {
	var rules []model.GostRule
	if err := r.DB.Find(&rules).Error; err != nil {
		return false, err
	}

	for _, rule := range rules {
		if rule.TunnelID != nil && *rule.TunnelID == tunnelID {
			return true, nil
		}
		if rule.PrimaryTunnelID != nil && *rule.PrimaryTunnelID == tunnelID {
			return true, nil
		}
		for _, backupID := range rule.BackupTunnelIDs {
			if backupID == tunnelID {
				return true, nil
			}
		}
	}
	return false, nil
}

// HasRules 检查隧道是否被规则使用（别名方法）
func (r *TunnelRepository) HasRules(tunnelID uint) (bool, error) {
	return r.HasRulesUsingTunnel(tunnelID)
}

// CountByStatus 按状态统计
func (r *TunnelRepository) CountByStatus(status model.TunnelStatus) (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostTunnel{}).Where("status = ?", status).Count(&count).Error
	return count, err
}

// UpdateServiceInfo 更新隧道的服务 ID 和 Chain ID
func (r *TunnelRepository) UpdateServiceInfo(id uint, serviceID, chainID string) error {
	return r.DB.Model(&model.GostTunnel{}).Where("id = ?", id).
		Updates(map[string]any{
			"service_id": serviceID,
			"chain_id":   chainID,
		}).Error
}

// ResetStatsCheckpoint 重置隧道统计检查点（不清空已累计总量）
func (r *TunnelRepository) ResetStatsCheckpoint(id uint) error {
	return r.UpdateFields(&model.GostTunnel{}, id, map[string]any{
		"last_reported_input_bytes":  0,
		"last_reported_output_bytes": 0,
	})
}

// UpdateStats 更新隧道流量统计（计算增量）
// Gost observer 上报的是累计总量，需要计算增量后再累加。
// 这里使用乐观并发控制，避免同一条累计上报在并发/重试场景下被重复累计。
// 对于小于当前检查点的回退值，视为过期/乱序快照并忽略；
// 合法重启场景应通过显式 ResetStatsCheckpoint 将检查点归零。
// 返回本次增量值 (inputDelta, outputDelta)
func (r *TunnelRepository) UpdateStats(id uint, reportedInputBytes, reportedOutputBytes int64) (int64, int64, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var tunnel model.GostTunnel
		if err := r.DB.Select("id", "last_reported_input_bytes", "last_reported_output_bytes").
			Where("id = ?", id).First(&tunnel).Error; err != nil {
			return 0, 0, err
		}

		lastInput := tunnel.LastReportedInputBytes
		lastOutput := tunnel.LastReportedOutputBytes

		if reportedInputBytes == lastInput && reportedOutputBytes == lastOutput {
			return 0, 0, nil
		}

		// 累计模式下 reported < last 表示 Gost 计数器重置（进程/服务重启），
		// 按计数器重置处理：以本次上报值作为新的增量重新累计，避免流量永久冻结。
		inputDelta := reportedInputBytes - lastInput
		if inputDelta < 0 {
			inputDelta = reportedInputBytes
		}
		outputDelta := reportedOutputBytes - lastOutput
		if outputDelta < 0 {
			outputDelta = reportedOutputBytes
		}

		updates := map[string]any{
			"last_reported_input_bytes":  reportedInputBytes,
			"last_reported_output_bytes": reportedOutputBytes,
		}
		if inputDelta > 0 || outputDelta > 0 {
			updates["input_bytes"] = gorm.Expr("input_bytes + ?", inputDelta)
			updates["output_bytes"] = gorm.Expr("output_bytes + ?", outputDelta)
			updates["total_bytes"] = gorm.Expr("total_bytes + ?", inputDelta+outputDelta)
		}

		result := r.DB.Model(&model.GostTunnel{}).
			Where("id = ?", id).
			Where("last_reported_input_bytes = ? AND last_reported_output_bytes = ?", lastInput, lastOutput).
			Updates(updates)
		if result.Error != nil {
			return 0, 0, result.Error
		}
		if result.RowsAffected == 0 {
			continue
		}

		return inputDelta, outputDelta, nil
	}

	return 0, 0, fmt.Errorf("更新隧道统计失败: 并发冲突")
}
