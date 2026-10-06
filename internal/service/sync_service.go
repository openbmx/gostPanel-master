package service

import (
	"sync"
	"time"

	"gost-panel/internal/repository"
	"gost-panel/pkg/logger"

	"gorm.io/gorm"
)

// RuleSyncService 规则状态同步服务
// 定时读取各节点的真实运行配置，由看门狗把节点对齐到期望状态并校正规则/隧道状态，
// 最后执行隧道规则的故障转移。
type RuleSyncService struct {
	nodeRepo    *repository.NodeRepository
	ruleService *RuleService
	watchdog    *Watchdog
	ticker      *time.Ticker
	stopChan    chan struct{}
	wg          sync.WaitGroup
}

// NewRuleSyncService 创建规则状态同步服务
func NewRuleSyncService(db *gorm.DB) *RuleSyncService {
	ruleService := NewRuleService(db)
	return &RuleSyncService{
		nodeRepo:    repository.NewNodeRepository(db),
		ruleService: ruleService,
		watchdog:    NewWatchdog(db, ruleService),
		stopChan:    make(chan struct{}),
	}
}

// Start 启动定时同步任务（每 5 秒）
func (s *RuleSyncService) Start() {
	s.ticker = time.NewTicker(5 * time.Second)
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()
		logger.Info("规则状态同步与看门狗已启动 (5s 间隔)")

		// 立即执行一次
		s.syncAll()

		for {
			select {
			case <-s.ticker.C:
				s.syncAll()
			case <-s.stopChan:
				logger.Info("规则状态同步服务已停止")
				return
			}
		}
	}()
}

// Stop 停止同步服务
func (s *RuleSyncService) Stop() {
	if s.ticker != nil {
		s.ticker.Stop()
	}
	close(s.stopChan)
	s.wg.Wait()
}

// syncAll 执行一轮巡检：读取节点配置 → 看门狗对账 → 故障转移
func (s *RuleSyncService) syncAll() {
	nodes, _, err := s.nodeRepo.List(nil)
	if err != nil {
		logger.Errorf("[Sync] 获取节点列表失败: %v", err)
		return
	}

	// 先等所有节点的配置读完再统一对账：隧道横跨多个节点，
	// 只有拿到每一跳的实际状态，才能判断整条链路是否完好
	snapshots := collectNodeSnapshots(nodes)
	s.watchdog.Reconcile(nodes, snapshots)

	s.ruleService.AutoFailover()
}
