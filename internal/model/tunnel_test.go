package model

import "testing"

func TestGostTunnelEffectiveHopsUsesLegacyExitFields(t *testing.T) {
	tunnel := &GostTunnel{
		EntryNodeID: 1,
		ExitNodeID:  2,
		Protocol:    "ws",
		RelayPort:   8443,
	}

	hops := tunnel.EffectiveHops()
	if len(hops) != 1 {
		t.Fatalf("expected one legacy hop, got %d", len(hops))
	}
	if hops[0].NodeID != 2 || hops[0].Protocol != "ws" || hops[0].RelayPort != 8443 {
		t.Fatalf("unexpected legacy hop: %+v", hops[0])
	}
}

func TestGostTunnelEffectiveHopsUsesExplicitOrderedHops(t *testing.T) {
	tunnel := &GostTunnel{
		EntryNodeID: 1,
		Protocol:    "ws",
		RelayPort:   8443,
		Hops: []TunnelHop{
			{NodeID: 2, Protocol: "tls", RelayPort: 9001},
			{NodeID: 3, Protocol: "grpc", RelayPort: 9002},
		},
	}

	hops := tunnel.EffectiveHops()
	if len(hops) != 2 {
		t.Fatalf("expected two explicit hops, got %d", len(hops))
	}
	if hops[0].NodeID != 2 || hops[0].Protocol != "tls" || hops[0].RelayPort != 9001 {
		t.Fatalf("unexpected first hop: %+v", hops[0])
	}
	if hops[1].NodeID != 3 || hops[1].Protocol != "grpc" || hops[1].RelayPort != 9002 {
		t.Fatalf("unexpected second hop: %+v", hops[1])
	}
}

func TestGostTunnelEffectiveHopsAppliesDefaultsToExplicitHops(t *testing.T) {
	tunnel := &GostTunnel{
		EntryNodeID: 1,
		Protocol:    "ws",
		RelayPort:   8443,
		Hops: []TunnelHop{
			{NodeID: 2},
		},
	}

	hops := tunnel.EffectiveHops()
	if len(hops) != 1 {
		t.Fatalf("expected one hop, got %d", len(hops))
	}
	if hops[0].Protocol != "ws" || hops[0].RelayPort != 8443 {
		t.Fatalf("expected default protocol and relay port, got %+v", hops[0])
	}
}

func TestGostTunnelUsesNodeIncludesEntryLegacyExitAndExplicitHops(t *testing.T) {
	tunnel := &GostTunnel{
		EntryNodeID: 1,
		ExitNodeID:  2,
		Hops: []TunnelHop{
			{NodeID: 3, Protocol: "ws", RelayPort: 9001},
		},
	}

	for _, nodeID := range []uint{1, 2, 3} {
		if !tunnel.UsesNode(nodeID) {
			t.Fatalf("expected tunnel to use node %d", nodeID)
		}
	}
	if tunnel.UsesNode(4) {
		t.Fatalf("did not expect tunnel to use node 4")
	}
}
