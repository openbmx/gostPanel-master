package dto

// ==================== 版本与在线更新 ====================

// VersionResp 当前版本与运行环境
type VersionResp struct {
	Version string `json:"version"`
	// RestartSupported 当前平台能否由面板触发自动重启
	RestartSupported bool `json:"restart_supported"`
}

// RollbackReq 回滚请求。
// Version 留空表示回滚到上一次更新前保留的本地备份；
// 填写时必须是 /system/update/rollback-versions 返回的版本之一。
type RollbackReq struct {
	Version string `json:"version" binding:"omitempty,max=32"`
}

// UpdateResultResp 更新/回滚的结果
type UpdateResultResp struct {
	NewVersion       string `json:"new_version,omitempty"`
	NeedRestart      bool   `json:"need_restart"`
	RestartSupported bool   `json:"restart_supported"`
}
