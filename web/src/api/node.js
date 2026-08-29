import request from '@/utils/request'

/**
 * 获取节点列表
 */
export function getNodeList(params) {
    return request({
        url: '/nodes',
        method: 'get',
        params
    })
}

/**
 * 获取节点详情
 */
export function getNode(id) {
    return request({
        url: `/nodes/${id}`,
        method: 'get'
    })
}

/**
 * 创建节点
 */
export function createNode(data) {
    return request({
        url: '/nodes',
        method: 'post',
        data
    })
}

/**
 * 更新节点
 */
export function updateNode(id, data) {
    return request({
        url: `/nodes/${id}`,
        method: 'put',
        data
    })
}

/**
 * 删除节点
 */
export function deleteNode(id) {
    return request({
        url: `/nodes/${id}`,
        method: 'delete'
    })
}

/**
 * 获取节点配置
 */
export function getNodeConfig(id) {
    return request({
        url: `/nodes/${id}/config`,
        method: 'get'
    })
}

/**
 * 获取节点 API 凭据（仅用于生成安装命令）
 *
 * 安全：节点密码不再随列表/详情下发 —— 它等同于目标主机 GOST 守护进程的完全控制权。
 * 该接口是唯一会返回密码的入口，服务端每次调用都会写入操作日志。
 * 请仅在用户明确点击"安装命令"时调用，不要预取。
 */
export function getNodeCredentials(id) {
    return request({
        url: `/nodes/${id}/credentials`,
        method: 'get'
    })
}
