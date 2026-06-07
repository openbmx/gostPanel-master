package service

import (
	"sync"
	"testing"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/pkg/logger"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

var tunnelChainLoggerOnce sync.Once

func TestTunnelRuntimePlanBuildsOrderedMultiHopChain(t *testing.T) {
	tunnel := &model.GostTunnel{
		ID:          42,
		EntryNodeID: 1,
		Hops: []model.TunnelHop{
			{NodeID: 2, Protocol: "ws", RelayPort: 9001},
			{NodeID: 3, Protocol: "tls", RelayPort: 9002},
		},
	}
	nodes := map[uint]*model.GostNode{
		2: {ID: 2, Name: "middle", Address: "10.0.0.2"},
		3: {ID: 3, Name: "exit", Address: "10.0.0.3"},
	}

	plan, err := buildTunnelRuntimePlan(tunnel, nodes)
	if err != nil {
		t.Fatalf("build runtime plan failed: %v", err)
	}

	if plan.Chain.Name != "tunnel-42-chain" {
		t.Fatalf("unexpected chain name: %s", plan.Chain.Name)
	}
	if len(plan.Chain.Hops) != 2 {
		t.Fatalf("expected two chain hops, got %d", len(plan.Chain.Hops))
	}
	if plan.Chain.Hops[0].Nodes[0].Addr != "10.0.0.2:9001" || plan.Chain.Hops[0].Nodes[0].Dialer.Type != "ws" {
		t.Fatalf("unexpected first chain node: %+v", plan.Chain.Hops[0].Nodes[0])
	}
	if plan.Chain.Hops[1].Nodes[0].Addr != "10.0.0.3:9002" || plan.Chain.Hops[1].Nodes[0].Dialer.Type != "tls" {
		t.Fatalf("unexpected second chain node: %+v", plan.Chain.Hops[1].Nodes[0])
	}

	if len(plan.Relays) != 2 {
		t.Fatalf("expected two relay services, got %d", len(plan.Relays))
	}
	if plan.Relays[0].Service.Name != "relay-tunnel-42-hop-0" || plan.Relays[0].EnableStats {
		t.Fatalf("unexpected intermediate relay plan: %+v", plan.Relays[0])
	}
	if plan.Relays[1].Service.Name != "relay-tunnel-42" || !plan.Relays[1].EnableStats {
		t.Fatalf("unexpected final relay plan: %+v", plan.Relays[1])
	}
}

func TestTunnelRuntimePlanPreservesLegacySingleHopRelayName(t *testing.T) {
	tunnel := &model.GostTunnel{
		ID:          7,
		EntryNodeID: 1,
		ExitNodeID:  2,
		Protocol:    "ws",
		RelayPort:   8443,
	}
	nodes := map[uint]*model.GostNode{
		2: {ID: 2, Name: "exit", Address: "10.0.0.2"},
	}

	plan, err := buildTunnelRuntimePlan(tunnel, nodes)
	if err != nil {
		t.Fatalf("build runtime plan failed: %v", err)
	}

	if len(plan.Relays) != 1 {
		t.Fatalf("expected one relay service, got %d", len(plan.Relays))
	}
	if plan.Relays[0].Service.Name != "relay-tunnel-7" {
		t.Fatalf("unexpected legacy relay name: %s", plan.Relays[0].Service.Name)
	}
	if len(plan.Chain.Hops) != 1 || plan.Chain.Hops[0].Nodes[0].Addr != "10.0.0.2:8443" {
		t.Fatalf("unexpected legacy chain: %+v", plan.Chain)
	}
}

func newTunnelChainServiceTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	var loggerErr error
	tunnelChainLoggerOnce.Do(func() {
		loggerErr = logger.Init(&logger.Config{
			Level:  "debug",
			Format: "console",
			Output: "stdout",
		})
	})
	if loggerErr != nil {
		t.Fatalf("init logger failed: %v", loggerErr)
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(&model.GostNode{}, &model.GostTunnel{}, &model.OperationLog{}); err != nil {
		t.Fatalf("auto migrate failed: %v", err)
	}
	return db
}

func createTunnelChainNode(t *testing.T, db *gorm.DB, name string) *model.GostNode {
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

func TestTunnelServiceCreateStoresExplicitOrderedHopsAndMirrorsLastHop(t *testing.T) {
	db := newTunnelChainServiceTestDB(t)
	svc := NewTunnelService(db)
	entry := createTunnelChainNode(t, db, "entry")
	middle := createTunnelChainNode(t, db, "middle")
	exit := createTunnelChainNode(t, db, "exit")

	tunnel, err := svc.Create(&dto.CreateTunnelReq{
		Name:        "chain",
		EntryNodeID: entry.ID,
		Hops: []dto.TunnelHopReq{
			{NodeID: middle.ID, Protocol: "ws", RelayPort: 9001},
			{NodeID: exit.ID, Protocol: "tls", RelayPort: 9002},
		},
	}, 1, "admin", "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("create chain tunnel failed: %v", err)
	}

	if tunnel.ExitNodeID != exit.ID || tunnel.Protocol != "tls" || tunnel.RelayPort != 9002 {
		t.Fatalf("last hop should be mirrored to legacy fields: %+v", tunnel)
	}
	if len(tunnel.Hops) != 2 || tunnel.Hops[0].NodeID != middle.ID || tunnel.Hops[1].NodeID != exit.ID {
		t.Fatalf("unexpected stored hops: %+v", tunnel.Hops)
	}
}

func TestTunnelServiceCreateKeepsLegacySingleHopRequestCompatible(t *testing.T) {
	db := newTunnelChainServiceTestDB(t)
	svc := NewTunnelService(db)
	entry := createTunnelChainNode(t, db, "entry")
	exit := createTunnelChainNode(t, db, "exit")

	tunnel, err := svc.Create(&dto.CreateTunnelReq{
		Name:        "legacy",
		EntryNodeID: entry.ID,
		ExitNodeID:  exit.ID,
		Protocol:    "ws",
		RelayPort:   8443,
	}, 1, "admin", "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("create legacy tunnel failed: %v", err)
	}

	if len(tunnel.Hops) != 1 || tunnel.Hops[0].NodeID != exit.ID || tunnel.Hops[0].Protocol != "ws" || tunnel.Hops[0].RelayPort != 8443 {
		t.Fatalf("legacy request should be normalized to one hop: %+v", tunnel.Hops)
	}
}

func TestTunnelServiceCreateRejectsRepeatedHopNodes(t *testing.T) {
	db := newTunnelChainServiceTestDB(t)
	svc := NewTunnelService(db)
	entry := createTunnelChainNode(t, db, "entry")
	hop := createTunnelChainNode(t, db, "hop")

	_, err := svc.Create(&dto.CreateTunnelReq{
		Name:        "bad-chain",
		EntryNodeID: entry.ID,
		Hops: []dto.TunnelHopReq{
			{NodeID: hop.ID, Protocol: "ws", RelayPort: 9001},
			{NodeID: hop.ID, Protocol: "ws", RelayPort: 9002},
		},
	}, 1, "admin", "127.0.0.1", "test")
	if err == nil {
		t.Fatalf("expected repeated hop nodes to be rejected")
	}
	if err != errors.ErrBadRequest {
		t.Fatalf("expected bad request, got %v", err)
	}
}

func TestTunnelServiceListFiltersByExplicitHopNodeBeforePagination(t *testing.T) {
	db := newTunnelChainServiceTestDB(t)
	svc := NewTunnelService(db)
	entry := createTunnelChainNode(t, db, "entry")
	middle := createTunnelChainNode(t, db, "middle")
	exit := createTunnelChainNode(t, db, "exit")

	for i := 0; i < 12; i++ {
		tunnel := &model.GostTunnel{
			Name:        "chain",
			EntryNodeID: entry.ID,
			ExitNodeID:  exit.ID,
			Protocol:    "ws",
			RelayPort:   9100 + i,
			Hops: []model.TunnelHop{
				{NodeID: middle.ID, Protocol: "ws", RelayPort: 9001 + i},
				{NodeID: exit.ID, Protocol: "ws", RelayPort: 9100 + i},
			},
		}
		if err := db.Create(tunnel).Error; err != nil {
			t.Fatalf("create tunnel failed: %v", err)
		}
	}

	tunnels, total, err := svc.List(&dto.TunnelListReq{
		Page:     2,
		PageSize: 5,
		NodeID:   middle.ID,
	})
	if err != nil {
		t.Fatalf("list tunnels failed: %v", err)
	}
	if total != 12 {
		t.Fatalf("expected total 12, got %d", total)
	}
	if len(tunnels) != 5 {
		t.Fatalf("expected second page with 5 tunnels, got %d", len(tunnels))
	}
}
