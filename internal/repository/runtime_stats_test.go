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

func TestRuleRepositoryUpdateStatsHandlesCounterReset(t *testing.T) {
	db := newRepositoryTestDB(t)
	repo := NewRuleRepository(db)
	node := createRepositoryTestNode(t, db, "node-1")
	rule := createRepositoryTestRule(t, db, &node.ID, nil, model.RuleTypeForward, 8081)
	serviceName := fmt.Sprintf("rule-%d-tcp", rule.ID)

	if _, _, _, err := repo.UpdateStats(rule.ID, serviceName, 200, 80, 3); err != nil {
		t.Fatalf("baseline update stats failed: %v", err)
	}

	// 模拟 Gost 进程/服务重启：累计计数器归零后从较小值重新上报。
	// 这必须被识别为“计数器重置”，并把本次上报值作为新的增量继续累计，
	// 而不是被忽略（否则流量会永久冻结）。
	inputDelta, outputDelta, connsDelta, err := repo.UpdateStats(rule.ID, serviceName, 20, 10, 1)
	if err != nil {
		t.Fatalf("counter-reset update stats failed: %v", err)
	}
	if inputDelta != 20 || outputDelta != 10 || connsDelta != 1 {
		t.Fatalf("counter reset should accumulate reported value as delta: in=%d out=%d conns=%d", inputDelta, outputDelta, connsDelta)
	}

	var updated model.GostRule
	if err := db.First(&updated, rule.ID).Error; err != nil {
		t.Fatalf("load updated rule failed: %v", err)
	}
	if updated.InputBytes != 220 || updated.OutputBytes != 90 || updated.TotalBytes != 310 {
		t.Fatalf("counter reset should continue accumulating traffic: %+v", updated)
	}
	if updated.TotalRequests != 4 {
		t.Fatalf("counter reset should continue accumulating request count: %d", updated.TotalRequests)
	}
	if updated.LastReportedInputBytesTCP != 20 || updated.LastReportedOutputBytesTCP != 10 || updated.LastReportedTotalConnsTCP != 1 {
		t.Fatalf("counter reset should move checkpoints to the new low value: %+v", updated)
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

func TestTunnelRepositoryFindByNodeIDIncludesExplicitHopNodes(t *testing.T) {
	db := newRepositoryTestDB(t)
	repo := NewTunnelRepository(db)
	entryNode := createRepositoryTestNode(t, db, "entry-node")
	middleNode := createRepositoryTestNode(t, db, "middle-node")
	exitNode := createRepositoryTestNode(t, db, "exit-node")
	tunnel := &model.GostTunnel{
		Name:        "chain-tunnel",
		EntryNodeID: entryNode.ID,
		ExitNodeID:  exitNode.ID,
		Protocol:    "ws",
		RelayPort:   8443,
		Hops: []model.TunnelHop{
			{NodeID: middleNode.ID, Protocol: "ws", RelayPort: 9001},
			{NodeID: exitNode.ID, Protocol: "ws", RelayPort: 9002},
		},
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}

	tunnels, err := repo.FindByNodeID(middleNode.ID)
	if err != nil {
		t.Fatalf("find by hop node id failed: %v", err)
	}
	if len(tunnels) != 1 || tunnels[0].ID != tunnel.ID {
		t.Fatalf("expected hop node lookup to include tunnel, got %+v", tunnels)
	}
}

func TestTunnelRepositoryStopByNodeIDIncludesExplicitHopNodes(t *testing.T) {
	db := newRepositoryTestDB(t)
	repo := NewTunnelRepository(db)
	entryNode := createRepositoryTestNode(t, db, "entry-node")
	middleNode := createRepositoryTestNode(t, db, "middle-node")
	exitNode := createRepositoryTestNode(t, db, "exit-node")
	tunnel := &model.GostTunnel{
		Name:        "chain-tunnel",
		EntryNodeID: entryNode.ID,
		ExitNodeID:  exitNode.ID,
		Protocol:    "ws",
		RelayPort:   8443,
		Status:      model.TunnelStatusRunning,
		Hops: []model.TunnelHop{
			{NodeID: middleNode.ID, Protocol: "ws", RelayPort: 9001},
			{NodeID: exitNode.ID, Protocol: "ws", RelayPort: 9002},
		},
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}

	if err := repo.StopByNodeID(middleNode.ID); err != nil {
		t.Fatalf("stop by hop node id failed: %v", err)
	}

	var updated model.GostTunnel
	if err := db.First(&updated, tunnel.ID).Error; err != nil {
		t.Fatalf("load updated tunnel failed: %v", err)
	}
	if updated.Status != model.TunnelStatusStopped {
		t.Fatalf("expected tunnel using hop node to be stopped, got %s", updated.Status)
	}
}

func TestTunnelRepositoryHasRulesIncludesPrimaryAndBackupTunnelReferences(t *testing.T) {
	db := newRepositoryTestDB(t)
	repo := NewTunnelRepository(db)
	entryNode := createRepositoryTestNode(t, db, "entry-node")
	exitNode := createRepositoryTestNode(t, db, "exit-node")
	primary := &model.GostTunnel{
		Name:        "primary",
		EntryNodeID: entryNode.ID,
		ExitNodeID:  exitNode.ID,
		Protocol:    "ws",
		RelayPort:   8443,
	}
	backup := &model.GostTunnel{
		Name:        "backup",
		EntryNodeID: entryNode.ID,
		ExitNodeID:  exitNode.ID,
		Protocol:    "ws",
		RelayPort:   8444,
	}
	active := &model.GostTunnel{
		Name:        "active",
		EntryNodeID: entryNode.ID,
		ExitNodeID:  exitNode.ID,
		Protocol:    "ws",
		RelayPort:   8445,
	}
	if err := db.Create(primary).Error; err != nil {
		t.Fatalf("create primary tunnel failed: %v", err)
	}
	if err := db.Create(backup).Error; err != nil {
		t.Fatalf("create backup tunnel failed: %v", err)
	}
	if err := db.Create(active).Error; err != nil {
		t.Fatalf("create active tunnel failed: %v", err)
	}

	rule := &model.GostRule{
		Name:            "rule",
		Type:            model.RuleTypeTunnel,
		TunnelID:        &active.ID,
		PrimaryTunnelID: &primary.ID,
		BackupTunnelIDs: []uint{backup.ID},
		ListenPort:      9000,
		Status:          model.RuleStatusStopped,
	}
	if err := db.Create(rule).Error; err != nil {
		t.Fatalf("create rule failed: %v", err)
	}

	for _, tunnelID := range []uint{primary.ID, backup.ID, active.ID} {
		hasRules, err := repo.HasRules(tunnelID)
		if err != nil {
			t.Fatalf("has rules failed for tunnel %d: %v", tunnelID, err)
		}
		if !hasRules {
			t.Fatalf("expected tunnel %d to be treated as referenced", tunnelID)
		}
	}
}
