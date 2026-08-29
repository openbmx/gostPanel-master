package handler

import (
	"context"
	"time"

	"gost-panel/internal/config"
	"gost-panel/internal/dto"
	"gost-panel/internal/model"
	"gost-panel/internal/service"
	"gost-panel/internal/utils"
	"gost-panel/pkg/logger"
	"gost-panel/pkg/response"
	"gost-panel/pkg/sysutil"

	"github.com/gin-gonic/gin"
)

// updateTimeout 一次完整更新的时间上限：拉取发布信息 + 在慢速链路上下载数十 MB。
// 必须大于 GitHub 下载客户端自身的 10 分钟超时，让下载持有自己的截止时间。
const updateTimeout = 15 * time.Minute

// restartDelay 重启前留给 HTTP 响应送达客户端的时间
const restartDelay = 500 * time.Millisecond

// UpdateHandler 版本与在线更新控制器
type UpdateHandler struct {
	updateService *service.UpdateService
	logService    *service.LogService
}

// NewUpdateHandler 创建控制器
func NewUpdateHandler(updateService *service.UpdateService, logService *service.LogService) *UpdateHandler {
	return &UpdateHandler{updateService: updateService, logService: logService}
}

// detachedContext 把长耗时任务与 HTTP 请求生命周期解耦。
//
// 浏览器与反向代理普遍在 30-60 秒后中断空闲请求（axios 默认超时、
// nginx 的 proxy_read_timeout）。若直接使用 c.Request.Context()，
// 下载会在中途被取消，更新以 "context canceled" 失败 —— 而此时二进制
// 可能已经处于半替换状态。这里保留请求上的值但剥离取消信号，
// 客户端断开不影响更新继续跑完。
func detachedContext(c *gin.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if c != nil && c.Request != nil {
		base = context.WithoutCancel(c.Request.Context())
	}
	return context.WithTimeout(base, updateTimeout)
}

func (h *UpdateHandler) audit(c *gin.Context, action, details string) {
	userID, _ := c.Get("userID")
	username, _ := c.Get("username")

	id, _ := userID.(uint)
	name, _ := username.(string)

	h.logService.Record(
		id, name, action, model.ResourceTypeSystem, 0, details,
		utils.ClientIP(c), c.GetHeader("User-Agent"))
}

// GetVersion 返回当前版本与运行环境信息
// GET /api/v1/system/version
func (h *UpdateHandler) GetVersion(c *gin.Context) {
	response.Success(c, dto.VersionResp{
		Version:          config.Version,
		RestartSupported: sysutil.RestartSupported(),
	})
}

// CheckUpdate 检查是否有可用更新
// GET /api/v1/system/update/check?force=true
func (h *UpdateHandler) CheckUpdate(c *gin.Context) {
	force := c.Query("force") == "true"

	// 检查更新只是一次 GitHub API 调用，用请求自身的上下文即可
	info, err := h.updateService.CheckUpdate(c.Request.Context(), force)
	if err != nil {
		response.HandleError(c, err)
		return
	}
	response.Success(c, info)
}

// PerformUpdate 升级到最新版本
// POST /api/v1/system/update
func (h *UpdateHandler) PerformUpdate(c *gin.Context) {
	ctx, cancel := detachedContext(c)
	defer cancel()

	newVersion, err := h.updateService.PerformUpdate(ctx)
	if err != nil {
		h.audit(c, model.ActionSystemUpdate, "在线更新失败: "+err.Error())
		response.HandleError(c, err)
		return
	}

	h.audit(c, model.ActionSystemUpdate, "在线更新成功: "+config.Version+" -> "+newVersion)
	logger.Warnf("面板二进制已更新到 %s（操作者 IP: %s），需重启生效", newVersion, utils.ClientIP(c))

	response.SuccessWithMessage(c, "更新完成，需要重启面板才能生效", dto.UpdateResultResp{
		NewVersion:       newVersion,
		NeedRestart:      true,
		RestartSupported: sysutil.RestartSupported(),
	})
}

// ListRollbackVersions 列出可回滚的历史版本
// GET /api/v1/system/update/rollback-versions
func (h *UpdateHandler) ListRollbackVersions(c *gin.Context) {
	versions, err := h.updateService.ListRollbackVersions(c.Request.Context())
	if err != nil {
		response.HandleError(c, err)
		return
	}
	response.Success(c, versions)
}

// Rollback 回滚。请求体不带 version 时恢复上一次更新前的备份；
// 带 version 时下载并安装该历史版本。
// POST /api/v1/system/update/rollback
func (h *UpdateHandler) Rollback(c *gin.Context) {
	var req dto.RollbackReq
	// 允许空请求体：不带版本号即表示回滚到本地备份
	_ = c.ShouldBindJSON(&req)

	var err error
	var target string

	if req.Version == "" {
		target = "本地备份"
		err = h.updateService.RollbackToBackup()
	} else {
		target = req.Version
		ctx, cancel := detachedContext(c)
		defer cancel()
		err = h.updateService.RollbackToVersion(ctx, req.Version)
	}

	if err != nil {
		h.audit(c, model.ActionSystemRollback, "回滚到 "+target+" 失败: "+err.Error())
		response.HandleError(c, err)
		return
	}

	h.audit(c, model.ActionSystemRollback, "已回滚到 "+target)
	logger.Warnf("面板二进制已回滚到 %s（操作者 IP: %s），需重启生效", target, utils.ClientIP(c))

	response.SuccessWithMessage(c, "回滚完成，需要重启面板才能生效", dto.UpdateResultResp{
		NewVersion:       req.Version,
		NeedRestart:      true,
		RestartSupported: sysutil.RestartSupported(),
	})
}

// Restart 重启面板服务
// POST /api/v1/system/restart
func (h *UpdateHandler) Restart(c *gin.Context) {
	if !sysutil.RestartSupported() {
		response.BadRequest(c, "当前平台不支持自动重启，请手动重启面板服务")
		return
	}

	h.audit(c, model.ActionSystemRestart, "手动触发面板重启")
	logger.Warnf("收到重启请求（操作者 IP: %s）", utils.ClientIP(c))

	// 先返回响应，再退出进程，否则客户端只会看到连接被重置
	response.SuccessWithMessage(c, "重启指令已下达，面板将在数秒内恢复", nil)
	sysutil.RestartAsync(restartDelay)
}
