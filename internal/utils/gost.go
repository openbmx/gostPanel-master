package utils

import (
	"fmt"
	"strings"
	"time"

	"gost-panel/internal/model"
	"gost-panel/pkg/gost"
)

// GetGostClient 根据节点配置创建 Gost 客户端
func GetGostClient(node *model.GostNode) *gost.Client {
	return gost.NewClient(&gost.Config{
		APIURL:   fmt.Sprintf("%s://%s:%d/api", NodeScheme(node), node.Address, node.Port),
		Username: node.Username,
		Password: node.Password,
		Timeout:  5 * time.Second,
	})
}

// NodeScheme 返回访问节点 API 应使用的协议。
// 安全：默认仍为 http 以兼容既有部署，但节点凭据会以明文跨网传输；
// 节点侧启用 TLS 后应把 scheme 改为 https。
func NodeScheme(node *model.GostNode) string {
	if strings.EqualFold(strings.TrimSpace(node.Scheme), "https") {
		return "https"
	}
	return "http"
}
