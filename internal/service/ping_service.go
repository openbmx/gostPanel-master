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

// observerRetryInterval 观察器对账失败后的重试间隔。
// 常见失败原因是尚未配置面板地址，这属于配置问题而非瞬时故障，
// 每 5 秒重试一次只会刷屏，因此拉长到分钟级。
const observerRetryInterval = 5 * time.Minute

// NodeHealthService 节点健康检测服务
// 使用 Gost API 进行健康检查
type NodeHealthService struct {
	nodeRepo   *repository.NodeRepository
	ruleRepo   *repository.RuleRepository
	tunnelRepo *repository.TunnelRepository
	sysRepo    *repository.SystemConfigRepository
	ticker     *time.Ticker
	stopChan   chan struct{}
	wg         sync.WaitGroup

	// observerSynced 记录本进程内已完成观察器对账的节点。
	//
	// 存在的意义见 reconcileObserver：上报接口加了令牌鉴权之后，
	// 从旧版本升级上来的节点仍持有不带令牌的旧回调地址，
	// 必须在面板启动后主动把新地址推下去，否则流量统计会静默停止。
	observerSynced sync.Map // nodeID(uint) -> time.Time（上次尝试时间）
}

// NewNodeHealthService 创建节点健康检测服务
func NewNodeHealthService(db *gorm.DB) *NodeHealthService {
	return &NodeHealthService{
		nodeRepo:   repository.NewNodeRepository(db),
		ruleRepo:   repository.NewRuleRepository(db),
		tunnelRepo: repository.NewTunnelRepository(db),
		sysRepo:    repository.NewSystemConfigRepository(db),
		stopChan:   make(chan struct{}),
	}
}

// reconcileObserver 确保节点上的观察器指向当前面板地址与上报令牌。
//
// 为什么需要它：观察器配置存放在节点侧的 GOST 进程里，面板只在
// 创建/启动规则与隧道时才会下发。这带来两个必须处理的场景：
//
//  1. 从旧版本升级：上报接口新增了令牌鉴权，而节点上仍是旧的、不带令牌的
//     回调地址。若不主动对账，所有存量规则的上报都会被 401 拒绝，
//     转发本身不受影响，但面板里的流量统计会一直停在升级那一刻。
//  2. 面板地址变更：运维改了 PanelURL 后，同样需要把新地址推下去。
//
// 每个节点在「上线」后对账一次即可；节点掉线时清除标记，
// 使其重新上线后再对账（节点可能在这期间重装或重置过配置）。
func (s *NodeHealthService) reconcileObserver(node model.GostNode) {
	if last, ok := s.observerSynced.Load(node.ID); ok {
		if t, isTime := last.(time.Time); isTime && time.Since(t) < observerRetryInterval {
			return
		}
	}
	s.observerSynced.Store(node.ID, time.Now())

	client := utils.GetGostClient(&node)
	if _, err := EnsureGlobalObserver(client, s.sysRepo); err != nil {
		// 未配置面板地址是最常见的原因，属于运维待办而非故障，
		// 这里只在 debug 级别记录，避免正常部署被噪音淹没
		logger.Debugf("节点 %s 观察器对账未完成: %v", node.Name, err)
		return
	}

	// 成功后标记为长期有效，不再重复对账
	s.observerSynced.Store(node.ID, time.Now().Add(24*time.Hour))
	logger.Infof("节点 %s 的流量上报配置已同步", node.Name)
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
				// 确保节点上的观察器指向当前面板地址与上报令牌。
				// 从旧版本升级时，节点持有的是不带令牌的旧地址，
				// 不对账的话流量统计会停在升级那一刻。
				s.reconcileObserver(n)
			} else {
				// 节点掉线：清除标记，使其重新上线后再对账一次
				// （节点可能在离线期间被重装或重置过配置）
				s.observerSynced.Delete(n.ID)
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
