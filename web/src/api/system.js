import request from '@/utils/request'

// ==================== 版本与在线更新 ====================

/** 当前版本与运行环境 */
export function getVersion() {
    return request({ url: '/system/version', method: 'get' })
}

/** 检查更新。force=true 跳过服务端 20 分钟缓存 */
export function checkUpdate(force = false) {
    return request({
        url: '/system/update/check',
        method: 'get',
        params: force ? { force: 'true' } : {}
    })
}

/**
 * 执行升级。
 * 服务端会下载数十 MB 并校验，耗时远超默认超时，这里单独放宽。
 * 即使前端超时断开，服务端的更新流程仍会跑完（请求上下文已解耦）。
 */
export function performUpdate() {
    return request({ url: '/system/update', method: 'post', timeout: 15 * 60 * 1000 })
}

/** 可回滚的历史版本列表 */
export function getRollbackVersions() {
    return request({ url: '/system/update/rollback-versions', method: 'get' })
}

/** 回滚。不传 version 表示回滚到本地备份 */
export function rollback(version) {
    return request({
        url: '/system/update/rollback',
        method: 'post',
        data: version ? { version } : {},
        timeout: 15 * 60 * 1000
    })
}

/** 重启面板服务 */
export function restartPanel() {
    return request({ url: '/system/restart', method: 'post' })
}

/**
 * 获取系统配置
 */
export function getSystemConfig() {
    return request({
        url: '/system/config',
        method: 'get'
    })
}

/**
 * 获取公开系统配置
 */
export function getPublicSystemConfig() {
    return request({
        url: '/system/public-config',
        method: 'get'
    })
}

/**
 * 更新系统配置
 */
export function updateSystemConfig(data) {
    return request({
        url: '/system/config',
        method: 'put',
        data
    })
}

export function sendTestEmail(data) {
    return request({
        url: '/system/email/test',
        method: 'post',
        data
    })
}

export function backupSystem() {
    return request({
        url: '/system/backup',
        method: 'post'
    })
}
