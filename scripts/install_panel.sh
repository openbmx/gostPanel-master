#!/bin/bash
# ========================================================
#  Gost Panel 一键安装脚本
#  用于从 cc.maipian.de 快速部署
# ========================================================

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
PLAIN='\033[0m'

# 配置
DOWNLOAD_URL="https://cc.maipian.de/gost-node"
VERSION="latest"
INSTALL_PATH="/usr/local/bin"
CONFIG_PATH="/etc/gost-panel"
DATA_PATH="/var/lib/gost-panel"
LOG_PATH="/var/log/gost-panel"
DEFAULT_PORT=39100

echo -e "${GREEN}========================================${PLAIN}"
echo -e "${GREEN}  Gost Panel 一键安装${PLAIN}"
echo -e "${GREEN}========================================${PLAIN}\n"

# 检查 Root 权限
check_root() {
    if [[ $EUID -ne 0 ]]; then
        echo -e "${RED}[错误] 请使用 root 用户运行此脚本${PLAIN}"
        exit 1
    fi
}

# 检测系统架构
get_arch() {
    local arch=$(uname -m)
    case "$arch" in
        x86_64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) echo -e "${RED}[错误] 不支持的架构: $arch${PLAIN}"; exit 1 ;;
    esac
}

# 下载文件
download_binary() {
    local arch=$(get_arch)
    local binary_name="gost-panel-linux-${arch}"
    local url="${DOWNLOAD_URL}/${binary_name}"
    
    echo -e "${BLUE}[1/6] 下载 Gost Panel ($arch)...${PLAIN}"
    echo "URL: $url"
    
    if command -v wget &> /dev/null; then
        wget --no-check-certificate -O /tmp/gost-panel "$url"
    elif command -v curl &> /dev/null; then
        curl -L -k -o /tmp/gost-panel "$url"
    else
        echo -e "${RED}[错误] 需要 wget 或 curl${PLAIN}"
        exit 1
    fi
    
    if [ ! -f /tmp/gost-panel ]; then
        echo -e "${RED}[错误] 下载失败${PLAIN}"
        exit 1
    fi
    
    chmod +x /tmp/gost-panel
    echo -e "${GREEN}✅ 下载完成${PLAIN}"
}

# 安装二进制文件
install_binary() {
    echo -e "${BLUE}[2/6] 安装程序...${PLAIN}"
    
    # 停止旧服务
    if systemctl is-active --quiet gost-panel 2>/dev/null; then
        echo "停止旧服务..."
        systemctl stop gost-panel
    fi
    
    mv /tmp/gost-panel ${INSTALL_PATH}/gost-panel
    chmod +x ${INSTALL_PATH}/gost-panel
    
    echo -e "${GREEN}✅ 安装到 ${INSTALL_PATH}/gost-panel${PLAIN}"
}

# 创建目录结构
create_directories() {
    echo -e "${BLUE}[3/6] 创建目录结构...${PLAIN}"
    
    mkdir -p $CONFIG_PATH
    mkdir -p $DATA_PATH
    mkdir -p $LOG_PATH
    
    echo -e "${GREEN}✅ 目录创建完成${PLAIN}"
}

# 生成配置文件
create_config() {
    echo -e "${BLUE}[4/6] 生成配置文件...${PLAIN}"
    
    # 如果配置文件已存在，备份
    if [ -f ${CONFIG_PATH}/config.yaml ]; then
        cp ${CONFIG_PATH}/config.yaml ${CONFIG_PATH}/config.yaml.bak.$(date +%s)
        echo "已备份旧配置文件"
    fi
    
    # 如果数据库已存在，备份并删除（重新初始化管理员账号）
    if [ -f ${DATA_PATH}/gost-panel.db ]; then
        echo -e "${YELLOW}检测到旧数据库，将备份并重新初始化...${PLAIN}"
        mv ${DATA_PATH}/gost-panel.db ${DATA_PATH}/gost-panel.db.bak.$(date +%s)
    fi
    
    # 生成随机管理员密码
    ADMIN_PASS=$(head -c 16 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 16)
    
    # 使用自定义端口或默认端口
    PORT=${CUSTOM_PORT:-$DEFAULT_PORT}
    
    echo -e "${GREEN}配置信息:${PLAIN}"
    echo -e "  端口: ${BLUE}${PORT}${PLAIN}"
    echo -e "  密码: ${BLUE}${ADMIN_PASS}${PLAIN}"
    
    cat > ${CONFIG_PATH}/config.yaml <<EOF
# Gost Panel 配置文件
server:
  port: ":${PORT}"
  mode: "release"

database:
  type: "sqlite"
  path: "${DATA_PATH}/gost-panel.db"

jwt:
  secret: "$(head -c 16 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 16)"
  expire: 7200

log:
  level: "info"
  format: "json"
  output: "${LOG_PATH}/app.log"

# 初始管理员账号（首次启动后建议修改密码）
admin:
  username: admin
  password: ${ADMIN_PASS}
EOF
    
    echo -e "${GREEN}✅ 配置文件: ${CONFIG_PATH}/config.yaml${PLAIN}"
    echo -e "${YELLOW}⚠️  管理员密码: ${ADMIN_PASS}${PLAIN}"
    echo -e "${YELLOW}   请妥善保管此密码！${PLAIN}"
}

# 创建 systemd 服务
create_service() {
    echo -e "${BLUE}[5/6] 创建系统服务...${PLAIN}"
    
    cat > /etc/systemd/system/gost-panel.service <<EOF
[Unit]
Description=Gost Panel Service
Documentation=https://github.com/openbmx/gostPanel-master
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=${DATA_PATH}
ExecStart=${INSTALL_PATH}/gost-panel -c ${CONFIG_PATH}/config.yaml
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
    
    systemctl daemon-reload
    systemctl enable gost-panel
    
    echo -e "${GREEN}✅ 服务创建完成${PLAIN}"
}

# 启动服务
start_service() {
    echo -e "${BLUE}[6/6] 启动服务...${PLAIN}"
    
    systemctl start gost-panel
    
    sleep 2
    
    if systemctl is-active --quiet gost-panel; then
        echo -e "${GREEN}✅ 服务启动成功${PLAIN}"
    else
        echo -e "${RED}❌ 服务启动失败${PLAIN}"
        echo "查看日志: journalctl -u gost-panel -n 50"
        exit 1
    fi
}

# 显示安装信息
show_info() {
    local ip=$(curl -s -4 https://api.ipify.org 2>/dev/null || curl -s -4 https://ifconfig.me 2>/dev/null || echo "YOUR_SERVER_IP")
    local port=${CUSTOM_PORT:-$DEFAULT_PORT}
    
    echo ""
    echo -e "${GREEN}========================================${PLAIN}"
    echo -e "${GREEN}  安装完成！${PLAIN}"
    echo -e "${GREEN}========================================${PLAIN}"
    echo ""
    echo -e "  访问地址: ${BLUE}http://${ip}:${port}${PLAIN}"
    echo -e "  用户名:   ${BLUE}admin${PLAIN}"
    echo -e "  密码:     ${BLUE}${ADMIN_PASS}${PLAIN}"
    echo ""
    echo -e "${RED}重要提示: 请立即保存上面的密码，并在首次登录后修改！${PLAIN}"
    echo ""
    echo -e "${YELLOW}常用命令:${PLAIN}"
    echo "  启动服务: systemctl start gost-panel"
    echo "  停止服务: systemctl stop gost-panel"
    echo "  重启服务: systemctl restart gost-panel"
    echo "  查看状态: systemctl status gost-panel"
    echo "  查看日志: journalctl -u gost-panel -f"
    echo ""
    echo -e "${YELLOW}配置文件:${PLAIN}"
    echo "  配置: ${CONFIG_PATH}/config.yaml"
    echo "  数据: ${DATA_PATH}"
    echo "  日志: ${LOG_PATH}"
    echo ""
    echo -e "${GREEN}========================================${PLAIN}"
}

# 主程序
main() {
    # 处理端口参数
    if [[ -n "${1:-}" && "${1}" != "uninstall" ]]; then
        CUSTOM_PORT="$1"
        echo -e "${GREEN}使用自定义端口: ${CUSTOM_PORT}${PLAIN}"
    fi
    
    check_root
    download_binary
    install_binary
    create_directories
    create_config
    create_service
    start_service
    show_info
}

# 卸载功能
uninstall() {
    echo -e "${YELLOW}正在卸载 Gost Panel...${PLAIN}"
    
    # 停止并禁用服务
    if systemctl is-active --quiet gost-panel 2>/dev/null; then
        systemctl stop gost-panel
    fi
    systemctl disable gost-panel 2>/dev/null || true
    
    # 删除文件
    rm -f /etc/systemd/system/gost-panel.service
    rm -f ${INSTALL_PATH}/gost-panel
    
    # 询问是否删除数据
    read -p "是否删除配置和数据？[y/N] " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        rm -rf ${CONFIG_PATH}
        rm -rf ${DATA_PATH}
        rm -rf ${LOG_PATH}
        echo -e "${GREEN}✅ 数据已删除${PLAIN}"
    else
        echo -e "${YELLOW}保留了配置和数据${PLAIN}"
    fi
    
    systemctl daemon-reload
    
    echo -e "${GREEN}✅ 卸载完成${PLAIN}"
}

# 参数处理
case "${1:-}" in
    uninstall)
        uninstall
        ;;
    *)
        main "$@"
        ;;
esac
