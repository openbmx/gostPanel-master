package service

import (
	"testing"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestRuleServiceCreateRejectsBackupTunnelWithDifferentEntryNode(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entryA := createRuleChainNode(t, db, "entry-a")
	entryB := createRuleChainNode(t, db, "entry-b")
	exit := createRuleChainNode(t, db, "exit")
	primary := createRuleChainTunnel(t, db, "primary", entryA.ID, exit.ID)
	backup := createRuleChainTunnel(t, db, "backup", entryB.ID, exit.ID)

	_, err := svc.Create(&dto.CreateRuleReq{
		Type:            string(model.RuleTypeTunnel),
		TunnelID:        &primary.ID,
		BackupTunnelIDs: []uint{backup.ID},
		Name:            "rule",
		ListenPort:      9000,
		Targets:         []string{"127.0.0.1:80"},
	}, 1, "admin", "127.0.0.1", "test")

	if err != errors.ErrTunnelEntryMismatch {
		t.Fatalf("expected ErrTunnelEntryMismatch, got %v", err)
	}
}

func TestRuleServiceUpdateRejectsBackupTunnelWithDifferentEntryNode(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entryA := createRuleChainNode(t, db, "entry-a")
	entryB := createRuleChainNode(t, db, "entry-b")
	exit := createRuleChainNode(t, db, "exit")
	primary := createRuleChainTunnel(t, db, "primary", entryA.ID, exit.ID)
	backup := createRuleChainTunnel(t, db, "backup", entryB.ID, exit.ID)
	rule := createRuleChainRule(t, db, primary.ID, 9000)

	_, err := svc.Update(rule.ID, &dto.UpdateRuleReq{
		TunnelID:        &primary.ID,
		BackupTunnelIDs: []uint{backup.ID},
		Name:            "rule",
		ListenPort:      9000,
		Targets:         []string{"127.0.0.1:80"},
	}, 1, "admin", "127.0.0.1", "test")

	if err != errors.ErrTunnelEntryMismatch {
		t.Fatalf("expected ErrTunnelEntryMismatch, got %v", err)
	}
}

func TestRuleServiceCreateAllowsBackupTunnelWithSameEntryNode(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entry := createRuleChainNode(t, db, "entry")
	exitA := createRuleChainNode(t, db, "exit-a")
	exitB := createRuleChainNode(t, db, "exit-b")
	primary := createRuleChainTunnel(t, db, "primary", entry.ID, exitA.ID)
	backup := createRuleChainTunnel(t, db, "backup", entry.ID, exitB.ID)

	rule, err := svc.Create(&dto.CreateRuleReq{
		Type:            string(model.RuleTypeTunnel),
		TunnelID:        &primary.ID,
		BackupTunnelIDs: []uint{backup.ID},
		Name:            "rule",
		ListenPort:      9000,
		Targets:         []string{"127.0.0.1:80"},
	}, 1, "admin", "127.0.0.1", "test")

	if err != nil {
		t.Fatalf("create rule failed: %v", err)
	}
	if len(rule.BackupTunnelIDs) != 1 || rule.BackupTunnelIDs[0] != backup.ID {
		t.Fatalf("expected same-entry backup tunnel to be kept, got %+v", rule.BackupTunnelIDs)
	}
}

func TestRuleServiceListFiltersTunnelRulesByRuntimeEntryNode(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entry := createRuleChainNode(t, db, "entry")
	exit := createRuleChainNode(t, db, "exit")
	tunnel := createRuleChainTunnel(t, db, "primary", entry.ID, exit.ID)
	rule := createRuleChainRule(t, db, tunnel.ID, 9000)

	rules, total, err := svc.List(&dto.RuleListReq{
		Page:     1,
		PageSize: 10,
		NodeID:   entry.ID,
	})
	if err != nil {
		t.Fatalf("list rules failed: %v", err)
	}
	if total != 1 || len(rules) != 1 || rules[0].ID != rule.ID {
		t.Fatalf("expected tunnel rule to be listed by runtime entry node, total=%d rules=%+v", total, rules)
	}
}

func TestRuleServiceSelectAvailableTunnelSkipsOfflineHopNodes(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entry := createRuleChainNode(t, db, "entry")
	middle := createRuleChainNode(t, db, "middle")
	exitA := createRuleChainNode(t, db, "exit-a")
	exitB := createRuleChainNode(t, db, "exit-b")

	middle.Status = model.NodeStatusOffline
	if err := db.Save(middle).Error; err != nil {
		t.Fatalf("mark middle offline failed: %v", err)
	}

	primary := createRuleChainTunnelWithHops(t, db, "primary", entry.ID, []model.TunnelHop{
		{NodeID: middle.ID, Protocol: "ws", RelayPort: 9001},
		{NodeID: exitA.ID, Protocol: "ws", RelayPort: 9002},
	})
	backup := createRuleChainTunnel(t, db, "backup", entry.ID, exitB.ID)
	if err := db.Model(backup).Updates(map[string]any{
		"status":   model.TunnelStatusRunning,
		"chain_id": "tunnel-backup-chain",
	}).Error; err != nil {
		t.Fatalf("mark backup running failed: %v", err)
	}

	rule := &model.GostRule{
		Name:            "rule",
		Type:            model.RuleTypeTunnel,
		TunnelID:        &primary.ID,
		PrimaryTunnelID: &primary.ID,
		BackupTunnelIDs: []uint{backup.ID},
		ListenPort:      9000,
		Status:          model.RuleStatusRunning,
	}
	if err := db.Create(rule).Error; err != nil {
		t.Fatalf("create rule failed: %v", err)
	}

	selected, err := svc.selectAvailableTunnel(rule)
	if err != nil {
		t.Fatalf("select available tunnel failed: %v", err)
	}
	if selected.ID != backup.ID {
		t.Fatalf("expected offline-hop primary to be skipped in favor of backup, got tunnel %d", selected.ID)
	}
}

func TestRuleServiceUpdateRejectsInvalidBackupBeforeStoppingRunningRule(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entryA := createRuleChainNode(t, db, "entry-a")
	entryB := createRuleChainNode(t, db, "entry-b")
	exit := createRuleChainNode(t, db, "exit")
	primary := createRuleChainTunnel(t, db, "primary", entryA.ID, exit.ID)
	backup := createRuleChainTunnel(t, db, "backup", entryB.ID, exit.ID)
	rule := createRuleChainRule(t, db, primary.ID, 9000)

	if err := db.Model(primary).Updates(map[string]any{
		"status":   model.TunnelStatusRunning,
		"chain_id": "tunnel-primary-chain",
	}).Error; err != nil {
		t.Fatalf("mark primary running failed: %v", err)
	}
	if err := db.Model(rule).Update("status", model.RuleStatusRunning).Error; err != nil {
		t.Fatalf("mark rule running failed: %v", err)
	}

	_, err := svc.Update(rule.ID, &dto.UpdateRuleReq{
		TunnelID:        &primary.ID,
		BackupTunnelIDs: []uint{backup.ID},
		Name:            "rule",
		ListenPort:      9000,
		Targets:         []string{"127.0.0.1:80"},
	}, 1, "admin", "127.0.0.1", "test")
	if err != errors.ErrTunnelEntryMismatch {
		t.Fatalf("expected ErrTunnelEntryMismatch, got %v", err)
	}

	var updated model.GostRule
	if err := db.First(&updated, rule.ID).Error; err != nil {
		t.Fatalf("load updated rule failed: %v", err)
	}
	if updated.Status != model.RuleStatusRunning {
		t.Fatalf("invalid update should not stop a running rule, got status %s", updated.Status)
	}
}

func TestRuleServiceUpdateRejectsRunningSwitchToTunnelWithOfflineHop(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entry := createRuleChainNode(t, db, "entry")
	middle := createRuleChainNode(t, db, "middle")
	exitA := createRuleChainNode(t, db, "exit-a")
	exitB := createRuleChainNode(t, db, "exit-b")
	current := createRuleChainTunnel(t, db, "current", entry.ID, exitA.ID)
	target := createRuleChainTunnelWithHops(t, db, "target", entry.ID, []model.TunnelHop{
		{NodeID: middle.ID, Protocol: "ws", RelayPort: 9001},
		{NodeID: exitB.ID, Protocol: "ws", RelayPort: 9002},
	})
	rule := createRuleChainRule(t, db, current.ID, 9000)

	if err := db.Model(current).Updates(map[string]any{
		"status":   model.TunnelStatusRunning,
		"chain_id": "tunnel-current-chain",
	}).Error; err != nil {
		t.Fatalf("mark current running failed: %v", err)
	}
	if err := db.Model(rule).Update("status", model.RuleStatusRunning).Error; err != nil {
		t.Fatalf("mark rule running failed: %v", err)
	}
	middle.Status = model.NodeStatusOffline
	if err := db.Save(middle).Error; err != nil {
		t.Fatalf("mark middle offline failed: %v", err)
	}

	_, err := svc.Update(rule.ID, &dto.UpdateRuleReq{
		TunnelID:   &target.ID,
		Name:       "rule",
		ListenPort: 9000,
		Targets:    []string{"127.0.0.1:80"},
	}, 1, "admin", "127.0.0.1", "test")
	if err != errors.ErrTunnelFailoverUnavailable {
		t.Fatalf("expected ErrTunnelFailoverUnavailable, got %v", err)
	}

	var updated model.GostRule
	if err := db.First(&updated, rule.ID).Error; err != nil {
		t.Fatalf("load updated rule failed: %v", err)
	}
	if updated.Status != model.RuleStatusRunning || updated.TunnelID == nil || *updated.TunnelID != current.ID {
		t.Fatalf("invalid running switch should keep the current rule state, got %+v", updated)
	}
}

func TestRuleServiceSelectAvailableTunnelSkipsDifferentEntryBackupFromLegacyData(t *testing.T) {
	db, svc := newRuleChainServiceTestFixture(t)
	entryA := createRuleChainNode(t, db, "entry-a")
	entryB := createRuleChainNode(t, db, "entry-b")
	exit := createRuleChainNode(t, db, "exit")
	primary := createRuleChainTunnel(t, db, "primary", entryA.ID, exit.ID)
	backup := createRuleChainTunnel(t, db, "backup", entryB.ID, exit.ID)
	rule := &model.GostRule{
		Name:            "rule",
		Type:            model.RuleTypeTunnel,
		TunnelID:        &primary.ID,
		PrimaryTunnelID: &primary.ID,
		BackupTunnelIDs: []uint{backup.ID},
		ListenPort:      9000,
		Status:          model.RuleStatusRunning,
	}
	if err := db.Create(rule).Error; err != nil {
		t.Fatalf("create rule failed: %v", err)
	}
	if err := db.Model(backup).Updates(map[string]any{
		"status":   model.TunnelStatusRunning,
		"chain_id": "tunnel-backup-chain",
	}).Error; err != nil {
		t.Fatalf("mark backup running failed: %v", err)
	}

	_, err := svc.selectAvailableTunnel(rule)
	if err != errors.ErrTunnelFailoverUnavailable {
		t.Fatalf("expected different-entry backup to be ignored, got %v", err)
	}
}

func newRuleChainServiceTestFixture(t *testing.T) (*gorm.DB, *RuleService) {
	t.Helper()
	initObserverServiceTestLogger(t)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(&model.GostNode{}, &model.GostTunnel{}, &model.GostRule{}, &model.OperationLog{}); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}
	return db, NewRuleService(db)
}

func createRuleChainNode(t *testing.T, db *gorm.DB, name string) *model.GostNode {
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

func createRuleChainTunnel(t *testing.T, db *gorm.DB, name string, entryNodeID, exitNodeID uint) *model.GostTunnel {
	t.Helper()

	tunnel := &model.GostTunnel{
		Name:        name,
		EntryNodeID: entryNodeID,
		ExitNodeID:  exitNodeID,
		Protocol:    "ws",
		RelayPort:   8443,
		Hops: []model.TunnelHop{
			{NodeID: exitNodeID, Protocol: "ws", RelayPort: 8443},
		},
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}
	return tunnel
}

func createRuleChainTunnelWithHops(t *testing.T, db *gorm.DB, name string, entryNodeID uint, hops []model.TunnelHop) *model.GostTunnel {
	t.Helper()

	if len(hops) == 0 {
		t.Fatalf("hops are required")
	}
	last := hops[len(hops)-1]
	tunnel := &model.GostTunnel{
		Name:        name,
		EntryNodeID: entryNodeID,
		ExitNodeID:  last.NodeID,
		Protocol:    last.Protocol,
		RelayPort:   last.RelayPort,
		Hops:        hops,
		Status:      model.TunnelStatusRunning,
		ChainID:     "tunnel-" + name + "-chain",
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}
	return tunnel
}

func createRuleChainRule(t *testing.T, db *gorm.DB, tunnelID uint, listenPort int) *model.GostRule {
	t.Helper()

	rule := &model.GostRule{
		Name:            "rule",
		Type:            model.RuleTypeTunnel,
		TunnelID:        &tunnelID,
		PrimaryTunnelID: &tunnelID,
		ListenPort:      listenPort,
		Targets:         []string{"127.0.0.1:80"},
		Status:          model.RuleStatusStopped,
	}
	if err := db.Create(rule).Error; err != nil {
		t.Fatalf("create rule failed: %v", err)
	}
	return rule
}
