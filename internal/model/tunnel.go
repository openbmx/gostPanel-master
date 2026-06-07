package model

import (
	"time"

	"gorm.io/gorm"
)

type TunnelStatus string

const (
	TunnelStatusStopped TunnelStatus = "stopped"
	TunnelStatusRunning TunnelStatus = "running"
	TunnelStatusError   TunnelStatus = "error"
)

// TunnelHop describes one relay hop in an ordered tunnel chain.
type TunnelHop struct {
	NodeID    uint   `json:"node_id"`
	Protocol  string `json:"protocol"`
	RelayPort int    `json:"relay_port"`
}

// GostTunnel manages a GOST chain from an entry node through one or more relay hops.
type GostTunnel struct {
	ID          uint         `gorm:"primaryKey" json:"id"`
	Name        string       `gorm:"size:100;not null" json:"name"`
	EntryNodeID uint         `gorm:"not null;index" json:"entry_node_id"`
	ExitNodeID  uint         `gorm:"not null;index" json:"exit_node_id"`
	Protocol    string       `gorm:"size:10;default:tcp" json:"protocol"`
	RelayPort   int          `gorm:"default:8443" json:"relay_port"`
	Hops        []TunnelHop  `gorm:"type:json;serializer:json" json:"hops"`
	Status      TunnelStatus `gorm:"size:20;default:stopped" json:"status"`

	ServiceID string `gorm:"size:100" json:"service_id"`
	ChainID   string `gorm:"size:100" json:"chain_id"`

	InputBytes  int64 `gorm:"default:0" json:"input_bytes"`
	OutputBytes int64 `gorm:"default:0" json:"output_bytes"`
	TotalBytes  int64 `gorm:"default:0" json:"total_bytes"`

	LastReportedInputBytes  int64          `gorm:"default:0" json:"-"`
	LastReportedOutputBytes int64          `gorm:"default:0" json:"-"`
	Remark                  string         `gorm:"type:text" json:"remark"`
	CreatedAt               time.Time      `json:"created_at"`
	UpdatedAt               time.Time      `json:"updated_at"`
	DeletedAt               gorm.DeletedAt `gorm:"index" json:"-"`

	EntryNode *GostNode  `gorm:"foreignKey:EntryNodeID" json:"entry_node,omitempty"`
	ExitNode  *GostNode  `gorm:"foreignKey:ExitNodeID" json:"exit_node,omitempty"`
	Rules     []GostRule `gorm:"foreignKey:TunnelID" json:"rules,omitempty"`
}

func (GostTunnel) TableName() string {
	return "tunnels"
}

// EffectiveHops returns explicit ordered hops, or legacy exit fields as one hop.
func (t *GostTunnel) EffectiveHops() []TunnelHop {
	if t == nil {
		return nil
	}

	if len(t.Hops) > 0 {
		hops := make([]TunnelHop, 0, len(t.Hops))
		for _, hop := range t.Hops {
			if hop.NodeID == 0 {
				continue
			}
			if hop.Protocol == "" {
				hop.Protocol = t.Protocol
			}
			if hop.RelayPort == 0 {
				hop.RelayPort = t.RelayPort
			}
			hops = append(hops, hop)
		}
		return hops
	}

	if t.ExitNodeID == 0 {
		return nil
	}
	return []TunnelHop{{
		NodeID:    t.ExitNodeID,
		Protocol:  t.Protocol,
		RelayPort: t.RelayPort,
	}}
}

// LastHop returns the final effective hop.
func (t *GostTunnel) LastHop() (TunnelHop, bool) {
	hops := t.EffectiveHops()
	if len(hops) == 0 {
		return TunnelHop{}, false
	}
	return hops[len(hops)-1], true
}

// UsesNode reports whether the tunnel references the node.
func (t *GostTunnel) UsesNode(nodeID uint) bool {
	if t == nil || nodeID == 0 {
		return false
	}
	if t.EntryNodeID == nodeID || t.ExitNodeID == nodeID {
		return true
	}
	for _, hop := range t.Hops {
		if hop.NodeID == nodeID {
			return true
		}
	}
	return false
}
