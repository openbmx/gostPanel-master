package service

import (
	"fmt"

	"gost-panel/internal/errors"
	"gost-panel/internal/model"
	"gost-panel/pkg/gost"
)

type tunnelRelayPlan struct {
	NodeID      uint
	Service     *gost.ServiceConfig
	EnableStats bool
}

type tunnelRuntimePlan struct {
	Chain  *gost.ChainConfig
	Relays []tunnelRelayPlan
}

func buildTunnelRuntimePlan(tunnel *model.GostTunnel, nodes map[uint]*model.GostNode) (*tunnelRuntimePlan, error) {
	hops := tunnel.EffectiveHops()
	if len(hops) == 0 {
		return nil, errors.ErrBadRequest
	}

	chain := &gost.ChainConfig{
		Name: fmt.Sprintf("tunnel-%d-chain", tunnel.ID),
		Hops: make([]*gost.HopConfig, 0, len(hops)),
	}
	relays := make([]tunnelRelayPlan, 0, len(hops))

	for i, hop := range hops {
		node := nodes[hop.NodeID]
		if node == nil || node.Address == "" {
			return nil, errors.ErrExtractHostFailed
		}

		isFinalHop := i == len(hops)-1
		relayName := fmt.Sprintf("relay-tunnel-%d-hop-%d", tunnel.ID, i)
		if isFinalHop {
			relayName = fmt.Sprintf("relay-tunnel-%d", tunnel.ID)
		}

		relays = append(relays, tunnelRelayPlan{
			NodeID: hop.NodeID,
			Service: &gost.ServiceConfig{
				Name: relayName,
				Addr: fmt.Sprintf(":%d", hop.RelayPort),
				Handler: &gost.HandlerConfig{
					Type: "relay",
				},
				Listener: &gost.ListenerConfig{
					Type: hop.Protocol,
				},
			},
			EnableStats: isFinalHop,
		})

		chain.Hops = append(chain.Hops, &gost.HopConfig{
			Name: fmt.Sprintf("hop-%d", i),
			Nodes: []*gost.NodeConfig{
				{
					Name: fmt.Sprintf("relay-hop-%d", i),
					Addr: fmt.Sprintf("%s:%d", node.Address, hop.RelayPort),
					Connector: &gost.ConnectorConfig{
						Type: "relay",
					},
					Dialer: &gost.DialerConfig{
						Type: hop.Protocol,
					},
				},
			},
		})
	}

	return &tunnelRuntimePlan{Chain: chain, Relays: relays}, nil
}
