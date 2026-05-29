#!/usr/bin/env bash
# ========================================================
#  Gost Panel 一体化管理脚本（傻瓜式交互菜单）
#  集成：安装 / 升级 / 卸载 / 状态 / 启停 / 日志 / 备份
#  适用：服务端（主控）与被控端（节点）
#  仓库：https://github.com/openbmx/gostPanel-master
#
#  快速启动：
#    bash <(curl -sSL https://raw.githubusercontent.com/openbmx/gostPanel-master/main/scripts/gost.sh)
# ========================================================

set -o pipefail

# ---------------- 颜色 ----------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
PLAIN='\033[0m'

# ---------------- 全局配置（可用环境变量覆盖）----------------
REPO="${REPO:-openbmx/gostPanel-master}"
RAW_BASE="${RAW_BASE:-https://raw.githubusercontent.com/${REPO}/main/scripts}"
GH_PROXY="${GH_PROXY:-}"

# 服务端路径
PANEL_BIN="/usr/local/bin/gost-panel"
PANEL_CONFIG="/etc/gost-panel/config.yaml"
PANEL_DATA="/var/lib/gost-panel"
PANEL_SERVICE="gost-panel"

# 节点路径
NODE_BIN="/usr/local/bin/gost-node"
NODE_CONF="/etc/gost-node/config.yaml"
NODE_SERVICE="gost-node"

# 远程脚本
INSTALL_PANEL_SH="${RAW_BASE}/install_panel.sh"
UPGRADE_PANEL_SH="${RAW_BASE}/upgrade_panel.sh"
INSTALL_NODE_SH="${RAW_BASE}/install_node.sh"
UPGRADE_NODE_SH="${RAW_BASE}/upgrade_node.sh"

# ---------------- 输出辅助 ----------------
info()  { echo -e "${BLUE}[信息]${PLAIN} $1"; }
ok()    { echo -e "${GREEN}[成功]${PLAIN} $1"; }
warn()  { echo -e "${YELLOW}[警告]${PLAIN} $1"; }
err()   { echo -e "${RED}[错误]${PLAIN} $1"; }
title() { echo -e "\n${CYAN}==== $1 ====${PLAIN}"; }

pause() {
    echo ""
    read -rp "$(echo -e "${YELLOW}按回车键返回菜单...${PLAIN}")" _
}

# ---------------- 基础检查 ----------------
check_root() {
    if [[ $EUID -ne 0 ]]; then
        err "请使用 root 用户运行（或使用 sudo）"
        exit 1
    fi
}

# 运行远程脚本：fetch_run <url> [args...]
fetch_run() {
    local url="$1"; shift
    local full="${GH_PROXY}${url}"
    info "执行: ${full} $*"
    if command -v curl >/dev/null 2>&1; then
        bash <(curl -sSL "$full") "$@"
    elif command -v wget >/dev/null 2>&1; then
        bash <(wget -qO- "$full") "$@"
    else
        err "需要 curl 或 wget"
        return 1
    fi
}

# 服务是否存在
svc_exists() {
    local name="$1"
    [ -f "/etc/systemd/system/${name}.service" ] || [ -f "/etc/init.d/${name}" ]
}

# 服务是否运行（兼容 systemd / openrc）
svc_running() {
    local name="$1"
    if command -v systemctl >/dev/null 2>&1; then
        systemctl is-active --quiet "$name" 2>/dev/null && return 0
    fi
    if [ -f "/etc/init.d/${name}" ]; then
        rc-service "$name" status >/dev/null 2>&1 && return 0
    fi
    return 1
}

# 服务控制：svc_ctl <name> <start|stop|restart>
svc_ctl() {
    local name="$1" action="$2"
    if command -v systemctl >/dev/null 2>&1 && [ -f "/etc/systemd/system/${name}.service" ]; then
        systemctl "$action" "$name"
    elif [ -f "/etc/init.d/${name}" ]; then
        rc-service "$name" "$action"
    else
        err "未找到服务 ${name}"
        return 1
    fi
}

# 查看日志
svc_log() {
    local name="$1"
    if command -v journalctl >/dev/null 2>&1 && [ -f "/etc/systemd/system/${name}.service" ]; then
        info "按 Ctrl+C 退出日志查看"
        journalctl -u "$name" -n 100 -f
    else
        warn "未使用 systemd，请自行查看服务日志（如 /var/log/）"
    fi
}

# 状态徽标
status_badge() {
    local name="$1"
    if ! svc_exists "$name"; then
        echo -e "${YELLOW}未安装${PLAIN}"
    elif svc_running "$name"; then
        echo -e "${GREEN}运行中${PLAIN}"
    else
        echo -e "${RED}已停止${PLAIN}"
    fi
}

# 解析服务端端口
panel_port() {
    local port=""
    [ -f "$PANEL_CONFIG" ] && port=$(grep -E '^\s*port:' "$PANEL_CONFIG" | head -n1 | sed -E 's/.*port:\s*"?:?([0-9]+)"?.*/\1/')
    echo "${port:-39100}"
}

# 公网 IP
public_ip() {
    curl -s -4 --max-time 5 https://api.ipify.org 2>/dev/null \
        || curl -s -4 --max-time 5 https://ifconfig.me 2>/dev/null \
        || echo "服务器IP"
}

# ============================================================
#                    服务端（主控）操作
# ============================================================
panel_install() {
    title "安装 / 重装 服务端"
    if svc_exists "$PANEL_SERVICE"; then
        warn "检测到服务端已安装。重装会重新生成管理员密码（旧数据库会被备份后重新初始化）。"
        warn "若只想更新版本且保留数据，请选择「升级」而不是重装。"
        read -rp "确认要继续安装/重装吗？[y/N] " a
        [[ "$a" =~ ^[Yy]$ ]] || { info "已取消"; return; }
    fi
    read -rp "请输入面板端口（直接回车使用默认 39100）: " p
    if [ -n "$p" ]; then
        fetch_run "$INSTALL_PANEL_SH" "$p"
    else
        fetch_run "$INSTALL_PANEL_SH"
    fi
}

panel_upgrade() {
    title "升级 服务端（保留配置与数据）"
    if ! svc_exists "$PANEL_SERVICE"; then
        err "尚未安装服务端，无法升级。"
        return
    fi
    read -rp "升级到指定版本？留空=最新版（例如 v1.2.0）: " ver
    if [ -n "$ver" ]; then
        VERSION="$ver" fetch_run "$UPGRADE_PANEL_SH"
    else
        fetch_run "$UPGRADE_PANEL_SH"
    fi
}

panel_uninstall() {
    title "卸载 服务端"
    if ! svc_exists "$PANEL_SERVICE"; then
        warn "服务端未安装。"
        return
    fi
    fetch_run "$INSTALL_PANEL_SH" uninstall
}

panel_info() {
    title "服务端信息"
    if ! svc_exists "$PANEL_SERVICE"; then
        warn "服务端未安装。"
        return
    fi
    local port ip ver
    port=$(panel_port)
    ip=$(public_ip)
    ver=$("$PANEL_BIN" --version 2>/dev/null || echo "未知")
    echo -e "  状态:     $(status_badge "$PANEL_SERVICE")"
    echo -e "  版本:     ${BLUE}${ver}${PLAIN}"
    echo -e "  访问地址: ${BLUE}http://${ip}:${port}${PLAIN}"
    echo -e "  配置文件: ${PANEL_CONFIG}"
    echo -e "  数据目录: ${PANEL_DATA}"
}

# ============================================================
#                    被控端（节点）操作
# ============================================================
node_install() {
    title "安装 / 重装 节点"
    read -rp "节点 API 端口（直接回车使用默认 39000）: " p
    read -rp "API 用户名（留空自动生成）: " u
    read -rp "API 密码（留空自动生成）: " pw
    fetch_run "$INSTALL_NODE_SH" "${p:-39000}" "$u" "$pw"
}

node_upgrade() {
    title "升级 节点（保留配置与账号）"
    if ! svc_exists "$NODE_SERVICE"; then
        err "尚未安装节点，无法升级。"
        return
    fi
    read -rp "目标 GOST 版本（留空=脚本默认）例如 3.2.6: " gv
    if [ -n "$gv" ]; then
        GOST_VERSION="$gv" fetch_run "$UPGRADE_NODE_SH"
    else
        fetch_run "$UPGRADE_NODE_SH"
    fi
}

node_uninstall() {
    title "卸载 节点"
    if ! svc_exists "$NODE_SERVICE"; then
        warn "节点未安装。"
        return
    fi
    fetch_run "$INSTALL_NODE_SH" uninstall
}

# ============================================================
#                    通用服务控制
# ============================================================
# pick_target：返回 panel 或 node
pick_target() {
    echo "" >&2
    echo -e "  ${GREEN}1.${PLAIN} 服务端 (gost-panel)" >&2
    echo -e "  ${GREEN}2.${PLAIN} 节点    (gost-node)" >&2
    read -rp "请选择操作对象 [1/2]: " t
    case "$t" in
        1) echo "$PANEL_SERVICE" ;;
        2) echo "$NODE_SERVICE" ;;
        *) echo "" ;;
    esac
}

ctrl_menu() {
    local action="$1" desc="$2"
    local svc
    svc=$(pick_target)
    if [ -z "$svc" ]; then
        err "无效选择"
        return
    fi
    if ! svc_exists "$svc"; then
        err "服务 ${svc} 未安装"
        return
    fi
    info "${desc} ${svc} ..."
    svc_ctl "$svc" "$action" && ok "${desc}完成" || err "${desc}失败"
}

log_menu() {
    local svc
    svc=$(pick_target)
    [ -z "$svc" ] && { err "无效选择"; return; }
    svc_exists "$svc" || { err "服务 ${svc} 未安装"; return; }
    svc_log "$svc"
}

# ============================================================
#                       主菜单
# ============================================================
show_menu() {
    clear 2>/dev/null || true
    echo -e "${CYAN}=====================================================${PLAIN}"
    echo -e "${CYAN}        Gost Panel 一体化管理脚本${PLAIN}"
    echo -e "${CYAN}=====================================================${PLAIN}"
    echo -e "  服务端: $(status_badge "$PANEL_SERVICE")    节点: $(status_badge "$NODE_SERVICE")"
    echo -e "${CYAN}-----------------------------------------------------${PLAIN}"
    echo -e "  ${GREEN}--- 服务端（主控）---${PLAIN}"
    echo -e "   ${GREEN}1.${PLAIN} 安装 / 重装服务端"
    echo -e "   ${GREEN}2.${PLAIN} 升级服务端（保留数据）"
    echo -e "   ${GREEN}3.${PLAIN} 卸载服务端"
    echo -e "   ${GREEN}4.${PLAIN} 查看服务端信息（地址/版本）"
    echo ""
    echo -e "  ${GREEN}--- 被控端（节点）---${PLAIN}"
    echo -e "   ${GREEN}5.${PLAIN} 安装 / 重装节点"
    echo -e "   ${GREEN}6.${PLAIN} 升级节点（保留账号）"
    echo -e "   ${GREEN}7.${PLAIN} 卸载节点"
    echo ""
    echo -e "  ${GREEN}--- 通用 ---${PLAIN}"
    echo -e "   ${GREEN}8.${PLAIN} 启动服务"
    echo -e "   ${GREEN}9.${PLAIN} 停止服务"
    echo -e "  ${GREEN}10.${PLAIN} 重启服务"
    echo -e "  ${GREEN}11.${PLAIN} 查看实时日志"
    echo ""
    echo -e "   ${GREEN}0.${PLAIN} 退出"
    echo -e "${CYAN}=====================================================${PLAIN}"
}

main() {
    check_root
    while true; do
        show_menu
        read -rp "请输入选项 [0-11]: " choice
        case "$choice" in
            1) panel_install; pause ;;
            2) panel_upgrade; pause ;;
            3) panel_uninstall; pause ;;
            4) panel_info; pause ;;
            5) node_install; pause ;;
            6) node_upgrade; pause ;;
            7) node_uninstall; pause ;;
            8) ctrl_menu start "启动"; pause ;;
            9) ctrl_menu stop "停止"; pause ;;
            10) ctrl_menu restart "重启"; pause ;;
            11) log_menu ;;
            0) echo -e "${GREEN}再见！${PLAIN}"; exit 0 ;;
            *) err "无效选项，请重新输入"; sleep 1 ;;
        esac
    done
}

main "$@"
