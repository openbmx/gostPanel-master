// Package dto 定义数据传输对象
package dto

// ==================== 节点相关 ====================

// CreateNodeReq 创建节点请求
type CreateNodeReq struct {
	Name     string `json:"name" binding:"required,min=1,max=100"`       // 节点名称
	Address  string `json:"address" binding:"required,max=255"`          // IP 或域名
	Port     int    `json:"port" binding:"required,min=1,max=65535"`     // 端口
	Scheme   string `json:"scheme" binding:"omitempty,oneof=http https"` // 节点 API 协议，默认 http
	Username string `json:"username" binding:"max=50"`                   // API 认证用户名
	Password string `json:"password" binding:"max=255"`                  // API 认证密码
	Remark   string `json:"remark" binding:"max=1000"`                   // 备注
}

// UpdateNodeReq 更新节点请求
type UpdateNodeReq struct {
	Name    string `json:"name" binding:"required,min=1,max=100"`       // 节点名称
	Address string `json:"address" binding:"required,max=255"`          // IP 或域名
	Port    int    `json:"port" binding:"required,min=1,max=65535"`     // 端口
	Scheme  string `json:"scheme" binding:"omitempty,oneof=http https"` // 节点 API 协议
	// Username 留空表示不修改
	Username string `json:"username" binding:"max=50"`
	// Password 留空表示沿用原密码。
	// 安全：节点密码不再随列表/详情下发，前端无法回填，因此必须支持"留空即不改"，
	// 否则每次编辑其他字段都会把密码清空。
	Password string `json:"password" binding:"max=255"`
	Remark   string `json:"remark" binding:"max=1000"`
}

// NodeCredentialsResp 节点凭据响应（仅用于生成安装命令，调用会被审计）
type NodeCredentialsResp struct {
	ID       uint   `json:"id"`
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// NodeListReq 节点列表请求
type NodeListReq struct {
	Page     int    `form:"page" binding:"omitempty,min=1"`             // 页码
	PageSize int    `form:"pageSize" binding:"omitempty,min=1,max=100"` // 每页数量
	Status   string `form:"status"`                                     // 状态筛选
	Keyword  string `form:"keyword"`                                    // 关键词搜索
}

// SetDefaults 设置默认值
func (r *NodeListReq) SetDefaults() {
	if r.Page == 0 {
		r.Page = 1
	}
	if r.PageSize == 0 {
		r.PageSize = 10
	}
}
