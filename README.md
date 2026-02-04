# Gost Panel

<div align="center">

**现代化 Gost v3 端口转发管理控制面板（主要面向中国用户-主要功能是正常的就是流量统计还有点问题）**

[![License](https://img.shields.io/github/license/qiuapeng921/gostPanel)](./LICENSE)
[![Docker Support](https://img.shields.io/badge/Docker-Ready-2496ED?logo=docker&logoColor=white)](./Dockerfile)
[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Vue Version](https://img.shields.io/badge/Vue-3.x-4FC08D?logo=vuedotjs&logoColor=white)](https://vuejs.org/)

</div>

---

## 📖 简介

**Gost Panel** 是一个功能强大的可视化管理面板，专为 [Gost v3](https://github.com/go-gost/gost) 安全隧道工具设计。它旨在简化复杂的网络转发配置，提供直观的 Web 界面来统一管理多台服务器节点、配置端口转发规则、监控链路延迟，并支持复杂的多级代理隧道链。

## ✨ 核心特性

- ⚡ **统一节点管理** - 在一个面板集中管理所有 Gost 客户端节点。
- 🔄 **灵活转发规则** - 支持 TCP/UDP 端口转发，简单易用。
- 🔗 **高级隧道编排** - 支持复杂的多跳隧道链路（入口 -> 中转 -> 出口）配置。
- 📊 **实时状态监控** - 定时检测节点连通性及转发链路延迟（Ping）。
- 🛡️ **安全与审计** - 内置 API 认证机制，记录完整的用户操作日志。
- 🐳 **容器化支持** - 原生支持 Docker 及 Docker Compose 一键部署。

## 📸 界面预览

| 仪表盘 | 节点管理 |
| :---: | :---: |
| <img src="./docs/screenshots/dash.png" alt="Dashboard" width="100%"> | <img src="./docs/screenshots/node.png" alt="Node Management" width="100%"> |
| **转发规则** | **隧道管理** |
| <img src="./docs/screenshots/forwards.png" alt="Forward Rules" width="100%"> | <img src="./docs/screenshots/tunnels.png" alt="Tunnel Management" width="100%"> |

## 🚀 快速部署

### 默认账户
> ⚠️ 部署后请务必修改默认密码！
- **访问地址**: `http://IP:39100` (默认端口)
- **用户名**: `admin`
- **密码**: `部署后会自动生成`

### 方式 A：Docker Compose (已废弃)

如果您熟悉 Docker，这是最干净的部署方式。

1. **获取配置文件**
   ```bash
   curl -sSL https://raw.githubusercontent.com/apicoder-peng/gostPanel/master/docker-compose.yml -o docker-compose.yml
   ```

2. **启动服务**
   ```bash
   docker-compose up -d
   ```

### 方式 B：一键安装脚本 (Linux) 主要是方便中国用户快速部署

适用于裸机部署，要求系统内存 > 128MB。

**默认安装 (端口 39100):**
```bash
bash <(curl -sSL https:/cc.maipian.de/gost-node/install_panel.sh)
```

**自定义端口 (例如 8080):**
```bash
bash <(curl -sSL https:/cc.maipian.de/gost-node/install_panel.sh) 8080
```

### 常用管理命令

**卸载面板:**
```bash
bash <(curl -sSL https:/cc.maipian.de/gost-node/install_panel.sh) uninstall
```

**被控端 (Agent) 卸载:**
```bash
bash <(curl -sSL https:/cc.maipian.de/gost-node/install_node.sh) uninstall
```

## 🕹️ 使用指南

### 添加节点 (Agent)

Gost Panel 采用 "服务端 - 客户端" 架构。您需要将其他服务器作为“节点”添加到面板中。

1. 登录 Gost Panel。
2. 进入 **节点管理** 页面。
3. 点击 **添加节点**，获取该节点的安装命令。
4. 在目标服务器（VPS）上执行复制的命令即可自动注册上线。

---

## 🛠️ 本地开发与构建

如果您想参与开发或自行编译：

### 环境要求
- Go 1.23+
- Node.js 18+ (包含 npm)

### 构建命令
项目根目录下提供了 `Makefile` 方便操作：

```bash
# 1. 编译前端和后端 (生成二进制文件 gost-panel)
make build

# 2. 仅编译前端
make build-web

# 3. 仅编译后端
make build-server

# 4. 运行开发模式
make run
```

### 配置文件
默认配置文件位于 `config/config.yaml`。您可以在此修改端口、数据库设置和日志级别。

## 📄 开源许可证

本项目基于 [MIT License](./LICENSE) 开源。
