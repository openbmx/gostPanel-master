package repository

import (
	"gost-panel/internal/model"
	"time"

	"gorm.io/gorm"
)

// NodeRepository 节点仓库
type NodeRepository struct {
	*BaseRepository
}

// NewNodeRepository 创建节点仓库
func NewNodeRepository(db *gorm.DB) *NodeRepository {
	return &NodeRepository{
		BaseRepository: NewBaseRepository(db),
	}
}

// Create 创建节点
func (r *NodeRepository) Create(node *model.GostNode) error {
	return r.DB.Create(node).Error
}

// Update 更新节点
func (r *NodeRepository) Update(node *model.GostNode) error {
	return r.DB.Save(node).Error
}

// Delete 删除节点
func (r *NodeRepository) Delete(id uint) error {
	return r.DB.Delete(&model.GostNode{}, id).Error
}

// FindByID 根据 ID 查询节点
func (r *NodeRepository) FindByID(id uint) (*model.GostNode, error) {
	var node model.GostNode
	err := r.DB.First(&node, id).Error
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// FindByIDWithRelations 根据 ID 查询节点（包含关联）
func (r *NodeRepository) FindByIDWithRelations(id uint) (*model.GostNode, error) {
	var node model.GostNode
	err := r.DB.Preload("Rules").Preload("EntryTunnels").Preload("ExitTunnels").First(&node, id).Error
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// List 查询节点列表
func (r *NodeRepository) List(opt *QueryOption) ([]model.GostNode, int64, error) {
	var nodes []model.GostNode
	var total int64

	db := r.DB.Model(&model.GostNode{})

	// 应用条件过滤（统计总数前）
	db = ApplyConditions(db, opt)

	// 统计总数（包含过滤条件）
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// 默认按创建时间倒序
	if opt == nil || len(opt.Orders) == 0 {
		db = db.Order("created_at DESC")
	}

	// 应用分页
	db = ApplyPagination(db, opt)

	if err := db.Find(&nodes).Error; err != nil {
		return nil, 0, err
	}

	return nodes, total, nil
}

// FindByName 根据名称查询节点
func (r *NodeRepository) FindByName(name string) (*model.GostNode, error) {
	var node model.GostNode
	err := r.DB.Where("name = ?", name).First(&node).Error
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// ExistsByName 检查名称是否存在
func (r *NodeRepository) ExistsByName(name string, excludeID ...uint) (bool, error) {
	var count int64
	db := r.DB.Model(&model.GostNode{}).Where("name = ?", name)
	if len(excludeID) > 0 {
		db = db.Where("id != ?", excludeID[0])
	}
	err := db.Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// UpdateStatus 更新节点状态
func (r *NodeRepository) UpdateStatus(id uint, status model.NodeStatus) error {
	return r.UpdateField(&model.GostNode{}, id, "status", status)
}

// UpdateLastCheck 更新最后检查时间
func (r *NodeRepository) UpdateLastCheck(id uint) error {
	return r.UpdateField(&model.GostNode{}, id, "last_check_at", time.Now())
}

// GetAllOnline 获取所有在线节点
func (r *NodeRepository) GetAllOnline() ([]model.GostNode, error) {
	var nodes []model.GostNode
	err := r.DB.Where("status = ?", model.NodeStatusOnline).Find(&nodes).Error
	return nodes, err
}

// CountByStatus 按状态统计数量
func (r *NodeRepository) CountByStatus(status model.NodeStatus) (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostNode{}).Where("status = ?", status).Count(&count).Error
	return count, err
}

// CountAll 统计总数
func (r *NodeRepository) CountAll() (int64, error) {
	var count int64
	err := r.DB.Model(&model.GostNode{}).Count(&count).Error
	return count, err
}

// UpdateStats 更新节点流量统计（计算增量）
// Gost observer 上报的是累计总量，需要计算增量后再累加
func (r *NodeRepository) UpdateStats(id uint, reportedInputBytes, reportedOutputBytes int64) error {
	// 先查询当前值
	var node model.GostNode
	if err := r.DB.Select("id", "last_reported_input_bytes", "last_reported_output_bytes").
		Where("id = ?", id).First(&node).Error; err != nil {
		return err
	}

	// 计算增量（如果是第一次上报或重启后，上报值可能小于上次值，此时重置为上报值）
	var inputDelta, outputDelta int64
	if reportedInputBytes >= node.LastReportedInputBytes {
		inputDelta = reportedInputBytes - node.LastReportedInputBytes
	} else {
		// Gost 重启后计数器重置，直接使用新值作为增量
		inputDelta = reportedInputBytes
	}

	if reportedOutputBytes >= node.LastReportedOutputBytes {
		outputDelta = reportedOutputBytes - node.LastReportedOutputBytes
	} else {
		outputDelta = reportedOutputBytes
	}

	// 只有增量大于0时才更新（避免无效更新）
	if inputDelta > 0 || outputDelta > 0 {
		return r.DB.Model(&model.GostNode{}).Where("id = ?", id).Updates(map[string]interface{}{
			"input_bytes":                gorm.Expr("input_bytes + ?", inputDelta),
			"output_bytes":               gorm.Expr("output_bytes + ?", outputDelta),
			"total_bytes":                gorm.Expr("total_bytes + ?", inputDelta+outputDelta),
			"last_reported_input_bytes":  reportedInputBytes,
			"last_reported_output_bytes": reportedOutputBytes,
		}).Error
	}

	// 没有增量，只更新上次上报值
	return r.DB.Model(&model.GostNode{}).Where("id = ?", id).Updates(map[string]interface{}{
		"last_reported_input_bytes":  reportedInputBytes,
		"last_reported_output_bytes": reportedOutputBytes,
	}).Error
}

// AddStatsDelta 按增量累加节点流量统计
func (r *NodeRepository) AddStatsDelta(id uint, inputDelta, outputDelta int64) error {
	if inputDelta == 0 && outputDelta == 0 {
		return nil
	}

	return r.DB.Model(&model.GostNode{}).Where("id = ?", id).Updates(map[string]interface{}{
		"input_bytes":  gorm.Expr("input_bytes + ?", inputDelta),
		"output_bytes": gorm.Expr("output_bytes + ?", outputDelta),
		"total_bytes":  gorm.Expr("total_bytes + ?", inputDelta+outputDelta),
	}).Error
}
