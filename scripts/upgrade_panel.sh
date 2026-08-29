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

# ---------------------------------------------------------------------------
# 老版本 -> 新版本的部署布局迁移
#
# 早期版本把二进制装在 /usr/local/bin 并以 root 运行，systemd 单元没有任何
# 沙箱约束。新版本需要两项变化：
#   1. 二进制移到服务账号可写的 /opt/gost-panel —— 这是面板内在线更新的前提
#      （更新用 rename 替换二进制，rename 作用于目录项，进程须能写该目录）
#   2. 以专用非 root 账号运行，并施加 systemd 沙箱约束
#
# 迁移全程幂等，且原单元会被备份；升级后的健康检查失败会一并回滚。
# ---------------------------------------------------------------------------

UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
UNIT_BACKUP=""

# 从现有 systemd 单元中读取一个字段的值（取最后一次出现）
unit_value() {
    local key="$1"
    [ -f "$UNIT_FILE" ] || return 0
    grep -E "^${key}=" "$UNIT_FILE" | tail -n1 | cut -d= -f2- || true
}

# 迁移二进制位置
migrate_binary_location() {
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
    ok "已迁移到 ${BIN_FILE}"
}

# 确保存在专用服务账号
ensure_service_user() {
    if id -u "${SERVICE_USER}" >/dev/null 2>&1; then
        return 0
    fi

    info "创建服务账号 ${SERVICE_USER}..."
    useradd --system --no-create-home --shell /usr/sbin/nologin "${SERVICE_USER}" 2>/dev/null \
        || adduser --system --no-create-home --shell /sbin/nologin "${SERVICE_USER}" 2>/dev/null \
        || adduser -S -H -s /sbin/nologin "${SERVICE_USER}" 2>/dev/null \
        || true

    if id -u "${SERVICE_USER}" >/dev/null 2>&1; then
        ok "服务账号已创建"
    else
        warn "未能创建 ${SERVICE_USER} 账号，将继续以现有账号运行"
    fi
}

# 校正各目录属主与权限
fix_ownership() {
    id -u "${SERVICE_USER}" >/dev/null 2>&1 || return 0

    mkdir -p "$INSTALL_PATH" "$DATA_PATH" "$LOG_PATH"
    chown -R "${SERVICE_USER}:${SERVICE_USER}" "$INSTALL_PATH" "$DATA_PATH" "$LOG_PATH" 2>/dev/null || true
    chmod 750 "$INSTALL_PATH" 2>/dev/null || true
    chmod 700 "$DATA_PATH" 2>/dev/null || true
    chmod 750 "$LOG_PATH" 2>/dev/null || true

    # 配置含 JWT 密钥与初始口令，保持 root 所有、服务账号只读
    if [ -f "$CONFIG_FILE" ]; then
        chgrp "${SERVICE_USER}" "$CONFIG_FILE" 2>/dev/null || true
        chmod 640 "$CONFIG_FILE" 2>/dev/null || true
    fi

    # 备份目录由面板运行时创建在工作目录下，可能还不存在
    if [ -d "${DATA_PATH}/backups" ]; then
        chown -R "${SERVICE_USER}:${SERVICE_USER}" "${DATA_PATH}/backups" 2>/dev/null || true
        chmod 700 "${DATA_PATH}/backups" 2>/dev/null || true
    fi
}

# 重写 systemd 单元为新版模板（含沙箱约束）
rewrite_unit() {
    local run_user="root"
    id -u "${SERVICE_USER}" >/dev/null 2>&1 && run_user="${SERVICE_USER}"

    # 幂等：单元已经是本函数会写出的样子就直接返回。
    #
    # 判断条件要对着实际会写入的 run_user 比，而不是硬编码 SERVICE_USER：
    # 服务账号创建失败时会回退到 root，若按 SERVICE_USER 判断则永远不相等，
    # 每次升级都会重写单元并留下一个新备份。
    if [ -f "$UNIT_FILE" ] \
        && [ "$(unit_value User)" = "$run_user" ] \
        && [ "$(unit_value ExecStart)" = "${BIN_FILE} -c ${CONFIG_FILE}" ] \
        && grep -q '^ProtectSystem=strict' "$UNIT_FILE" \
        && grep -q "^ReadWritePaths=.*${INSTALL_PATH}" "$UNIT_FILE"; then
        return 0
    fi

    info "更新 systemd 单元（运行账号: ${run_user}，附加沙箱约束）..."

    if [ -f "$UNIT_FILE" ]; then
        UNIT_BACKUP="${UNIT_FILE}.bak.$(date +%s)"
        cp -p "$UNIT_FILE" "$UNIT_BACKUP"
        info "原单元已备份: $UNIT_BACKUP"
    fi

    cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Gost Panel Service
Documentation=https://github.com/${REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${run_user}
Group=${run_user}
WorkingDirectory=${DATA_PATH}
ExecStart=${BIN_FILE} -c ${CONFIG_FILE}
Restart=always
RestartSec=5
LimitNOFILE=65536

# ---- 安全加固 ----
NoNewPrivileges=true
ProtectSystem=strict
# INSTALL_PATH 必须可写：面板内在线更新通过 rename 替换二进制
ReadWritePaths=${DATA_PATH} ${LOG_PATH} ${INSTALL_PATH}
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    ok "systemd 单元已更新"
}

# 迁移历史遗留的数据库位置。
# 早期配置里的 database.path 可能是相对路径（./gost-panel.db），
# 相对于 WorkingDirectory 解析后就落在 DATA_PATH 下，通常无需搬动；
# 这里只处理 root 运行遗留下来的属主问题。
migrate_database() {
    id -u "${SERVICE_USER}" >/dev/null 2>&1 || return 0
    local db
    db=$(grep -E '^\s*path:' "$CONFIG_FILE" 2>/dev/null | head -n1 | sed -E 's/.*path:\s*"?([^"]*)"?\s*$/\1/') || true
    [ -n "$db" ] || return 0

    # 相对路径按 WorkingDirectory 解析
    case "$db" in
        /*) ;;
        *) db="${DATA_PATH}/$(basename "$db")" ;;
    esac

    for f in "$db" "${db}-wal" "${db}-shm"; do
        [ -f "$f" ] && chown "${SERVICE_USER}:${SERVICE_USER}" "$f" 2>/dev/null || true
    done
}

migrate_layout() {
    migrate_binary_location
    ensure_service_user
    fix_ownership
    migrate_database
    rewrite_unit
}

# 前置检查：确认已安装
check_installed() {
    # 旧版本装在 /usr/local/bin，这里两个位置都认；
    # 实际的布局迁移放在 do_upgrade 中执行，以便被健康检查与回滚覆盖。
    if [ ! -f "$BIN_FILE" ] && [ -f "$LEGACY_BIN" ]; then
        info "检测到旧版安装布局（${LEGACY_BIN}），升级过程中会迁移到 ${INSTALL_PATH}"
        BIN_FILE="$LEGACY_BIN"
    fi

    if [ ! -f "$BIN_FILE" ]; then
        err "未检测到已安装的 Gost Panel（${INSTALL_PATH}/gost-panel 或 ${LEGACY_BIN}），请先使用 install_panel.sh 安装。"
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

    # 兼容两种包内布局：GoReleaser 打包的叫 gost-panel，
    # 而本功能上线前手工打包的版本叫 gost-panel-linux-<arch>。
    tar -zxf "${TMP_DIR}/${asset}" -C "${TMP_DIR}"
    NEW_BIN=""
    for candidate in "${TMP_DIR}/gost-panel" "${TMP_DIR}/gost-panel-linux-${arch}"; do
        if [ -f "$candidate" ]; then
            NEW_BIN="$candidate"
            break
        fi
    done
    if [ -z "$NEW_BIN" ]; then
        err "压缩包内未找到 gost-panel 二进制"
        exit 1
    fi
    chmod +x "$NEW_BIN"
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

    # 布局迁移放在服务停止后、启动前执行，
    # 这样它与二进制替换一起被随后的健康检查覆盖：任一环节出问题都会整体回滚。
    migrate_layout

    # migrate_layout 可能把 BIN_FILE 从旧位置切换到了新位置
    BIN_FILE="${INSTALL_PATH}/gost-panel"

    info "替换二进制 ..."
    install -m 0755 "$NEW_BIN" "$BIN_FILE"
    if id -u "${SERVICE_USER}" >/dev/null 2>&1; then
        chown "${SERVICE_USER}:${SERVICE_USER}" "$BIN_FILE" 2>/dev/null || true
    fi

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
    # 布局迁移会重写 systemd 单元；若新单元本身就是失败原因
    # （例如沙箱约束与本机路径冲突），只还原二进制是不够的。
    if [ -n "$UNIT_BACKUP" ] && [ -f "$UNIT_BACKUP" ]; then
        cp -p "$UNIT_BACKUP" "$UNIT_FILE"
        systemctl daemon-reload
        info "已还原原 systemd 单元"
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

    # 放在 if 条件中调用：这样 set -e 在函数体内被挂起，
    # 迁移或替换过程中的任何失败都会走到回滚，而不是直接退出脚本
    # 留下一个半迁移的状态。
    if ! do_upgrade; then
        rollback
    fi

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
