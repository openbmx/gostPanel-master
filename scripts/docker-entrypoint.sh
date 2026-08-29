#!/bin/sh
# Gost Panel 容器入口。
#
# 目的：让面板进程以非 root 身份运行，同时不破坏既有的 bind mount 部署。
#
# 直接在 Dockerfile 里写 USER 会有一个现实问题：docker-compose 用
# `./data:/app/data` 这类 bind mount 时，宿主目录通常属于 root，
# 非 root 进程无法写入，升级后面板直接起不来。
#
# 这里采用官方镜像（postgres/redis 等）的通用做法：入口脚本以 root 启动，
# 仅把数据目录的属主修正为应用用户，然后用 su-exec 降权执行真正的进程。
# root 权限只存在于这几行 chown 期间，面板本身全程非特权运行。
set -e

APP_USER="${GOSTPANEL_USER:-gostpanel}"
APP_GROUP="${GOSTPANEL_GROUP:-gostpanel}"

if [ "$(id -u)" = "0" ]; then
    expected_owner="$(id -u "$APP_USER")"

    # 迁移旧版本遗留在 /app 下的数据库。
    #
    # 必须在这里、以 root 身份完成：老容器以 root 运行，/app/gost-panel.db 属主是
    # root 且 /app 目录不可被应用用户写入，降权后的进程无法移动它，
    # 程序内的迁移逻辑会失败并直接终止启动。
    if [ -f /app/gost-panel.db ] && [ ! -f /app/data/gost-panel.db ]; then
        echo "[entrypoint] 迁移旧数据库 /app/gost-panel.db -> /app/data/（该位置位于持久化卷内）"
        mkdir -p /app/data
        mv /app/gost-panel.db /app/data/gost-panel.db
        for suffix in -wal -shm; do
            [ -f "/app/gost-panel.db${suffix}" ] && mv "/app/gost-panel.db${suffix}" "/app/data/gost-panel.db${suffix}"
        done
    fi

    for dir in /app/data /app/logs /app/backups; do
        [ -d "$dir" ] || mkdir -p "$dir"
        # 只在属主不符时才递归 chown，避免每次启动都遍历大目录
        current_owner="$(stat -c '%u' "$dir" 2>/dev/null || echo '')"
        if [ "$current_owner" != "$expected_owner" ]; then
            echo "[entrypoint] 修正 $dir 的属主为 ${APP_USER}"
            chown -R "${APP_USER}:${APP_GROUP}" "$dir"
        fi
    done

    # 配置文件保持只读即可，属主不改（可能由运维挂载并自行管理权限）
    exec su-exec "${APP_USER}:${APP_GROUP}" "$@"
fi

# 已经是非 root（例如 compose 里显式指定了 user:），直接执行
exec "$@"
