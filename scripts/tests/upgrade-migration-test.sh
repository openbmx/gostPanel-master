#!/usr/bin/env bash
# ============================================================================
# 升级脚本的布局迁移演练
#
# 在临时目录里搭出「旧版部署」的样子（二进制在 /usr/local/bin、systemd 单元
# 以 root 运行且无沙箱约束），跑一遍 upgrade_panel.sh 里的迁移函数，
# 断言迁移结果符合预期。
#
# 不依赖 root、systemd 或网络：脚本内把相关命令替换为桩，只验证文件布局与
# 单元内容的变换逻辑。CI 与本地都能跑。
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPGRADE_SH="${SCRIPT_DIR}/../upgrade_panel.sh"

if [ ! -f "$UPGRADE_SH" ]; then
    echo "找不到 upgrade_panel.sh" >&2
    exit 1
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail=0
check() {
    local desc="$1" cond="$2"
    if eval "$cond"; then
        echo "  ✅ $desc"
    else
        echo "  ❌ $desc"
        fail=1
    fi
}

# --- 搭建旧版部署 --------------------------------------------------------
INSTALL_PATH="${WORK}/opt/gost-panel"
LEGACY_BIN="${WORK}/usr/local/bin/gost-panel"
CONFIG_PATH="${WORK}/etc/gost-panel"
DATA_PATH="${WORK}/var/lib/gost-panel"
LOG_PATH="${WORK}/var/log/gost-panel"
CONFIG_FILE="${CONFIG_PATH}/config.yaml"
UNIT_FILE="${WORK}/etc/systemd/system/gost-panel.service"
BIN_FILE="${INSTALL_PATH}/gost-panel"
SERVICE_NAME="gost-panel"
SERVICE_USER="gost-panel"
REPO="openbmx/gostPanel-master"
UNIT_BACKUP=""

mkdir -p "$(dirname "$LEGACY_BIN")" "$CONFIG_PATH" "$DATA_PATH" "$LOG_PATH" "$(dirname "$UNIT_FILE")"
printf '#!/bin/sh\necho old\n' > "$LEGACY_BIN"
chmod +x "$LEGACY_BIN"
printf 'database:\n  path: "%s/gost-panel.db"\n' "$DATA_PATH" > "$CONFIG_FILE"
printf 'fake db' > "${DATA_PATH}/gost-panel.db"

cat > "$UNIT_FILE" <<EOF
[Unit]
Description=Gost Panel Service

[Service]
Type=simple
User=root
WorkingDirectory=${DATA_PATH}
ExecStart=${LEGACY_BIN} -c ${CONFIG_FILE}
Restart=always

[Install]
WantedBy=multi-user.target
EOF

# --- 桩：不实际调用系统命令 ----------------------------------------------
systemctl() { :; }
useradd()   { return 1; }   # 模拟无法创建用户，验证回退到当前账号的路径
adduser()   { return 1; }
id() { command id "$@"; }
chown() { :; }
chgrp() { :; }
info() { :; }
ok()   { :; }
warn() { :; }
err()  { :; }

# --- 载入被测函数 --------------------------------------------------------
# 迁移相关的函数在源文件中是连续的一段，从 unit_value() 开始、
# 到 migrate_layout() 的收尾大括号结束。整块提取比逐个匹配稳健：
# 函数体内含有 heredoc 与嵌套大括号，按函数名逐个 awk 会截断。
START=$(grep -n '^unit_value() {' "$UPGRADE_SH" | cut -d: -f1)
END=$(awk 'f && /^}/ {print NR; exit} /^migrate_layout\(\) \{/ {f=1}' "$UPGRADE_SH")

if [ -z "$START" ] || [ -z "$END" ]; then
    echo "无法在 upgrade_panel.sh 中定位迁移函数区间（脚本结构已变化？）" >&2
    exit 1
fi

eval "$(sed -n "${START},${END}p" "$UPGRADE_SH")"

# 确认函数确实载入了，避免因结构变化导致"空跑通过"
for fn in migrate_binary_location ensure_service_user fix_ownership migrate_database rewrite_unit migrate_layout; do
    if ! declare -F "$fn" >/dev/null; then
        echo "函数 $fn 未能载入" >&2
        exit 1
    fi
done

echo "运行布局迁移..."
migrate_layout

echo ""
echo "断言迁移结果："
check "二进制已移动到新位置"          '[ -f "$BIN_FILE" ]'
check "旧位置的二进制已不存在"        '[ ! -f "$LEGACY_BIN" ]'
check "systemd 单元的 ExecStart 指向新位置" 'grep -q "ExecStart=${BIN_FILE}" "$UNIT_FILE"'
check "单元包含 ProtectSystem 沙箱约束"     'grep -q "^ProtectSystem=strict" "$UNIT_FILE"'
check "单元包含 NoNewPrivileges"            'grep -q "^NoNewPrivileges=true" "$UNIT_FILE"'
check "ReadWritePaths 含程序目录（自更新前提）" 'grep -q "^ReadWritePaths=.*${INSTALL_PATH}" "$UNIT_FILE"'
check "ReadWritePaths 含数据目录"           'grep -q "^ReadWritePaths=.*${DATA_PATH}" "$UNIT_FILE"'
check "保留 CAP_NET_BIND_SERVICE（支持低端口）" 'grep -q "^AmbientCapabilities=CAP_NET_BIND_SERVICE" "$UNIT_FILE"'
check "原单元已备份"                        '[ -n "$UNIT_BACKUP" ] && [ -f "$UNIT_BACKUP" ]'
check "数据库未被移动"                      '[ -f "${DATA_PATH}/gost-panel.db" ]'
check "配置文件未被改动"                    'grep -q "gost-panel.db" "$CONFIG_FILE"'

echo ""
echo "断言幂等性（再跑一次不应产生变化）："
BEFORE=$(md5sum "$UNIT_FILE" | awk '{print $1}')
PREV_BACKUP="$UNIT_BACKUP"
migrate_layout
AFTER=$(md5sum "$UNIT_FILE" | awk '{print $1}')
check "重复迁移不改动 systemd 单元" '[ "$BEFORE" = "$AFTER" ]'
check "重复迁移不重复备份"           '[ "$PREV_BACKUP" = "$UNIT_BACKUP" ]'

echo ""
if [ "$fail" -eq 0 ]; then
    echo "全部通过"
else
    echo "存在失败项" >&2
fi
exit "$fail"
