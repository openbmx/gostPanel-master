#!/usr/bin/env bash
# ========================================================
#  Gost 节点（客户端/被控端）升级脚本
#  - 仅替换 gost 二进制，完整保留节点配置（含 API 账号）
#  - 升级前自动备份旧二进制与配置
#  - 升级后健康检查，失败自动回滚
#  - 兼容 systemd 与 OpenRC
#  官方 GOST: https://github.com/go-gost/gost
# ========================================================

set -eo pipefail

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
PLAIN='\033[0m'

# 配置（与 install_node.sh 保持一致，可用环境变量覆盖）
BASE_PATH="/etc/gost-node"
BIN_PATH="/usr/local/bin/gost-node"
CONF_FILE="${BASE_PATH}/config.yaml"
GOST_VERSION="${GOST_VERSION:-3.2.6}"      # 目标版本，可通过环境变量覆盖
GH_PROXY="${GH_PROXY:-}"                    # 可选 GitHub 加速前缀
SERVICE_NAME="gost-node"
HEALTH_TIMEOUT=20

info() { echo -e "${BLUE}[信息]${PLAIN} $1"; }
ok()   { echo -e "${GREEN}[成功]${PLAIN} $1"; }
warn() { echo -e "${YELLOW}[警告]${PLAIN} $1"; }
err()  { echo -e "${RED}[错误]${PLAIN} $1"; }

echo -e "${GREEN}========================================${PLAIN}"
echo -e "${GREEN}  Gost 节点升级 (目标 v${GOST_VERSION})${PLAIN}"
echo -e "${GREEN}========================================${PLAIN}\n"

BACKUP_BIN=""
CONF_BACKUP=""
TMP_DIR=""
SERVICE_MGR=""   # systemd | openrc

cleanup() {
    [ -n "$TMP_DIR" ] && rm -rf "$TMP_DIR" 2>/dev/null || true
}
trap cleanup EXIT

check_root() {
    if [[ $EUID -ne 0 ]]; then
        err "请使用 root 用户运行此脚本"
        exit 1
    fi
}

# 检测服务管理器
detect_service_mgr() {
    if command -v systemctl >/dev/null 2>&1 && [ -f "/etc/systemd/system/${SERVICE_NAME}.service" ]; then
        SERVICE_MGR="systemd"
    elif [ -f "/etc/init.d/${SERVICE_NAME}" ]; then
        SERVICE_MGR="openrc"
    else
        err "未检测到 ${SERVICE_NAME} 服务，请先使用 install_node.sh 安装。"
        exit 1
    fi
    info "服务管理器: ${SERVICE_MGR}"
}

check_installed() {
    if [ ! -f "$BIN_PATH" ]; then
        err "未检测到已安装的节点二进制 ($BIN_PATH)，请先安装。"
        exit 1
    fi
    info "当前版本: $(${BIN_PATH} -V 2>/dev/null || ${BIN_PATH} --version 2>/dev/null || echo '未知')"
}

get_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64) echo "amd64" ;;
        aarch64) echo "arm64" ;;
        armv7l) echo "armv7" ;;
        *) err "暂不支持该架构: $arch"; exit 1 ;;
    esac
}

# 服务控制封装
svc_stop() {
    if [ "$SERVICE_MGR" = "systemd" ]; then
        systemctl stop "$SERVICE_NAME" || true
    else
        rc-service "$SERVICE_NAME" stop || true
    fi
}
svc_start() {
    if [ "$SERVICE_MGR" = "systemd" ]; then
        systemctl start "$SERVICE_NAME"
    else
        rc-service "$SERVICE_NAME" start
    fi
}
svc_active() {
    if [ "$SERVICE_MGR" = "systemd" ]; then
        systemctl is-active --quiet "$SERVICE_NAME"
    else
        rc-service "$SERVICE_NAME" status >/dev/null 2>&1
    fi
}

# 下载新版 gost 官方二进制
download_binary() {
    local arch url
    arch=$(get_arch)
    url="${GH_PROXY}https://github.com/go-gost/gost/releases/download/v${GOST_VERSION}/gost_${GOST_VERSION}_linux_${arch}.tar.gz"

    info "下载 GOST v${GOST_VERSION} (${arch}): $url"
    TMP_DIR=$(mktemp -d)

    if command -v curl >/dev/null 2>&1; then
        curl -fL -o "${TMP_DIR}/gost.tar.gz" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -O "${TMP_DIR}/gost.tar.gz" "$url"
    else
        err "需要 wget 或 curl"
        exit 1
    fi

    if [ ! -s "${TMP_DIR}/gost.tar.gz" ]; then
        err "下载失败，请检查网络、版本号或设置 GH_PROXY 加速"
        exit 1
    fi

    tar -zxf "${TMP_DIR}/gost.tar.gz" -C "${TMP_DIR}"
    if [ ! -f "${TMP_DIR}/gost" ]; then
        err "压缩包内未找到 gost 二进制"
        exit 1
    fi
    chmod +x "${TMP_DIR}/gost"
    NEW_BIN="${TMP_DIR}/gost"
    ok "下载完成"
}

# 备份旧二进制与配置（保留账号等数据）
backup_current() {
    local ts
    ts=$(date +%Y%m%d_%H%M%S)

    BACKUP_BIN="${BIN_PATH}.bak.${ts}"
    cp -p "$BIN_PATH" "$BACKUP_BIN"
    info "已备份旧二进制: $BACKUP_BIN"

    if [ -f "$CONF_FILE" ]; then
        CONF_BACKUP="${CONF_FILE}.bak.${ts}"
        cp -p "$CONF_FILE" "$CONF_BACKUP"
        ok "已备份节点配置: $CONF_BACKUP（账号配置完整保留）"
    else
        warn "未找到配置文件 ($CONF_FILE)"
    fi
}

# 解析 API 端口用于健康检查
get_api_port() {
    local port=""
    if [ -f "$CONF_FILE" ]; then
        # 形如  addr: ":39000"
        port=$(grep -E '^\s*addr:' "$CONF_FILE" | head -n1 | sed -E 's/.*addr:\s*"?:?([0-9]+)"?.*/\1/')
    fi
    echo "$port"
}

do_upgrade() {
    info "停止服务 ${SERVICE_NAME} ..."
    svc_stop

    info "替换二进制 ..."
    install -m 0755 "$NEW_BIN" "$BIN_PATH"

    info "启动服务 ${SERVICE_NAME} ..."
    svc_start
}

health_check() {
    local port elapsed=0
    port=$(get_api_port)
    info "健康检查（最长 ${HEALTH_TIMEOUT}s）..."

    while [ "$elapsed" -lt "$HEALTH_TIMEOUT" ]; do
        if svc_active; then
            # 服务存活；若能解析到 API 端口，进一步确认端口已监听
            if [ -n "$port" ]; then
                if command -v ss >/dev/null 2>&1; then
                    if ss -tln 2>/dev/null | grep -q ":${port} "; then
                        ok "健康检查通过（服务运行，API 端口 ${port} 已监听）"
                        return 0
                    fi
                elif command -v netstat >/dev/null 2>&1; then
                    if netstat -tln 2>/dev/null | grep -q ":${port} "; then
                        ok "健康检查通过（服务运行，API 端口 ${port} 已监听）"
                        return 0
                    fi
                else
                    ok "健康检查通过（服务处于运行状态）"
                    return 0
                fi
            else
                ok "健康检查通过（服务处于运行状态）"
                return 0
            fi
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    return 1
}

rollback() {
    err "升级失败，正在回滚..."
    svc_stop
    if [ -n "$BACKUP_BIN" ] && [ -f "$BACKUP_BIN" ]; then
        install -m 0755 "$BACKUP_BIN" "$BIN_PATH"
        info "已还原旧二进制"
    fi
    svc_start || true
    err "已回滚到升级前版本。"
    if [ "$SERVICE_MGR" = "systemd" ]; then
        err "查看日志: journalctl -u ${SERVICE_NAME} -n 80"
    fi
    [ -n "$CONF_BACKUP" ] && info "配置备份位于: $CONF_BACKUP（未被修改）"
    exit 1
}

main() {
    check_root
    detect_service_mgr
    check_installed
    download_binary
    backup_current
    do_upgrade

    if health_check; then
        echo ""
        ok "升级完成！"
        info "新版本: $(${BIN_PATH} -V 2>/dev/null || ${BIN_PATH} --version 2>/dev/null || echo '未知')"
        info "旧二进制备份: ${BACKUP_BIN}"
        [ -n "$CONF_BACKUP" ] && info "配置备份: ${CONF_BACKUP}"
        echo ""
        info "如确认无误，可手动删除上述 .bak 备份文件。"
    else
        rollback
    fi
}

main "$@"
