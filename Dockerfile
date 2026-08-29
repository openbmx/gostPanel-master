# 前端构建阶段
FROM node:20-alpine AS web-builder
WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci
COPY web/ .
RUN npm run build

# 后端构建阶段
FROM golang:1.23-alpine AS backend-builder
WORKDIR /app

# 复制依赖文件
COPY go.mod go.sum ./
RUN go mod download

# 复制后端源码
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
COPY config/ config/

# 从前端构建阶段复制 dist 到后端 embedding 路径
# 注意：前端构建输出到了 ../internal/router/dist，即 /app/internal/router/dist
COPY --from=web-builder /app/internal/router/dist ./internal/router/dist

# 编译（关闭 CGO，使用纯 Go SQLite 驱动）
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" -o gost-panel cmd/server/main.go

# 运行阶段
FROM alpine:latest
WORKDIR /app

# su-exec 用于入口脚本在修正数据目录属主后降权执行面板进程
RUN apk --no-cache add ca-certificates tzdata su-exec
ENV TZ=Asia/Shanghai

# 安全：面板不需要 root。它被攻破等同于拿到所有受管节点的转发控制权，
# 若再叠加容器内 root，攻击面会进一步扩大到容器逃逸类问题。
RUN addgroup -g 1000 gostpanel && \
    adduser -u 1000 -G gostpanel -s /bin/sh -D gostpanel

# 复制程序和配置
COPY --from=backend-builder /app/gost-panel .
COPY --from=backend-builder /app/config ./config
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# 创建运行时目录并交给应用用户；配置目录保持只读
RUN mkdir -p /app/data /app/logs /app/backups && \
    chown -R gostpanel:gostpanel /app/data /app/logs /app/backups && \
    chmod 700 /app/data /app/backups && \
    chmod 750 /app/logs && \
    chmod +x /usr/local/bin/docker-entrypoint.sh /app/gost-panel

# 面板默认监听 :39100（见 config/config.yaml）
EXPOSE 39100

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:39100/api/v1/health || exit 1

# 入口脚本以 root 启动，仅修正 bind mount 目录属主后即用 su-exec 降权。
# 面板进程本身全程以 gostpanel(uid 1000) 运行。
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["./gost-panel"]
