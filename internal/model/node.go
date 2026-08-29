package model

import (
	"time"

	"gorm.io/gorm"
)

// NodeStatus 节点状态
type NodeStatus string

const (
	NodeStatusOnline  NodeStatus = "online"  // 在线
	NodeStatusOffline NodeStatus = "offline" // 离线
	NodeStatusError   NodeStatus = "error"   // 错误
)

// GostNode Gost 节点模型
type GostNode struct {
	ID      uint   `gorm:"primaryKey" json:"id"`
	Name    string `gorm:"size:100;not null" json:"name"`    // 节点名称
	Address string `gorm:"size:255;not null" json:"address"` // IP 或域名
	Port    int    `gorm:"not null" json:"port"`             // 端口

	// Scheme 调用节点 GOST API 使用的协议，http 或 https。
	// 安全：节点 API 用 Basic Auth 认证，走 http 意味着凭据以明文跨公网传输。
	// 若节点侧已配置 TLS，应设为 https。
	Scheme string `gorm:"size:10;default:http" json:"scheme"`

	Username string `gorm:"size:50" json:"username"` // API 认证用户名
	// Password 节点 GOST API 的认证密码。
	// 安全：绝不随节点列表/详情下发 —— 它等同于该主机 GOST 守护进程的完全控制权
	// （可通过 /config API 创建任意转发与代理服务）。
	// 需要展示时走独立的 /nodes/:id/credentials 接口，并留审计日志。
	Password string `gorm:"size:255" json:"-"`

	Status NodeStatus `gorm:"size:20;default:offline" json:"status"` // 状态

	// 流量统计
	TotalBytes  int64 `gorm:"default:0" json:"total_bytes"`
	InputBytes  int64 `gorm:"default:0" json:"input_bytes"`
	OutputBytes int64 `gorm:"default:0" json:"output_bytes"`

	// Gost上报的累计值（用于计算增量）
	LastReportedInputBytes  int64 `gorm:"default:0" json:"-"` // 上次上报的入站累计值
	LastReportedOutputBytes int64 `gorm:"default:0" json:"-"` // 上次上报的出站累计值

	LastCheckAt *time.Time     `json:"last_check_at"`           // 最后检查时间
	Remark      string         `gorm:"type:text" json:"remark"` // 备注
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`

	// 关联 - 规则
	Rules []GostRule `gorm:"foreignKey:NodeID" json:"rules,omitempty"`

	// 关联 - 隧道（作为入口或出口节点）
	EntryTunnels []GostTunnel `gorm:"foreignKey:EntryNodeID" json:"entry_tunnels,omitempty"`
	ExitTunnels  []GostTunnel `gorm:"foreignKey:ExitNodeID" json:"exit_tunnels,omitempty"`
}

// TableName 指定表名
func (GostNode) TableName() string {
	return "nodes"
}
