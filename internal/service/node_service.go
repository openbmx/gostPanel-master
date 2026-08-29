// Package service 提供业务逻辑层服务
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

// NodeService 节点服务
// 负责节点的 CRUD 操作和业务逻辑处理
type NodeService struct {
	nodeRepo   *repository.NodeRepository
	tunnelRepo *repository.TunnelRepository
	logService *LogService
}

// NewNodeService 创建节点服务
func NewNodeService(db *gorm.DB) *NodeService {
	return &NodeService{
		nodeRepo:   repository.NewNodeRepository(db),
		tunnelRepo: repository.NewTunnelRepository(db),
		logService: NewLogService(db),
	}
}

// Create 创建节点
// 创建前会检查节点名称是否已存在
func (s *NodeService) Create(req *dto.CreateNodeReq, userID uint, username string, ip, userAgent string) (*model.GostNode, error) {
	// 检查名称是否存在
	exists, err := s.nodeRepo.ExistsByName(req.Name)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.ErrNodeNameExists
	}

	scheme := req.Scheme
	if scheme == "" {
		scheme = "http"
	}

	// 创建节点
	node := &model.GostNode{
		Name:     req.Name,
		Address:  req.Address,
		Port:     req.Port,
		Scheme:   scheme,
		Username: req.Username,
		Password: req.Password,
		Remark:   req.Remark,
		Status:   model.NodeStatusOffline,
	}

	if err = s.nodeRepo.Create(node); err != nil {
		return nil, err
	}

	// 记录操作日志
	s.logService.Record(
		userID,
		username,
		model.ActionCreate,
		model.ResourceTypeNode,
		node.ID,
		fmt.Sprintf("创建节点: %s", node.Name),
		ip,
		userAgent)

	logger.Infof("创建节点成功: %s (%s:%d)", node.Name, node.Address, node.Port)
	return node, nil
}

// Update 更新节点
// 更新前会检查节点是否存在以及新名称是否与其他节点冲突
func (s *NodeService) Update(id uint, req *dto.UpdateNodeReq, userID uint, username string, ip, userAgent string) (*model.GostNode, error) {
	// 查询节点
	node, err := s.nodeRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrNodeNotFound
		}
		return nil, err
	}

	// 检查名称是否存在（排除自身）
	exists, err := s.nodeRepo.ExistsByName(req.Name, id)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, errors.ErrNodeNameExists
	}

	// 更新节点
	node.Name = req.Name
	node.Address = req.Address
	node.Port = req.Port
	node.Remark = req.Remark
	if req.Scheme != "" {
		node.Scheme = req.Scheme
	}
	if req.Username != "" {
		node.Username = req.Username
	}
	// 安全：密码不再随节点数据下发，前端无法回填。
	// 留空一律视为"不修改"，否则编辑备注这类操作会把节点凭据清空。
	if req.Password != "" {
		node.Password = req.Password
	}

	if err = s.nodeRepo.Update(node); err != nil {
		return nil, err
	}

	// 记录操作日志
	s.logService.Record(
		userID,
		username,
		model.ActionUpdate,
		model.ResourceTypeNode,
		node.ID,
		fmt.Sprintf("更新节点: %s", node.Name),
		ip,
		userAgent)

	return node, nil
}

// Delete 删除节点
// 删除前会检查节点是否存在以及是否有关联的转发规则
func (s *NodeService) Delete(id uint, userID uint, username string, ip, userAgent string) error {
	// 查询节点（包含关联）
	node, err := s.nodeRepo.FindByIDWithRelations(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return errors.ErrNodeNotFound
		}
		return err
	}

	// 检查是否有关联的规则
	if len(node.Rules) > 0 {
		return errors.ErrNodeHasRules
	}

	// 删除节点前，用户需要手动删除相关隧道
	tunnels, err := s.tunnelRepo.FindByNodeID(id)
	if err != nil {
		return err
	}
	if len(node.EntryTunnels) > 0 || len(node.ExitTunnels) > 0 || len(tunnels) > 0 {
		return errors.ErrNodeHasTunnels
	}

	// 删除节点
	if err = s.nodeRepo.Delete(id); err != nil {
		return err
	}

	// 记录操作日志
	s.logService.Record(
		userID,
		username,
		model.ActionDelete,
		model.ResourceTypeNode,
		id,
		fmt.Sprintf("删除节点: %s", node.Name),
		ip,
		userAgent)

	logger.Infof("删除节点成功: %s", node.Name)
	return nil
}

// GetByID 获取节点详情
func (s *NodeService) GetByID(id uint) (*model.GostNode, error) {
	node, err := s.nodeRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrNodeNotFound
		}
		return nil, err
	}
	return node, nil
}

// GetCredentials 获取节点的 API 凭据，用于生成安装命令。
//
// 安全：这是唯一会返回节点密码的接口。密码等同于目标主机 GOST 守护进程的
// 完全控制权，因此这里刻意做成一次显式的、单节点的、留痕的操作 ——
// 而不是像此前那样，随 GET /nodes 把所有节点的密码一起下发。
func (s *NodeService) GetCredentials(id uint, userID uint, username, ip, userAgent string) (*dto.NodeCredentialsResp, error) {
	node, err := s.nodeRepo.FindByID(id)
	if err != nil {
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.ErrNodeNotFound
		}
		return nil, err
	}

	s.logService.Record(
		userID,
		username,
		model.ActionViewSecret,
		model.ResourceTypeNode,
		node.ID,
		fmt.Sprintf("查看节点凭据: %s", node.Name),
		ip,
		userAgent)
	logger.Warnf("节点凭据被查看: node=%s(%d) by=%s ip=%s", node.Name, node.ID, username, ip)

	return &dto.NodeCredentialsResp{
		ID:       node.ID,
		Name:     node.Name,
		Port:     node.Port,
		Username: node.Username,
		Password: node.Password,
	}, nil
}

// List 获取节点列表
func (s *NodeService) List(req *dto.NodeListReq) ([]model.GostNode, int64, error) {
	// 设置默认值
	req.SetDefaults()

	opt := &repository.QueryOption{
		Pagination: &repository.Pagination{
			Page:     req.Page,
			PageSize: req.PageSize,
		},
		Conditions: make(map[string]any),
	}

	// 状态筛选
	if req.Status != "" {
		opt.Conditions["status = ?"] = req.Status
	}

	// 关键词搜索
	if req.Keyword != "" {
		// 转义 LIKE 通配符，避免用户输入的 % / _ 变成任意匹配
		kw := utils.EscapeLike(req.Keyword)
		opt.Conditions["name LIKE ? ESCAPE '\\' OR address LIKE ? ESCAPE '\\'"] = []interface{}{
			"%" + kw + "%",
			"%" + kw + "%",
		}
	}

	return s.nodeRepo.List(opt)
}

// CreateGostClient 创建节点的 Gost 客户端
func (s *NodeService) CreateGostClient(id uint) (*gost.Client, error) {
	node, err := s.nodeRepo.FindByID(id)
	if err != nil {
		return nil, err
	}

	return utils.GetGostClient(node), nil
}

// GetConfig 获取节点配置
func (s *NodeService) GetConfig(id uint) (*gost.GostConfig, error) {
	node, err := s.nodeRepo.FindByID(id)
	if err != nil {
		return nil, err
	}

	client := utils.GetGostClient(node)

	config, err := client.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("获取节点配置失败: %v", err)
	}

	return config, nil
}
