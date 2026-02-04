package handler

import (
	"bytes"
	"gost-panel/internal/dto"
	"gost-panel/internal/service"
	"gost-panel/pkg/logger"
	"io"

	"github.com/gin-gonic/gin"
)

// ObserverHandler 观察器控制器
type ObserverHandler struct {
	observerService *service.ObserverService
}

// NewObserverHandler 创建观察器控制器
func NewObserverHandler(observerService *service.ObserverService) *ObserverHandler {
	return &ObserverHandler{observerService: observerService}
}

// Report 接收 GOST 观察器上报的数据
// POST /api/v1/observer/report
func (h *ObserverHandler) Report(c *gin.Context) {
	// 1. 读取原始 Body
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Warnf("读取观察器上报数据失败: %v", err)
		c.JSON(400, dto.ObserverReportResp{OK: false})
		return
	}

	// 2. 恢复 Body 供 binder 使用
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// 3. 打印原始数据（调试用，确认接收到的内容）
	logger.Infof("观察器上报原始数据: %s", string(bodyBytes))

	var req dto.ObserverReportReq
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.Warnf("解析观察器上报数据失败: %v", err)
		c.JSON(400, dto.ObserverReportResp{OK: false})
		return
	}

	// 简单的调试日志：只在有非零流量时打印，避免日志爆炸
	for _, event := range req.Events {
		if event.Type == "stats" && event.Stats != nil {
			if event.Stats.InputBytes > 0 || event.Stats.OutputBytes > 0 {
				logger.Infof("收到有效流量: Service=%s, In=%d, Out=%d", event.Service, event.Stats.InputBytes, event.Stats.OutputBytes)
			}
		}
	}

	// 处理上报数据
	if err := h.observerService.HandleReport(&req); err != nil {
		logger.Warnf("处理观察器上报数据失败: %v", err)
		c.JSON(500, dto.ObserverReportResp{OK: false})
		return
	}

	// 返回成功响应（GOST 需要 ok: true 才认为上报成功）
	c.JSON(200, dto.ObserverReportResp{OK: true})
}
