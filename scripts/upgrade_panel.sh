#!/bin/bash
# ========================================================
#  Gost Panel 服务端（主控）升级脚本
#  - 仅替换二进制，完整保留配置文件与数据库
#  - 升级前自动备份旧二进制与数据库
#  - 升级后健康检查，失败自动回滚
#  仓库: https://github.com/openbmx/gostPanel-master
# ========================================================

set -euo pipefail

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
PLAIN='\033[0m'

# 配置（均可通过环境变量覆盖，须与安装脚本保持一致）
REPO="${REPO:-openbmx/gostPanel-master}"   # GitHub 仓库 owner/name
VERSION="${VERSION:-latest}"               # latest 或具体 tag，如 v1.0.0
GH_PROXY="${GH_PROXY:-}"                    # 可选 GitHub 加速前缀，如 https://ghfast.top/
# 二进制现在装在服务账号可写的独立目录，以支持面板内在线更新
INSTALL_PATH="/opt/gost-panel"
LEGACY_BIN="/usr/local/bin/gost-panel"     # 旧版本的安装位置
CONFIG_PATH="/etc/gost-panel"
DATA_PATH="/var/lib/gost-panel"
LOG_PATH="/var/log/gost-panel"
SERVICE_NAME="gost-panel"
SERVICE_USER="gost-panel"
BIN_FILE="${INSTALL_PATH}/gost-panel"
CONFIG_FILE="${CONFIG_PATH}/config.yaml"
HEALTH_TIMEOUT=30                          # 健康检查超时秒数

info() { echo -e "${BLUE}[信息]${PLAIN} $1"; }
ok()   { echo -e "${GREEN}[成功]${PLAIN} $1"; }
warn() { echo -e "${YELLOW}[警告]${PLAIN} $1"; }
err()  { echo -e "${RED}[错误]${PLAIN} $1"; }

echo -e "${GREEN}========================================${PLAIN}"
echo -e "${GREEN}  Gost Panel 服务端升级${PLAIN}"
echo -e "${GREEN}========================================${PLAIN}\n"

# 临时变量（回滚用）
BACKUP_BIN=""
DB_BACKUP=""
TMP_DIR=""

cleanup() {
    [ -n "$TMP_DIR" ] && rm -rf "$TMP_DIR" 2>/dev/null || true
}
trap cleanup EXIT

# 检查 Root 权限
check_root() {
    if [[ $EUID -ne 0 ]]; then
        err "请使用 root 用户运行此脚本"
        exit 1
    fi
}

# 把旧版本安装（/usr/local/bin）迁移到新布局（/opt/gost-panel）。
# 新布局是面板内在线更新的前提：二进制必须位于服务账号可写的目录。
migrate_layout() {
    if [ -f "$BIN_FILE" ]; then
        return 0
    fi
    if [ ! -f "$LEGACY_BIN" ]; then
        return 0
    fi

    info "检测到旧版安装布局，正在迁移二进制到 ${INSTALL_PATH}..."
    mkdir -p "$INSTALL_PATH"
    mv "$LEGACY_BIN" "$BIN_FILE"
    chmod +x "$BIN_FILE"

    # systemd 单元里的 ExecStart 与 ReadWritePaths 都要跟着改
    local unit="/etc/systemd/system/${SERVICE_NAME}.service"
    if [ -f "$unit" ]; then
        sed -i "s#^ExecStart=.*/gost-panel#ExecStart=${BIN_FILE}#" "$unit"
        if grep -q '^ReadWritePaths=' "$unit"; then
            # 已有该行则追加程序目录（幂等：已包含就不重复加）
            grep -q "ReadWritePaths=.*${INSTALL_PATH}" "$unit" \
                || sed -i "s#^ReadWritePaths=.*#& ${INSTALL_PATH}#" "$unit"
        fi
        systemctl daemon-reload
    fi

    # 目录归服务账号，否则面板内更新仍然无法替换二进制
    if id -u "${SERVICE_USER}" >/dev/null 2>&1; then
        chown -R "${SERVICE_USER}:${SERVICE_USER}" "$INSTALL_PATH" 2>/dev/null || true
        chmod 750 "$INSTALL_PATH" 2>/dev/null || true
    fi

    ok "已迁移到 ${BIN_FILE}"
}

# 前置检查：确认已安装
check_installed() {
    migrate_layout

    if [ ! -f "$BIN_FILE" ]; then
        err "未检测到已安装的 Gost Panel ($BIN_FILE)，请先使用 install_panel.sh 安装。"
        exit 1
    fi
    if ! command -v systemctl >/dev/null 2>&1 || [ ! -f "/etc/systemd/system/${SERVICE_NAME}.service" ]; then
        err "未检测到 systemd 服务 ${SERVICE_NAME}，本升级脚本仅支持 systemd 部署。"
        exit 1
    fi
    info "当前版本: $(${BIN_FILE} --version 2>/dev/null || echo '未知')"
}

# 检测系统架构
get_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) err "不支持的架构: $arch"; exit 1 ;;
    esac
}

# 校验下载产物的 SHA256。
#
# 安全：校验和文件始终直连 GitHub 获取，不经过 GH_PROXY。
# 若两者都走同一个加速镜像，镜像方可以同时替换二进制与其校验和，
# 校验就完全失去意义 —— 这正是很多"支持加速"的升级脚本的实际漏洞。
verify_checksum() {
    local file="$1" name="$2" url expected actual

    if [ "$VERSION" = "latest" ]; then
        url="https://github.com/${REPO}/releases/latest/download/checksums.txt"
    else
        url="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
    fi

    if [ "${SKIP_CHECKSUM:-0}" = "1" ]; then
        warn "SKIP_CHECKSUM=1，已跳过完整性校验（不推荐）"
        return 0
    fi

    info "校验文件完整性..."
    local fetched=0
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "${TMP_DIR}/checksums.txt" "$url" 2>/dev/null && fetched=1
    fi
    if [ "$fetched" -eq 0 ] && command -v wget >/dev/null 2>&1; then
        wget -qO "${TMP_DIR}/checksums.txt" "$url" 2>/dev/null && fetched=1
    fi

    if [ "$fetched" -eq 0 ] || [ ! -s "${TMP_DIR}/checksums.txt" ]; then
        err "无法获取 checksums.txt，已中止升级。"
        err "若目标版本发布于该功能上线之前，请升级到更新的版本；"
        err "确需跳过校验时可设置 SKIP_CHECKSUM=1（不推荐，等同于放弃完整性保证）。"
        exit 1
    fi

    expected=$(grep -E "[ *]${name}\$" "${TMP_DIR}/checksums.txt" | awk '{print $1}' | head -n1)
    if [ -z "$expected" ]; then
        err "checksums.txt 中没有 ${name} 的条目，已中止升级"
        exit 1
    fi

    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$file" | awk '{print $1}')
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$file" | awk '{print $1}')
    else
        err "缺少 sha256sum/shasum，无法校验完整性，已中止升级"
        exit 1
    fi

    if [ "$expected" != "$actual" ]; then
        err "校验失败！文件可能已损坏或被篡改，已中止升级"
        err "  期望: $expected"
        err "  实际: $actual"
        exit 1
    fi
    ok "校验通过"
}

# 下载新版二进制到临时目录
download_binary() {
    local arch asset url
    arch=$(get_arch)
    asset="gost-panel-linux-${arch}.tar.gz"
    if [ "$VERSION" = "latest" ]; then
        url="${GH_PROXY}https://github.com/${REPO}/releases/latest/download/${asset}"
    else
        url="${GH_PROXY}https://github.com/${REPO}/releases/download/${VERSION}/${asset}"
    fi

    info "下载新版本 ($arch): $url"
    TMP_DIR=$(mktemp -d)

    if command -v curl >/dev/null 2>&1; then
        curl -fL -o "${TMP_DIR}/${asset}" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -O "${TMP_DIR}/${asset}" "$url"
    else
        err "需要 wget 或 curl"
        exit 1
    fi

    if [ ! -s "${TMP_DIR}/${asset}" ]; then
        err "下载失败，请检查网络或设置 GH_PROXY 加速"
        exit 1
    fi

    verify_checksum "${TMP_DIR}/${asset}" "$asset"

    tar -zxf "${TMP_DIR}/${asset}" -C "${TMP_DIR}"
    if [ ! -f "${TMP_DIR}/gost-panel-linux-${arch}" ]; then
        err "压缩包内未找到 gost-panel 二进制"
        exit 1
    fi
    chmod +x "${TMP_DIR}/gost-panel-linux-${arch}"
    NEW_BIN="${TMP_DIR}/gost-panel-linux-${arch}"
    ok "下载完成"
}

# 备份旧二进制与数据库（不影响数据）
backup_current() {
    local ts
    ts=$(date +%Y%m%d_%H%M%S)

    BACKUP_BIN="${BIN_FILE}.bak.${ts}"
    cp -p "$BIN_FILE" "$BACKUP_BIN"
    info "已备份旧二进制: $BACKUP_BIN"

    # 解析数据库路径（优先读配置，回退默认）
    local db_path=""
    if [ -f "$CONFIG_FILE" ]; then
        db_path=$(grep -E '^\s*path:' "$CONFIG_FILE" | head -n1 | sed -E 's/.*path:\s*"?([^"]*)"?\s*$/\1/')
    fi
    [ -z "$db_path" ] && db_path="${DATA_PATH}/gost-panel.db"

    if [ -f "$db_path" ]; then
        DB_BACKUP="${db_path}.bak.${ts}"
        cp -p "$db_path" "$DB_BACKUP"
        ok "已备份数据库: $DB_BACKUP（数据完整保留）"
    else
        warn "未找到数据库文件 ($db_path)，跳过数据库备份"
    fi
}

# 解析监听端口用于健康检查
get_port() {
    local port=""
    if [ -f "$CONFIG_FILE" ]; then
        # 形如  port: ":39100"
        port=$(grep -E '^\s*port:' "$CONFIG_FILE" | head -n1 | sed -E 's/.*port:\s*"?:?([0-9]+)"?.*/\1/')
    fi
    [ -z "$port" ] && port="39100"
    echo "$port"
}

# 替换二进制并重启服务
do_upgrade() {
    info "停止服务 ${SERVICE_NAME} ..."
    systemctl stop "$SERVICE_NAME" || true

    info "替换二进制 ..."
    install -m 0755 "$NEW_BIN" "$BIN_FILE"

    info "启动服务 ${SERVICE_NAME} ..."
    systemctl start "$SERVICE_NAME"
}

# 健康检查
health_check() {
    local port elapsed=0
    port=$(get_port)
    info "健康检查 http://127.0.0.1:${port}/api/v1/health （最长 ${HEALTH_TIMEOUT}s）..."

    while [ "$elapsed" -lt "$HEALTH_TIMEOUT" ]; do
        if ! systemctl is-active --quiet "$SERVICE_NAME"; then
            warn "服务未处于 active 状态，继续等待..."
        elif curl -fsS "http://127.0.0.1:${port}/api/v1/health" >/dev/null 2>&1; then
            ok "健康检查通过"
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    return 1
}

# 回滚
rollback() {
    err "升级失败，正在回滚..."
    systemctl stop "$SERVICE_NAME" || true
    if [ -n "$BACKUP_BIN" ] && [ -f "$BACKUP_BIN" ]; then
        install -m 0755 "$BACKUP_BIN" "$BIN_FILE"
        info "已还原旧二进制"
    fi
    systemctl start "$SERVICE_NAME" || true
    err "已回滚到升级前版本。请查看日志: journalctl -u ${SERVICE_NAME} -n 80"
    [ -n "$DB_BACKUP" ] && info "数据库备份位于: $DB_BACKUP（未被修改）"
    exit 1
}

main() {
    check_root
    check_installed
    download_binary
    backup_current
    do_upgrade

    if health_check; then
        echo ""
        ok "升级完成！"
        info "新版本: $(${BIN_FILE} --version 2>/dev/null || echo '未知')"
        info "旧二进制备份: ${BACKUP_BIN}"
        [ -n "$DB_BACKUP" ] && info "数据库备份: ${DB_BACKUP}"
        echo ""
        info "如确认无误，可手动删除上述 .bak 备份文件。"
    else
        rollback
    fi
}

main "$@"
