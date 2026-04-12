package repository

import (
	"fmt"
	"path/filepath"
	"testing"

	"gost-panel/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newRepositoryTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "repository-test.db")
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

	return db
}

func createRepositoryTestNode(t *testing.T, db *gorm.DB, name string) *model.GostNode {
	t.Helper()

	node := &model.GostNode{
		Name:    name,
		Address: "127.0.0.1",
		Port:    39000,
		Status:  model.NodeStatusOnline,
	}
	if err := db.Create(node).Error; err != nil {
		t.Fatalf("create node failed: %v", err)
	}

	return node
}

func createRepositoryTestRule(t *testing.T, db *gorm.DB, nodeID *uint, tunnelID *uint, ruleType model.RuleType, port int) *model.GostRule {
	t.Helper()

	rule := &model.GostRule{
		Name:       fmt.Sprintf("rule-%d", port),
		NodeID:     nodeID,
		TunnelID:   tunnelID,
		Type:       ruleType,
		ListenPort: port,
		Status:     model.RuleStatusRunning,
	}
	if err := db.Create(rule).Error; err != nil {
		t.Fatalf("create rule failed: %v", err)
	}

	return rule
}

func TestRuleRepositoryUpdateStatsDeduplicatesDuplicateReports(t *testing.T) {
	db := newRepositoryTestDB(t)
	repo := NewRuleRepository(db)
	node := createRepositoryTestNode(t, db, "node-1")
	rule := createRepositoryTestRule(t, db, &node.ID, nil, model.RuleTypeForward, 8080)
	serviceName := fmt.Sprintf("rule-%d-tcp", rule.ID)

	inputDelta, outputDelta, connsDelta, err := repo.UpdateStats(rule.ID, serviceName, 100, 50, 2)
	if err != nil {
		t.Fatalf("first update stats failed: %v", err)
	}
	if inputDelta != 100 || outputDelta != 50 || connsDelta != 2 {
		t.Fatalf("unexpected first deltas: in=%d out=%d conns=%d", inputDelta, outputDelta, connsDelta)
	}

	inputDelta, outputDelta, connsDelta, err = repo.UpdateStats(rule.ID, serviceName, 100, 50, 2)
	if err != nil {
		t.Fatalf("duplicate update stats failed: %v", err)
	}
	if inputDelta != 0 || outputDelta != 0 || connsDelta != 0 {
		t.Fatalf("duplicate report should produce zero delta: in=%d out=%d conns=%d", inputDelta, outputDelta, connsDelta)
	}

	var updated model.GostRule
	if err := db.First(&updated, rule.ID).Error; err != nil {
		t.Fatalf("load updated rule failed: %v", err)
	}
	if updated.InputBytes != 100 || updated.OutputBytes != 50 || updated.TotalBytes != 150 {
		t.Fatalf("duplicate report should not change accumulated traffic: %+v", updated)
	}
	if updated.TotalRequests != 2 {
		t.Fatalf("duplicate report should not change request count: %d", updated.TotalRequests)
	}
}

func TestRuleRepositoryUpdateStatsIgnoresOutOfOrderRollback(t *testing.T) {
	db := newRepositoryTestDB(t)
	repo := NewRuleRepository(db)
	node := createRepositoryTestNode(t, db, "node-1")
	rule := createRepositoryTestRule(t, db, &node.ID, nil, model.RuleTypeForward, 8081)
	serviceName := fmt.Sprintf("rule-%d-tcp", rule.ID)

	if _, _, _, err := repo.UpdateStats(rule.ID, serviceName, 200, 80, 3); err != nil {
		t.Fatalf("baseline update stats failed: %v", err)
	}

	inputDelta, outputDelta, connsDelta, err := repo.UpdateStats(rule.ID, serviceName, 20, 10, 1)
	if err != nil {
		t.Fatalf("rollback update stats failed: %v", err)
	}
	if inputDelta != 0 || outputDelta != 0 || connsDelta != 0 {
		t.Fatalf("rollback report should be ignored: in=%d out=%d conns=%d", inputDelta, outputDelta, connsDelta)
	}

	var updated model.GostRule
	if err := db.First(&updated, rule.ID).Error; err != nil {
		t.Fatalf("load updated rule failed: %v", err)
	}
	if updated.InputBytes != 200 || updated.OutputBytes != 80 || updated.TotalBytes != 280 {
		t.Fatalf("rollback report should not change accumulated traffic: %+v", updated)
	}
	if updated.TotalRequests != 3 {
		t.Fatalf("rollback report should not change request count: %d", updated.TotalRequests)
	}
	if updated.LastReportedInputBytesTCP != 200 || updated.LastReportedOutputBytesTCP != 80 || updated.LastReportedTotalConnsTCP != 3 {
		t.Fatalf("rollback report should not move checkpoints: %+v", updated)
	}
}

func TestTunnelRepositoryUpdateStatsDeduplicatesDuplicateReports(t *testing.T) {
	db := newRepositoryTestDB(t)
	repo := NewTunnelRepository(db)
	entryNode := createRepositoryTestNode(t, db, "entry-node")
	exitNode := createRepositoryTestNode(t, db, "exit-node")
	tunnel := &model.GostTunnel{
		Name:        "tunnel-1",
		EntryNodeID: entryNode.ID,
		ExitNodeID:  exitNode.ID,
		Protocol:    "ws",
		RelayPort:   8443,
		Status:      model.TunnelStatusRunning,
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}

	inputDelta, outputDelta, err := repo.UpdateStats(tunnel.ID, 300, 120)
	if err != nil {
		t.Fatalf("first tunnel update stats failed: %v", err)
	}
	if inputDelta != 300 || outputDelta != 120 {
		t.Fatalf("unexpected first tunnel deltas: in=%d out=%d", inputDelta, outputDelta)
	}

	inputDelta, outputDelta, err = repo.UpdateStats(tunnel.ID, 300, 120)
	if err != nil {
		t.Fatalf("duplicate tunnel update stats failed: %v", err)
	}
	if inputDelta != 0 || outputDelta != 0 {
		t.Fatalf("duplicate tunnel report should produce zero delta: in=%d out=%d", inputDelta, outputDelta)
	}

	var updated model.GostTunnel
	if err := db.First(&updated, tunnel.ID).Error; err != nil {
		t.Fatalf("load updated tunnel failed: %v", err)
	}
	if updated.InputBytes != 300 || updated.OutputBytes != 120 || updated.TotalBytes != 420 {
		t.Fatalf("duplicate tunnel report should not change accumulated traffic: %+v", updated)
	}
}

func TestRuleRepositoryRuntimeNodeQueriesIncludeTunnelRules(t *testing.T) {
	db := newRepositoryTestDB(t)
	ruleRepo := NewRuleRepository(db)
	entryNode := createRepositoryTestNode(t, db, "entry-node")
	exitNode := createRepositoryTestNode(t, db, "exit-node")
	tunnel := &model.GostTunnel{
		Name:        "tunnel-1",
		EntryNodeID: entryNode.ID,
		ExitNodeID:  exitNode.ID,
		Protocol:    "ws",
		RelayPort:   8443,
		Status:      model.TunnelStatusRunning,
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}

	rule := createRepositoryTestRule(t, db, nil, &tunnel.ID, model.RuleTypeTunnel, 9000)

	rules, err := ruleRepo.FindByRuntimeNodeID(entryNode.ID)
	if err != nil {
		t.Fatalf("find by runtime node id failed: %v", err)
	}
	if len(rules) != 1 || rules[0].ID != rule.ID {
		t.Fatalf("runtime node query should include tunnel entry rule, got: %+v", rules)
	}

	exists, err := ruleRepo.ExistsByPort(entryNode.ID, rule.ListenPort)
	if err != nil {
		t.Fatalf("exists by port failed: %v", err)
	}
	if !exists {
		t.Fatalf("port conflict check should include tunnel entry rule")
	}
}
