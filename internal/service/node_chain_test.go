package service

import (
	"testing"

	"gost-panel/internal/errors"
	"gost-panel/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestNodeServiceDeleteRejectsNodeUsedByTunnelHop(t *testing.T) {
	db, svc := newNodeChainServiceTestFixture(t)
	entry := createNodeChainNode(t, db, "entry")
	middle := createNodeChainNode(t, db, "middle")
	exit := createNodeChainNode(t, db, "exit")

	tunnel := &model.GostTunnel{
		Name:        "chain",
		EntryNodeID: entry.ID,
		ExitNodeID:  exit.ID,
		Protocol:    "ws",
		RelayPort:   9002,
		Hops: []model.TunnelHop{
			{NodeID: middle.ID, Protocol: "ws", RelayPort: 9001},
			{NodeID: exit.ID, Protocol: "ws", RelayPort: 9002},
		},
	}
	if err := db.Create(tunnel).Error; err != nil {
		t.Fatalf("create tunnel failed: %v", err)
	}

	err := svc.Delete(middle.ID, 1, "admin", "127.0.0.1", "test")
	if err != errors.ErrNodeHasTunnels {
		t.Fatalf("expected node used by hop to be rejected with ErrNodeHasTunnels, got %v", err)
	}
}

func newNodeChainServiceTestFixture(t *testing.T) (*gorm.DB, *NodeService) {
	t.Helper()
	initObserverServiceTestLogger(t)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(&model.GostNode{}, &model.GostTunnel{}, &model.GostRule{}, &model.OperationLog{}); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}
	return db, NewNodeService(db)
}

func createNodeChainNode(t *testing.T, db *gorm.DB, name string) *model.GostNode {
	t.Helper()

	node := &model.GostNode{
		Name:    name,
		Address: "10.0.0.1",
		Port:    39000,
		Status:  model.NodeStatusOnline,
	}
	if err := db.Create(node).Error; err != nil {
		t.Fatalf("create node failed: %v", err)
	}
	return node
}
