package dto

type TunnelHopReq struct {
	NodeID    uint   `json:"node_id" binding:"required"`
	Protocol  string `json:"protocol" binding:"required,oneof=tcp udp tls mtls ws mws wss mwss h2 grpc quic kcp ssh"`
	RelayPort int    `json:"relay_port" binding:"required,min=1,max=65535"`
}

type CreateTunnelReq struct {
	Name        string         `json:"name" binding:"required,min=1,max=100"`
	EntryNodeID uint           `json:"entry_node_id" binding:"required"`
	ExitNodeID  uint           `json:"exit_node_id"`
	Protocol    string         `json:"protocol" binding:"omitempty,oneof=tcp udp tls mtls ws mws wss mwss h2 grpc quic kcp ssh"`
	RelayPort   int            `json:"relay_port" binding:"omitempty,min=1,max=65535"`
	Hops        []TunnelHopReq `json:"hops"`
	Remark      string         `json:"remark"`
}

type UpdateTunnelReq struct {
	Name      string         `json:"name" binding:"required,min=1,max=100"`
	Protocol  string         `json:"protocol" binding:"omitempty,oneof=tcp udp tls mtls ws mws wss mwss h2 grpc quic kcp ssh"`
	RelayPort int            `json:"relay_port" binding:"omitempty,min=1,max=65535"`
	Hops      []TunnelHopReq `json:"hops"`
	Remark    string         `json:"remark"`
}

type TunnelListReq struct {
	Page     int    `form:"page" binding:"omitempty,min=1"`
	PageSize int    `form:"pageSize" binding:"omitempty,min=1,max=100"`
	NodeID   uint   `form:"node_id"`
	Status   string `form:"status"`
	Keyword  string `form:"keyword"`
}

func (r *TunnelListReq) SetDefaults() {
	if r.Page == 0 {
		r.Page = 1
	}
	if r.PageSize == 0 {
		r.PageSize = 10
	}
}
