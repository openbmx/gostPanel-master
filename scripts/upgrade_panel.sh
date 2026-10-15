#!/bin/bash
# ========================================================
#  Gost Panel 服务端（主控）升级脚本
#  - 仅替换二进制，完整保留配置文件与数据库
#  - 升级前自动备份旧二进制与数据库
#  - 升级后健康检查并核对实际运行的版本，失败自动回滚
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
# 升级后服务运行的二进制，始终是新布局下的位置，任何步骤都不得改写它。
# 升级前实际在跑的是哪一个，记录在 CURRENT_BIN 中。
BIN_FILE="${INSTALL_PATH}/gost-panel"
CONFIG_FILE="${CONFIG_PATH}/config.yaml"
HEALTH_TIMEOUT=30                          # 健康检查超时秒数

# GitHub API：用于确定目标版本、获取产物的 SHA256，也可作为下载来源。
# 部分网络（如国内云服务器）下 github.com 时通时断，api.github.com 却稳定可用。
API_BASE="https://api.github.com/repos/${REPO}"

# 网络超时。GitHub 在部分网络下会建立连接后不再返回任何数据，
# 不设超时下载会无限挂起，看上去就像升级卡死了。
# 连接 15 秒超时；传输中连续 60 秒低于 1 KB/s 视为卡死，转而尝试下一个来源。
CURL_NET_OPTS=(--connect-timeout 15 --speed-limit 1024 --speed-time 60)
WGET_NET_OPTS=(--dns-timeout=15 --connect-timeout=15 --read-timeout=60 --tries=2)

info() { echo -e "${BLUE}[信息]${PLAIN} $1"; }
ok()   { echo -e "${GREEN}[成功]${PLAIN} $1"; }
warn() { echo -e "${YELLOW}[警告]${PLAIN} $1"; }
err()  { echo -e "${RED}[错误]${PLAIN} $1"; }

echo -e "${GREEN}========================================${PLAIN}"
echo -e "${GREEN}  Gost Panel 服务端升级${PLAIN}"
echo -e "${GREEN}========================================${PLAIN}\n"

# 运行期状态（回滚用）
CURRENT_BIN=""      # 升级前服务实际运行的二进制，由 check_installed 确定
BACKUP_BIN=""
DB_BACKUP=""
TMP_DIR=""
NEW_BIN=""
RELEASE_JSON=""     # GitHub API 返回的 release 元数据文件，获取失败时为空
RUNNING_VERSION=""  # 健康检查时服务自报的版本

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

# systemd 单元 ExecStart 中的程序路径（去掉参数与 systemd 的前缀修饰符）
unit_exec_bin() {
    unit_value ExecStart | awk '{print $1}' | sed -E 's/^[-@:+!]+//'
}

# 迁移二进制位置
#
# 新位置已经有文件时不动旧二进制：这是早期升级脚本留下的错位状态，
# 旧二进制可能正是服务在跑的那个，要等新版本通过健康检查后再清理（cleanup_legacy_bin）。
migrate_binary_location() {
    if [ -f "$BIN_FILE" ]; then
        return 0
    fi
    if [ ! -f "$LEGACY_BIN" ]; then
        return 0
    fi

    info "检测到旧版安装布局，正在迁移二进制到 ${INSTALL_PATH}..."
    mkdir -p "$INSTALL_PATH" || return 1
    mv "$LEGACY_BIN" "$BIN_FILE" || return 1
    chmod +x "$BIN_FILE" || return 1
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
        cp -p "$UNIT_FILE" "$UNIT_BACKUP" || return 1
        info "原单元已备份: $UNIT_BACKUP"
    fi

    cat > "$UNIT_FILE" <<EOF || return 1
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

    systemctl daemon-reload || return 1
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

# 注意：本函数经由 do_upgrade 在 if 条件中执行，set -e 在其中整体失效，
# 关键步骤必须显式返回失败，否则失败会被静默跳过、无法触发回滚。
migrate_layout() {
    migrate_binary_location || return 1
    ensure_service_user
    fix_ownership
    migrate_database
    rewrite_unit || return 1
}

# 前置检查：确认已安装，并确定升级前服务实际运行的二进制（CURRENT_BIN）。
#
# 以 systemd 单元的 ExecStart 为准，而不是看哪个位置有文件：本脚本的早期版本
# 在这里把 BIN_FILE 改写成了旧路径，导致随后的布局迁移误以为二进制已在新位置、
# 并把旧路径写回了单元 —— 新二进制装进了 /opt/gost-panel，服务却继续跑
# /usr/local/bin 下的旧版本。这类机器两处都有文件，只按文件是否存在判断，
# 就会备份错对象、显示错版本、回滚到错位置。
check_installed() {
    if ! command -v systemctl >/dev/null 2>&1 || [ ! -f "$UNIT_FILE" ]; then
        err "未检测到 systemd 服务 ${SERVICE_NAME}，本升级脚本仅支持 systemd 部署。"
        exit 1
    fi

    local candidate
    for candidate in "$(unit_exec_bin)" "$BIN_FILE" "$LEGACY_BIN"; do
        if [ -n "$candidate" ] && [ -f "$candidate" ]; then
            CURRENT_BIN="$candidate"
            break
        fi
    done

    if [ -z "$CURRENT_BIN" ]; then
        err "未检测到已安装的 Gost Panel（${BIN_FILE} 或 ${LEGACY_BIN}），请先使用 install_panel.sh 安装。"
        exit 1
    fi
    if [ "$CURRENT_BIN" != "$BIN_FILE" ]; then
        info "检测到旧版安装布局（服务当前运行 ${CURRENT_BIN}），升级过程中会迁移到 ${BIN_FILE}"
    fi
    info "当前版本: $("$CURRENT_BIN" --version 2>/dev/null || echo '未知')"
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

# ---------------------------------------------------------------------------
# 下载与完整性校验
#
# 安全：校验基准（SHA256）只从 GitHub 直连获取，从不经过 GH_PROXY。
# 若二进制与其校验和都走同一个加速镜像，镜像方可以同时替换两者，
# 校验就完全失去意义 —— 这正是很多"支持加速"的升级脚本的实际漏洞。
#
# 直连 GitHub 的校验基准有两个来源，任一可用即可：
#   1. api.github.com 返回的产物 digest 字段（GitHub 在产物上传时计算）
#   2. github.com 上随 release 一起发布的 checksums.txt
# 优先用 1：部分网络下 github.com 基本不可达而 api.github.com 正常，
# 只认 2 的话，升级在这类网络下必然失败，设置 GH_PROXY 也无济于事。
# ---------------------------------------------------------------------------

# 获取小文件（API 响应、校验和文件）：fetch_small <url> <输出文件> [Accept]
fetch_small() {
    local url="$1" out="$2" accept="${3:-*/*}"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "${CURL_NET_OPTS[@]}" --max-time 60 -H "Accept: ${accept}" -o "$out" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "${WGET_NET_OPTS[@]}" --header="Accept: ${accept}" -O "$out" "$url"
    else
        return 1
    fi
}

# 下载发布产物（显示进度）：fetch_file <url> <输出文件> [Accept]
fetch_file() {
    local url="$1" out="$2" accept="${3:-*/*}"
    if command -v curl >/dev/null 2>&1; then
        curl -fL "${CURL_NET_OPTS[@]}" -H "Accept: ${accept}" -o "$out" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget "${WGET_NET_OPTS[@]}" --header="Accept: ${accept}" -O "$out" "$url"
    else
        err "需要 wget 或 curl"
        exit 1
    fi
}

# 从 GitHub API 获取目标 release 的元数据；失败时 RELEASE_JSON 保持为空
fetch_release_meta() {
    local url out="${TMP_DIR}/release.json"
    if [ "$VERSION" = "latest" ]; then
        url="${API_BASE}/releases/latest"
    else
        url="${API_BASE}/releases/tags/${VERSION}"
    fi
    if fetch_small "$url" "$out" "application/vnd.github+json" 2>/dev/null && [ -s "$out" ]; then
        RELEASE_JSON="$out"
    fi
}

# release 元数据中的 tag_name
release_tag() {
    grep -o '"tag_name": *"[^"]*"' "$RELEASE_JSON" | head -n1 | sed -E 's/.*"([^"]*)"$/\1/' || true
}

# 从 release 元数据中取出某个产物的 ID 与 SHA256：asset_meta <产物名>
# 输出 "<id> <sha256>"；该产物没有 digest 时只有 id，找不到该产物时无输出。
#
# 环境中未必有 jq，这里按 GitHub 返回的字段顺序做最小解析：每个产物对象依次为
# url、id、node_id、name、label、uploader{...}、...、digest、...
# 解析即便出错，也只会导致下载失败或校验不通过而中止升级，
# 不会让错误的文件通过校验 —— 安装的产物必须与这里取到的 SHA256 一致。
asset_meta() {
    tr ',{}' '\n\n\n' < "$RELEASE_JSON" | awk -v want="$1" '
        function val(s) {
            sub(/^"[^"]*": */, "", s)
            gsub(/"/, "", s)
            return s
        }
        { gsub(/^[ \t]+/, ""); gsub(/[ \t\r]+$/, "") }
        /^"id": *[0-9]+$/ { last_id = val($0); next }
        /^"name": *"/ {
            # 进入了下一个产物仍未见到 digest：该产物没有 digest
            if (found) exit
            if (val($0) == want) { found = 1; id = last_id }
            next
        }
        found && /^"digest": *"sha256:[0-9a-f]+"$/ {
            d = val($0)
            sub(/^sha256:/, "", d)
            if (length(d) == 64) digest = d
            exit
        }
        END { if (found) print id, digest }
    ' || true
}

# 校验下载产物的 SHA256：verify_checksum <文件> <产物名> <tag> <API 给出的 SHA256（可为空）>
verify_checksum() {
    local file="$1" name="$2" tag="$3" expected="$4" basis url actual

    if [ "${SKIP_CHECKSUM:-0}" = "1" ]; then
        warn "SKIP_CHECKSUM=1，已跳过完整性校验（不推荐）"
        return 0
    fi

    info "校验文件完整性..."
    if [ -n "$expected" ]; then
        basis="GitHub API"
    else
        # API 不可用或该产物没有 digest：退回 checksums.txt（同样直连 GitHub）
        if [ "$tag" = "latest" ]; then
            url="https://github.com/${REPO}/releases/latest/download/checksums.txt"
        else
            url="https://github.com/${REPO}/releases/download/${tag}/checksums.txt"
        fi
        if ! fetch_small "$url" "${TMP_DIR}/checksums.txt" 2>/dev/null || [ ! -s "${TMP_DIR}/checksums.txt" ]; then
            err "无法从 GitHub 获取校验基准（api.github.com 与 checksums.txt 均不可用），已中止升级。"
            err "确需跳过校验时可设置 SKIP_CHECKSUM=1（不推荐，等同于放弃完整性保证）。"
            exit 1
        fi
        expected=$(grep -E "[ *]${name}\$" "${TMP_DIR}/checksums.txt" | awk '{print $1}' | head -n1) || true
        if [ -z "$expected" ]; then
            err "checksums.txt 中没有 ${name} 的条目，已中止升级"
            exit 1
        fi
        basis="checksums.txt"
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
        err "  期望: $expected（来自 ${basis}）"
        err "  实际: $actual"
        exit 1
    fi
    ok "校验通过（基准来自 ${basis}）"
}

# 下载新版二进制到临时目录
download_binary() {
    local arch asset out tag resolved gh_url api_url="" asset_id="" api_digest="" src downloaded=0
    local -a sources=()
    arch=$(get_arch)
    asset="gost-panel-linux-${arch}.tar.gz"
    TMP_DIR=$(mktemp -d)
    out="${TMP_DIR}/${asset}"

    # 先向 GitHub API 确认目标版本，拿到确切的 tag、产物 ID 与 SHA256。
    # VERSION=latest 时据此锁定具体 tag，保证下载与校验针对同一个版本 ——
    # 否则恰逢新版本发布，两次请求的 latest 可能指向不同的版本。
    tag="$VERSION"
    fetch_release_meta
    if [ -n "$RELEASE_JSON" ]; then
        resolved=$(release_tag)
        if [ -n "$resolved" ]; then
            tag="$resolved"
        fi
        read -r asset_id api_digest <<< "$(asset_meta "$asset")" || true
        info "目标版本: ${tag}"
    else
        warn "无法访问 GitHub API（api.github.com），将直接从 github.com 下载"
    fi

    if [ "$tag" = "latest" ]; then
        gh_url="https://github.com/${REPO}/releases/latest/download/${asset}"
    else
        gh_url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
    fi
    if [ -n "$asset_id" ]; then
        api_url="${API_BASE}/releases/assets/${asset_id}"
    fi

    # 下载来源依次尝试，任一成功即可（无论来自哪里，都必须通过随后的 SHA256 校验）：
    #   设置了 GH_PROXY：加速镜像 → GitHub API → github.com
    #   未设置：        GitHub API → github.com
    # API 排在 github.com 之前：两者最终都重定向到同一个产物 CDN，
    # 但 github.com 这一跳在部分网络下基本不可达。
    if [ -n "$GH_PROXY" ]; then
        sources+=("${GH_PROXY}${gh_url}")
    fi
    if [ -n "$api_url" ]; then
        sources+=("$api_url")
    fi
    sources+=("$gh_url")

    for src in "${sources[@]}"; do
        info "下载新版本 (${arch}): ${src}"
        if [ "$src" = "$api_url" ]; then
            fetch_file "$src" "$out" "application/octet-stream" && downloaded=1
        else
            fetch_file "$src" "$out" && downloaded=1
        fi
        if [ "$downloaded" -eq 1 ] && [ -s "$out" ]; then
            break
        fi
        downloaded=0
        warn "该来源下载失败"
    done

    if [ "$downloaded" -ne 1 ]; then
        err "下载失败：所有来源均不可用。可设置 GH_PROXY 使用 GitHub 加速镜像后重试，例如："
        err "  GH_PROXY=https://ghfast.top/ bash <(curl -sSL https://raw.githubusercontent.com/${REPO}/main/scripts/upgrade_panel.sh)"
        exit 1
    fi

    verify_checksum "$out" "$asset" "$tag" "$api_digest"

    # 兼容两种包内布局：GoReleaser 打包的叫 gost-panel，
    # 而本功能上线前手工打包的版本叫 gost-panel-linux-<arch>。
    tar -zxf "$out" -C "${TMP_DIR}"
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

    # 备份升级前实际运行的二进制，就放在它原来的目录里，回滚时原样还原
    BACKUP_BIN="${CURRENT_BIN}.bak.${ts}"
    cp -p "$CURRENT_BIN" "$BACKUP_BIN"
    info "已备份旧二进制: $BACKUP_BIN"

    # 解析数据库路径（优先读配置，回退默认）
    local db_path=""
    if [ -f "$CONFIG_FILE" ]; then
        db_path=$(grep -E '^\s*path:' "$CONFIG_FILE" | head -n1 | sed -E 's/.*path:\s*"?([^"]*)"?\s*$/\1/') || true
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
        port=$(grep -E '^\s*port:' "$CONFIG_FILE" | head -n1 | sed -E 's/.*port:\s*"?:?([0-9]+)"?.*/\1/') || true
    fi
    [ -z "$port" ] && port="39100"
    echo "$port"
}

# 替换二进制并重启服务
#
# 注意：本函数在 main 的 if 条件中调用，set -e 在函数体内（包括它调用的函数）
# 整体失效，失败不会自动中断。关键步骤必须显式 return 1 才能走到回滚。
do_upgrade() {
    info "停止服务 ${SERVICE_NAME} ..."
    systemctl stop "$SERVICE_NAME" || true

    # 布局迁移放在服务停止后、启动前执行，
    # 这样它与二进制替换一起被随后的健康检查覆盖：任一环节出问题都会整体回滚。
    migrate_layout || return 1

    info "替换二进制 ..."
    install -m 0755 "$NEW_BIN" "$BIN_FILE" || return 1
    if id -u "${SERVICE_USER}" >/dev/null 2>&1; then
        chown "${SERVICE_USER}:${SERVICE_USER}" "$BIN_FILE" 2>/dev/null || true
    fi

    info "启动服务 ${SERVICE_NAME} ..."
    systemctl start "$SERVICE_NAME" || return 1
}

# 去掉版本号的 v 前缀以便比较（早期构建自报 v1.0.4，GoReleaser 构建自报 1.1.1）
normalize_version() {
    local v="${1:-}"
    echo "${v#v}"
}

# 健康检查：服务可访问，且实际运行的正是新安装的版本。
#
# 只看能否访问是不够的：本脚本的早期版本在迁移旧布局时把旧二进制的路径写回了
# systemd 单元，服务照常启动、健康检查照常通过，跑的却仍是旧版本 ——
# 升级"成功"了，却什么都没变。/api/v1/health 返回 version 正是为了这里的核对。
health_check() {
    local port expected body scheme elapsed=0
    port=$(get_port)
    expected=$(normalize_version "$("$BIN_FILE" --version 2>/dev/null | awk '{print $NF}')")
    info "健康检查 127.0.0.1:${port}/api/v1/health，期望版本 ${expected:-未知}（最长 ${HEALTH_TIMEOUT}s）..."

    while [ "$elapsed" -lt "$HEALTH_TIMEOUT" ]; do
        if ! systemctl is-active --quiet "$SERVICE_NAME"; then
            warn "服务未处于 active 状态，继续等待..."
        else
            # 开启 TLS 的面板在同一端口上只接受 HTTPS；本机自检不校验证书
            for scheme in http https; do
                body=$(curl -fsSk --max-time 5 "${scheme}://127.0.0.1:${port}/api/v1/health" 2>/dev/null) && break
            done
            if [ -n "$body" ]; then
                RUNNING_VERSION=$(normalize_version "$(echo "$body" | grep -o '"version": *"[^"]*"' | sed -E 's/.*"([^"]*)"$/\1/')")
                if [ -z "$expected" ] || [ -z "$RUNNING_VERSION" ] || [ "$RUNNING_VERSION" = "$expected" ]; then
                    ok "健康检查通过"
                    return 0
                fi
                err "服务实际运行的版本是 ${RUNNING_VERSION}，而不是新安装的 ${expected}"
                return 1
            fi
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
    # 旧二进制还原到它原来的位置 —— 还原后的 systemd 单元指向的正是那里
    if [ -n "$BACKUP_BIN" ] && [ -f "$BACKUP_BIN" ]; then
        install -m 0755 "$BACKUP_BIN" "$CURRENT_BIN"
        info "已还原旧二进制: ${CURRENT_BIN}"
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

# 升级成功后清理旧位置的二进制。
#
# 正常迁移时它已被移走，只有早期升级脚本留下的错位状态里才会残留。
# 留着它会让人以为面板还是旧版本（早期的 gost.sh 就从这里读版本号），
# 也说不清服务到底在跑哪一个。若它正是升级前在跑的那个，已备份为 BACKUP_BIN。
cleanup_legacy_bin() {
    if [ "$LEGACY_BIN" = "$BIN_FILE" ] || [ ! -f "$LEGACY_BIN" ]; then
        return 0
    fi
    if rm -f "$LEGACY_BIN"; then
        info "已移除旧位置的二进制 ${LEGACY_BIN}"
    else
        warn "未能移除旧位置的二进制 ${LEGACY_BIN}，可手动删除"
    fi
}

main() {
    check_root
    check_installed
    download_binary
    backup_current

    # 放在 if 条件中调用：失败时走回滚，而不是直接退出脚本留下一个半迁移的状态。
    # set -e 在 do_upgrade 内部整体失效，其中的关键步骤都显式检查了返回值。
    if ! do_upgrade; then
        rollback
    fi

    if health_check; then
        cleanup_legacy_bin
        echo ""
        ok "升级完成！"
        info "运行中的版本: ${RUNNING_VERSION:-未知}"
        info "旧二进制备份: ${BACKUP_BIN}"
        [ -n "$DB_BACKUP" ] && info "数据库备份: ${DB_BACKUP}"
        echo ""
        info "如确认无误，可手动删除上述 .bak 备份文件。"
    else
        rollback
    fi
}

main "$@"
