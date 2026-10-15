#!/usr/bin/env bash
# ============================================================================
# 升级脚本的布局迁移演练
#
# 在临时目录里搭出几种存量部署的样子，按 upgrade_panel.sh 中 main 的真实顺序
# （check_installed → backup_current → do_upgrade → 成功后清理 / 失败时回滚）
# 跑一遍，断言结果符合预期。
#
# 必须按真实顺序跑，只单独调用迁移函数是不够的：早期版本的 check_installed
# 会把 BIN_FILE 改写成旧路径，迁移函数单独测试一切正常，真实升级时却误以为
# 二进制已在新位置、并把旧路径写回了 systemd 单元 —— 新二进制装好了，
# 服务却继续跑旧版本，升级"成功"而什么都没变。
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

WORK_ROOT=$(mktemp -d)
trap 'rm -rf "$WORK_ROOT"' EXIT

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
# 从 unit_value() 开始、到 main() 之前，源文件中是一段连续的函数定义。
# 整块提取比逐个匹配稳健：函数体内含有 heredoc 与嵌套大括号，按函数名逐个 awk 会截断。
START=$(grep -n '^unit_value() {' "$UPGRADE_SH" | cut -d: -f1)
END=$(grep -n '^main() {' "$UPGRADE_SH" | cut -d: -f1)

if [ -z "$START" ] || [ -z "$END" ]; then
    echo "无法在 upgrade_panel.sh 中定位被测函数区间（脚本结构已变化？）" >&2
    exit 1
fi

eval "$(sed -n "${START},$((END - 1))p" "$UPGRADE_SH")"

# 确认函数确实载入了，避免因结构变化导致"空跑通过"
for fn in unit_exec_bin migrate_binary_location ensure_service_user fix_ownership migrate_database \
          rewrite_unit migrate_layout check_installed backup_current do_upgrade rollback \
          cleanup_legacy_bin release_tag asset_meta; do
    if ! declare -F "$fn" >/dev/null; then
        echo "函数 $fn 未能载入" >&2
        exit 1
    fi
done

# --- 搭建部署 ------------------------------------------------------------
# 生成假的面板二进制，--version 输出指定版本
fake_bin() {
    mkdir -p "$(dirname "$1")"
    printf '#!/bin/sh\necho "gost-panel %s"\n' "$2" > "$1"
    chmod +x "$1"
}

version_of() { "$1" --version | awk '{print $NF}'; }

exec_start_is() { grep -qxF "ExecStart=$1 -c ${CONFIG_FILE}" "$UNIT_FILE"; }

# setup <场景>
#   legacy  旧版布局：二进制只在 /usr/local/bin，单元以 root 运行、没有沙箱约束
#   split   早期升级脚本留下的错位状态：/opt 与 /usr/local/bin 都有二进制，
#           单元已是新模板，ExecStart 却仍指向 /usr/local/bin（服务在跑旧版本）
#   current 新版布局：二进制在 /opt/gost-panel，单元已是新模板
setup() {
    WORK="${WORK_ROOT}/$1"
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
    CURRENT_BIN=""
    BACKUP_BIN=""
    DB_BACKUP=""

    mkdir -p "$CONFIG_PATH" "$DATA_PATH" "$LOG_PATH" "$(dirname "$UNIT_FILE")"
    printf 'database:\n  path: "%s/gost-panel.db"\n' "$DATA_PATH" > "$CONFIG_FILE"
    printf 'fake db' > "${DATA_PATH}/gost-panel.db"

    # 本次要装上的新版本
    NEW_BIN="${WORK}/download/gost-panel"
    fake_bin "$NEW_BIN" 9.9.9

    case "$1" in
        legacy)
            fake_bin "$LEGACY_BIN" v1.0.4
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
            ;;
        split)
            fake_bin "$LEGACY_BIN" v1.0.4
            fake_bin "$BIN_FILE" 1.1.1
            # 复现早期脚本的产物：按新模板写单元，ExecStart 却是旧路径
            local real_bin="$BIN_FILE"
            BIN_FILE="$LEGACY_BIN"
            rewrite_unit
            BIN_FILE="$real_bin"
            UNIT_BACKUP=""
            ;;
        current)
            fake_bin "$BIN_FILE" 1.1.1
            rewrite_unit
            UNIT_BACKUP=""
            ;;
    esac
}

# 按 main 中的真实顺序执行升级（下载与健康检查除外）
run_upgrade() {
    check_installed
    backup_current
    do_upgrade
}

# --- 场景 ----------------------------------------------------------------
echo "场景一：旧版布局（二进制在 /usr/local/bin、以 root 运行）"
setup legacy
run_upgrade
check "识别出升级前运行的是旧位置的二进制"     '[ "$CURRENT_BIN" = "$LEGACY_BIN" ]'
check "BIN_FILE 未被改写，仍指向新位置"        '[ "$BIN_FILE" = "${INSTALL_PATH}/gost-panel" ]'
check "systemd 单元的 ExecStart 指向新位置"    'exec_start_is "$BIN_FILE"'
check "新版本已安装到新位置"                   '[ "$(version_of "$BIN_FILE")" = 9.9.9 ]'
check "旧位置的二进制已不存在"                 '[ ! -f "$LEGACY_BIN" ]'
check "旧二进制已备份"                         '[ "$(version_of "$BACKUP_BIN")" = v1.0.4 ]'
check "单元包含 ProtectSystem 沙箱约束"        'grep -q "^ProtectSystem=strict" "$UNIT_FILE"'
check "单元包含 NoNewPrivileges"               'grep -q "^NoNewPrivileges=true" "$UNIT_FILE"'
check "ReadWritePaths 含程序目录（自更新前提）" 'grep -q "^ReadWritePaths=.*${INSTALL_PATH}" "$UNIT_FILE"'
check "ReadWritePaths 含数据目录"              'grep -q "^ReadWritePaths=.*${DATA_PATH}" "$UNIT_FILE"'
check "保留 CAP_NET_BIND_SERVICE（支持低端口）" 'grep -q "^AmbientCapabilities=CAP_NET_BIND_SERVICE" "$UNIT_FILE"'
check "原单元已备份"                           '[ -n "$UNIT_BACKUP" ] && [ -f "$UNIT_BACKUP" ]'
check "数据库未被移动"                         '[ -f "${DATA_PATH}/gost-panel.db" ]'
check "数据库已备份"                           '[ -n "$DB_BACKUP" ] && [ -f "$DB_BACKUP" ]'
check "配置文件未被改动"                       'grep -q "gost-panel.db" "$CONFIG_FILE"'

echo "  幂等性（再跑一次迁移不应产生变化）："
BEFORE=$(md5sum "$UNIT_FILE" | awk '{print $1}')
PREV_BACKUP="$UNIT_BACKUP"
migrate_layout
AFTER=$(md5sum "$UNIT_FILE" | awk '{print $1}')
check "重复迁移不改动 systemd 单元" '[ "$BEFORE" = "$AFTER" ]'
check "重复迁移不重复备份"           '[ "$PREV_BACKUP" = "$UNIT_BACKUP" ]'

echo ""
echo "场景二：旧版布局升级失败后回滚"
setup legacy
run_upgrade
( rollback ) || true
check "ExecStart 指回旧位置"           'exec_start_is "$LEGACY_BIN"'
check "旧位置的二进制已还原为旧版本"   '[ "$(version_of "$LEGACY_BIN")" = v1.0.4 ]'

echo ""
echo "场景三：早期升级脚本留下的错位状态（两处都有二进制，单元指向旧位置）"
setup split
check "（前提）单元已是新模板，ExecStart 却指向旧位置" 'exec_start_is "$LEGACY_BIN" && grep -q "^ProtectSystem=strict" "$UNIT_FILE"'
run_upgrade
check "识别出服务实际运行的是旧位置的二进制" '[ "$CURRENT_BIN" = "$LEGACY_BIN" ]'
check "备份的是实际运行的旧版本"             '[ "$(version_of "$BACKUP_BIN")" = v1.0.4 ]'
check "ExecStart 改为指向新位置"             'exec_start_is "$BIN_FILE"'
check "新版本已安装到新位置"                 '[ "$(version_of "$BIN_FILE")" = 9.9.9 ]'
check "健康检查通过前旧二进制保持不动"       '[ -f "$LEGACY_BIN" ]'
cleanup_legacy_bin
check "升级成功后清理掉旧位置的二进制"       '[ ! -f "$LEGACY_BIN" ]'

echo ""
echo "场景四：错位状态下升级失败后回滚"
setup split
run_upgrade
( rollback ) || true
check "ExecStart 指回旧位置"   'exec_start_is "$LEGACY_BIN"'
check "旧二进制仍是旧版本"     '[ "$(version_of "$LEGACY_BIN")" = v1.0.4 ]'

echo ""
echo "场景五：新版布局的常规升级与回滚"
setup current
run_upgrade
check "升级前运行的二进制即新位置"   '[ "$CURRENT_BIN" = "$BIN_FILE" ]'
check "单元无需改写（未产生备份）"   '[ -z "$UNIT_BACKUP" ]'
check "新版本已安装"                 '[ "$(version_of "$BIN_FILE")" = 9.9.9 ]'
( rollback ) || true
check "回滚后还原为旧版本"           '[ "$(version_of "$BIN_FILE")" = 1.1.1 ]'

echo ""
echo "场景六：解析 GitHub API 返回的 release 元数据"
# 结构与真实响应一致（节选）：产物对象内嵌 uploader 对象；
# arm64 刻意不带 digest，正文里刻意混入形似字段的转义文本。
PRETTY_JSON="${WORK_ROOT}/release.json"
cat > "$PRETTY_JSON" <<'EOF'
{
  "url": "https://api.github.com/repos/openbmx/gostPanel-master/releases/1",
  "id": 1,
  "author": {
    "login": "openbmx",
    "id": 2
  },
  "tag_name": "v1.2.0",
  "name": "Gost Panel 1.2.0",
  "body": "正文 \"name\": \"gost-panel-linux-amd64.tar.gz\", \"digest\": \"sha256:0000000000000000000000000000000000000000000000000000000000000000\" {不是字段}",
  "assets": [
    {
      "url": "https://api.github.com/repos/openbmx/gostPanel-master/releases/assets/11",
      "id": 11,
      "node_id": "RA_1",
      "name": "gost-panel-linux-arm64.tar.gz",
      "label": "",
      "uploader": {
        "login": "github-actions[bot]",
        "id": 41898282,
        "following_url": "https://api.github.com/users/x/following{/other_user}"
      },
      "size": 8177946,
      "browser_download_url": "https://github.com/openbmx/gostPanel-master/releases/download/v1.2.0/gost-panel-linux-arm64.tar.gz"
    },
    {
      "url": "https://api.github.com/repos/openbmx/gostPanel-master/releases/assets/617163004",
      "id": 617163004,
      "node_id": "RA_2",
      "name": "gost-panel-linux-amd64.tar.gz",
      "label": "",
      "uploader": {
        "login": "github-actions[bot]",
        "id": 41898282
      },
      "size": 9007803,
      "digest": "sha256:2658be882b9b6a39f5a6e8f58b769a6eee3f3defe20e6a2d84e4c4dd8dc0b946",
      "browser_download_url": "https://github.com/openbmx/gostPanel-master/releases/download/v1.2.0/gost-panel-linux-amd64.tar.gz"
    }
  ]
}
EOF
# 同一份数据的紧凑格式（无换行与缩进）
COMPACT_JSON="${WORK_ROOT}/release-compact.json"
tr -d '\n' < "$PRETTY_JSON" | sed -E 's/": +/":/g; s/ {2,}//g' > "$COMPACT_JSON"

meta_of() {
    local id="" digest=""
    read -r id digest <<< "$(asset_meta "$1")" || true
    echo "${id}|${digest}"
}

AMD64_DIGEST="2658be882b9b6a39f5a6e8f58b769a6eee3f3defe20e6a2d84e4c4dd8dc0b946"
for RELEASE_JSON in "$PRETTY_JSON" "$COMPACT_JSON"; do
    fmt=$([ "$RELEASE_JSON" = "$PRETTY_JSON" ] && echo "格式化" || echo "紧凑")
    check "（${fmt}）解析出 tag_name"                   '[ "$(release_tag)" = v1.2.0 ]'
    check "（${fmt}）解析出产物 ID 与 SHA256"            '[ "$(meta_of gost-panel-linux-amd64.tar.gz)" = "617163004|${AMD64_DIGEST}" ]'
    check "（${fmt}）产物缺 digest 时不串用相邻产物的"   '[ "$(meta_of gost-panel-linux-arm64.tar.gz)" = "11|" ]'
    check "（${fmt}）不存在的产物返回空"                 '[ "$(meta_of gost-panel-linux-mips.tar.gz)" = "|" ]'
done

echo ""
if [ "$fail" -eq 0 ]; then
    echo "全部通过"
else
    echo "存在失败项" >&2
fi
exit "$fail"
