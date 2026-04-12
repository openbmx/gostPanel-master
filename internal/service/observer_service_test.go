package service

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"gost-panel/internal/dto"
	"gost-panel/internal/model"
	"gost-panel/pkg/logger"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var testLoggerOnce sync.Once

func initObserverServiceTestLogger(t *testing.T) {
	t.Helper()

	var err error
	testLoggerOnce.Do(func() {
		err = logger.Init(&logger.Config{
			Level:  "debug",
			Format: "console",
			Output: "stdout",
		})
	})
	if err != nil {
		t.Fatalf("init logger failed: %v", err)
	}
}

func newObserverServiceTestFixture(t *testing.T) (*gorm.DB, *ObserverService) {
	t.Helper()
	initObserverServiceTestLogger(t)

	dbPath := filepath.Join(t.TempDir(), "observer-service-test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db failed: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	if err := db.AutoMigrate(&model.GostNode{}, &model.GostTunnel{}, &model.GostRule{}); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}

	return db, NewObserverService(db)
}

func createForwardRuleFixture(t *testing.T, db *gorm.DB) (*model.GostNode, *model.GostRule) {
	t.Helper()

	node := &model.GostNode{
		Name:    "node-1",
		Address: "127.0.0.1",
		Port:    39000,
		Status:  model.NodeStatusOnline,
	}
	if err := db.Create(node).Error; err != nil {
		t.Fatalf("create node failed: %v", err)
	}

	rule := &model.GostRule{
		Name:       "rule-1",
		Type:       model.RuleTypeForward,
		NodeID:     &node.ID,
		ListenPort: 8080,
		Status:     model.RuleStatusRunning,
	}
	if err := db.Create(rule).Error; err != nil {
		t.Fatalf("create rule failed: %v", err)
	}

	return node, rule
}

func loadRuleAndNode(t *testing.T, db *gorm.DB, ruleID, nodeID uint) (*model.GostRule, *model.GostNode) {
	t.Helper()

	var rule model.GostRule
	if err := db.First(&rule, ruleID).Error; err != nil {
		t.Fatalf("load rule failed: %v", err)
	}

	var node model.GostNode
	if err := db.First(&node, nodeID).Error; err != nil {
		t.Fatalf("load node failed: %v", err)
	}

	return &rule, &node
}

func TestObserverServiceProcessEventAcceptsServiceStats(t *testing.T) {
	db, svc := newObserverServiceTestFixture(t)
	node, rule := createForwardRuleFixture(t, db)

	event := &dto.ObserverEvent{
		Kind:    "service",
		Service: fmt.Sprintf("rule-%d-tcp", rule.ID),
		Type:    "stats",
		Stats: &dto.ObserverStats{
			InputBytes:  100,
			OutputBytes: 50,
			TotalConns:  2,
		},
	}

	if err := svc.processEvent(event); err != nil {
		t.Fatalf("process service event failed: %v", err)
	}

	updatedRule, updatedNode := loadRuleAndNode(t, db, rule.ID, node.ID)

	if updatedRule.InputBytes != 100 || updatedRule.OutputBytes != 50 || updatedRule.TotalBytes != 150 {
		t.Fatalf("unexpected rule traffic stats: %+v", updatedRule)
	}
	if updatedRule.TotalRequests != 2 {
		t.Fatalf("unexpected rule request count: %d", updatedRule.TotalRequests)
	}
	if updatedRule.LastReportedInputBytesTCP != 100 || updatedRule.LastReportedOutputBytesTCP != 50 || updatedRule.LastReportedTotalConnsTCP != 2 {
		t.Fatalf("unexpected rule tcp checkpoints: %+v", updatedRule)
	}

	if updatedNode.InputBytes != 100 || updatedNode.OutputBytes != 50 || updatedNode.TotalBytes != 150 {
		t.Fatalf("unexpected node traffic stats: %+v", updatedNode)
	}
}

func TestObserverServiceProcessEventIgnoresHandlerStats(t *testing.T) {
	db, svc := newObserverServiceTestFixture(t)
	node, rule := createForwardRuleFixture(t, db)

	serviceEvent := &dto.ObserverEvent{
		Kind:    "service",
		Service: fmt.Sprintf("rule-%d-tcp", rule.ID),
		Type:    "stats",
		Stats: &dto.ObserverStats{
			InputBytes:  100,
			OutputBytes: 50,
			TotalConns:  2,
		},
	}
	if err := svc.processEvent(serviceEvent); err != nil {
		t.Fatalf("process baseline service event failed: %v", err)
	}

	handlerEvent := &dto.ObserverEvent{
		Kind:    "handler",
		Service: fmt.Sprintf("rule-%d-tcp", rule.ID),
		Client:  "client-1",
		Type:    "stats",
		Stats: &dto.ObserverStats{
			InputBytes:  200,
			OutputBytes: 100,
			TotalConns:  4,
		},
	}
	if err := svc.processEvent(handlerEvent); err != nil {
		t.Fatalf("process handler event failed: %v", err)
	}

	updatedRule, updatedNode := loadRuleAndNode(t, db, rule.ID, node.ID)

	if updatedRule.InputBytes != 100 || updatedRule.OutputBytes != 50 || updatedRule.TotalBytes != 150 {
		t.Fatalf("handler event should not change rule stats: %+v", updatedRule)
	}
	if updatedRule.TotalRequests != 2 {
		t.Fatalf("handler event should not change request count: %d", updatedRule.TotalRequests)
	}
	if updatedRule.LastReportedInputBytesTCP != 100 || updatedRule.LastReportedOutputBytesTCP != 50 || updatedRule.LastReportedTotalConnsTCP != 2 {
		t.Fatalf("handler event should not change checkpoints: %+v", updatedRule)
	}

	if updatedNode.InputBytes != 100 || updatedNode.OutputBytes != 50 || updatedNode.TotalBytes != 150 {
		t.Fatalf("handler event should not change node stats: %+v", updatedNode)
	}
}

func TestObserverServiceProcessEventIgnoresClientScopedStatsWithoutKind(t *testing.T) {
	db, svc := newObserverServiceTestFixture(t)
	node, rule := createForwardRuleFixture(t, db)

	event := &dto.ObserverEvent{
		Service: fmt.Sprintf("rule-%d-tcp", rule.ID),
		Client:  "client-1",
		Type:    "stats",
		Stats: &dto.ObserverStats{
			InputBytes:  100,
			OutputBytes: 50,
			TotalConns:  2,
		},
	}
	if err := svc.processEvent(event); err != nil {
		t.Fatalf("process client-scoped stats failed: %v", err)
	}

	updatedRule, updatedNode := loadRuleAndNode(t, db, rule.ID, node.ID)

	if updatedRule.InputBytes != 0 || updatedRule.OutputBytes != 0 || updatedRule.TotalBytes != 0 || updatedRule.TotalRequests != 0 {
		t.Fatalf("client-scoped stats without kind should not change rule stats: %+v", updatedRule)
	}
	if updatedRule.LastReportedInputBytesTCP != 0 || updatedRule.LastReportedOutputBytesTCP != 0 || updatedRule.LastReportedTotalConnsTCP != 0 {
		t.Fatalf("client-scoped stats without kind should not change checkpoints: %+v", updatedRule)
	}

	if updatedNode.InputBytes != 0 || updatedNode.OutputBytes != 0 || updatedNode.TotalBytes != 0 {
		t.Fatalf("client-scoped stats without kind should not change node stats: %+v", updatedNode)
	}
}
