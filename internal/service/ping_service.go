package service

import (
	"sync"
	"time"

	"gost-panel/internal/model"
	"gost-panel/internal/repository"
	"gost-panel/internal/utils"
	"gost-panel/pkg/logger"

	"gorm.io/gorm"
)

// NodeHealthService 节点健康检测服务
// 使用 Gost API 进行健康检查
type NodeHealthService struct {
	nodeRepo   *repository.NodeRepository
	ruleRepo   *repository.RuleRepository
	tunnelRepo *repository.TunnelRepository
	ticker     *time.Ticker
	stopChan   chan struct{}
	wg         sync.WaitGroup
}

// NewNodeHealthService 创建节点健康检测服务
func NewNodeHealthService(db *gorm.DB) *NodeHealthService {
	return &NodeHealthService{
		nodeRepo:   repository.NewNodeRepository(db),
		ruleRepo:   repository.NewRuleRepository(db),
		tunnelRepo: repository.NewTunnelRepository(db),
		stopChan:   make(chan struct{}),
	}
}

// Start 启动定时健康检测（每 5 秒）
func (s *NodeHealthService) Start() {
	s.ticker = time.NewTicker(5 * time.Second)
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		logger.Info("节点健康检测服务已启动")

		// 立即执行一次
		s.checkAll()

		for {
			select {
			case <-s.ticker.C:
				s.checkAll()
			case <-s.stopChan:
				logger.Info("节点健康检测服务已停止")
				return
			}
		}
	}()
}

// Stop 停止健康检测
func (s *NodeHealthService) Stop() {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	close(s.stopChan)
	s.wg.Wait()
}

// checkAll 检测所有资源
func (s *NodeHealthService) checkAll() {
	s.checkNodes()
}

// checkNodes 检测所有节点
// 使用 Gost API 的 /config 接口进行健康检查
func (s *NodeHealthService) checkNodes() {
	nodes, _, err := s.nodeRepo.List(nil)
	if err != nil {
		logger.Errorf("获取节点列表失败: %v", err)
		return
	}

	for _, node := range nodes {
		go func(n model.GostNode) {
			status := s.checkNodeHealth(n)

			// 状态变更处理
			if status != n.Status {
				logger.Infof("节点 %s 状态变更: %s -> %s", n.Name, n.Status, status)
				oldStatus := n.Status
				if err = s.nodeRepo.UpdateStatus(n.ID, status); err != nil {
					logger.Errorf("更新节点 %s 状态失败: %v", n.Name, err)
				}

				// 节点从离线恢复到在线，仅记录提示。
				// 规则/隧道真实状态交由规则同步服务按节点配置回读判断，
				// 避免把“控制面不可达但转发面仍生效”的资源误标为 stopped。
				if oldStatus == model.NodeStatusOffline && status == model.NodeStatusOnline {
					logger.Infof("节点 %s 恢复在线，规则与隧道状态将由同步服务自动校正", n.Name)
				}
			}

			if status == model.NodeStatusOnline {
				logger.Debugf("节点 %s 在线", n.Name)
			} else {
				// 不再因为节点 API 暂时不可达就强制把规则/隧道写成 stopped。
				// 否则会出现“规则实际仍生效，但面板显示已停止”的误判。
				logger.Debugf("节点 %s 离线，保留规则/隧道最后已知状态，等待后续同步校正", n.Name)
			}

			_ = s.nodeRepo.UpdateLastCheck(n.ID)
		}(node)
	}
}

// checkNodeHealth 检查单个节点的健康状态
// 通过调用 Gost API 的 /config 接口来判断节点是否可用
func (s *NodeHealthService) checkNodeHealth(node model.GostNode) model.NodeStatus {
	// 检查地址是否有效
	if node.Address == "" || node.Port == 0 {
		return model.NodeStatusOffline
	}

	// 验证 Gost API 是否可用
	client := utils.GetGostClient(&node)

	if err := client.HealthCheck(); err != nil {
		logger.Debugf("节点 %d (%s) API 检查失败: %v", node.ID, node.Name, err)
		return model.NodeStatusOffline
	}

	return model.NodeStatusOnline
}
