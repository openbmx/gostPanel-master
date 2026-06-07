<template>
  <div class="page-container">
    <div class="page-header">
      <h3>隧道管理</h3>
    </div>

    <el-card shadow="hover">
      <div class="search-bar">
        <div class="filters">
          <el-input
            v-model="searchKeyword"
            placeholder="搜索隧道名称"
            :prefix-icon="Search"
            clearable
            style="width: 250px"
            @clear="handleSearch"
            @keyup.enter="handleSearch"
          />
          <el-select v-model="searchNodeId" placeholder="选择节点" clearable style="width: 180px" @change="handleSearch">
            <el-option v-for="node in nodeList" :key="node.id" :label="node.name" :value="node.id" />
          </el-select>
          <el-select v-model="searchStatus" placeholder="状态" clearable style="width: 120px" @change="handleSearch">
            <el-option label="运行中" value="running" />
            <el-option label="已停止" value="stopped" />
            <el-option label="错误" value="error" />
          </el-select>
          <el-button :icon="Search" @click="handleSearch">搜索</el-button>
          <el-button :icon="Refresh" @click="fetchData">刷新</el-button>
        </div>
        <el-button type="primary" :icon="Plus" @click="openDialog()">添加隧道</el-button>
      </div>

      <el-table :data="tunnelList" v-loading="loading" style="width: 100%" border>
        <el-table-column prop="id" label="ID" width="70" align="center" />
        <el-table-column prop="name" label="隧道名称" min-width="150" align="center" show-overflow-tooltip />
        <el-table-column label="链路" min-width="260" align="center" show-overflow-tooltip>
          <template #default="{ row }">
            {{ getTunnelPath(row) }}
          </template>
        </el-table-column>
        <el-table-column label="跳数" width="80" align="center">
          <template #default="{ row }">
            <el-tag size="small">{{ getTunnelHops(row).length }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="出口" width="140" align="center">
          <template #default="{ row }">
            <el-tag size="small" type="success">{{ row.exit_node?.name || '-' }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="status" label="状态" width="100" align="center">
          <template #default="{ row }">
            <el-tag :type="getStatusType(row.status)" size="small">{{ getStatusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="remark" label="备注" min-width="150" show-overflow-tooltip />
        <el-table-column label="操作" width="180" align="center" fixed="right">
          <template #default="{ row }">
            <el-button v-if="row.status !== 'running'" type="success" link size="small" @click="handleStart(row)">启动</el-button>
            <el-button v-else type="warning" link size="small" @click="handleStop(row)">停止</el-button>
            <el-button type="primary" link size="small" :disabled="row.status === 'running'" @click="openDialog(row)">编辑</el-button>
            <el-button type="danger" link size="small" @click="handleDelete(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="pagination">
        <el-pagination
          v-model:current-page="page"
          v-model:page-size="pageSize"
          :total="total"
          :page-sizes="[10, 20, 50, 100]"
          layout="total, sizes, prev, pager, next"
          @size-change="fetchData"
          @current-change="fetchData"
        />
      </div>
    </el-card>

    <el-dialog
      v-model="dialogVisible"
      :title="isEdit ? '编辑隧道' : '添加隧道'"
      width="760px"
      :close-on-click-modal="false"
    >
      <el-form ref="formRef" :model="form" :rules="formRules" label-width="100px">
        <el-form-item label="隧道名称" prop="name">
          <el-input v-model="form.name" placeholder="请输入隧道名称" :prefix-icon="EditPen" />
        </el-form-item>

        <el-divider content-position="left">链路配置</el-divider>

        <el-form-item label="入口节点" prop="entry_node_id">
          <el-select v-model="form.entry_node_id" placeholder="选择入口节点" style="width: 100%" :disabled="isEdit">
            <el-option
              v-for="node in nodeList"
              :key="node.id"
              :label="node.name"
              :value="node.id"
              :disabled="isNodeUsedInHops(node.id)"
            />
          </el-select>
          <div class="form-hint">规则监听服务运行在入口节点，链路从这里开始。</div>
        </el-form-item>

        <el-form-item label="链路跳点" prop="hops">
          <el-table :data="form.hops" border size="small" style="width: 100%">
            <el-table-column label="#" width="50" align="center">
              <template #default="{ $index }">{{ $index + 1 }}</template>
            </el-table-column>
            <el-table-column label="节点" min-width="190">
              <template #default="{ row, $index }">
                <el-select v-model="row.node_id" placeholder="选择节点" style="width: 100%">
                  <el-option
                    v-for="node in nodeList"
                    :key="node.id"
                    :label="node.name"
                    :value="node.id"
                    :disabled="isHopNodeDisabled(node.id, $index)"
                  />
                </el-select>
              </template>
            </el-table-column>
            <el-table-column label="协议" width="150">
              <template #default="{ row }">
                <el-select v-model="row.protocol" style="width: 100%">
                  <el-option v-for="item in protocolOptions" :key="item.value" :label="item.label" :value="item.value" />
                </el-select>
              </template>
            </el-table-column>
            <el-table-column label="Relay端口" width="140">
              <template #default="{ row }">
                <el-input-number v-model="row.relay_port" :min="1" :max="65535" controls-position="right" style="width: 100%" />
              </template>
            </el-table-column>
            <el-table-column label="操作" width="150" align="center">
              <template #default="{ $index }">
                <el-button link :icon="ArrowUp" :disabled="$index === 0" @click="moveHop($index, -1)" />
                <el-button link :icon="ArrowDown" :disabled="$index === form.hops.length - 1" @click="moveHop($index, 1)" />
                <el-button type="danger" link :icon="Delete" :disabled="form.hops.length <= 1" @click="removeHop($index)" />
              </template>
            </el-table-column>
          </el-table>
          <div class="hop-actions">
            <el-button type="primary" link :icon="Plus" @click="addHop">添加跳点</el-button>
          </div>
          <div class="form-hint">最后一个跳点就是出口节点。每个跳点会在对应节点上创建 Relay 服务。</div>
        </el-form-item>

        <el-form-item label="备注" prop="remark">
          <el-input v-model="form.remark" type="textarea" :rows="2" placeholder="备注信息" />
        </el-form-item>
      </el-form>

      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitLoading" @click="handleSubmit">确定</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted, onBeforeUnmount } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { ArrowDown, ArrowUp, Delete, EditPen, Plus, Refresh, Search } from '@element-plus/icons-vue'
import { getTunnelList, createTunnel, updateTunnel, deleteTunnel, startTunnel, stopTunnel } from '@/api/tunnel'
import { getNodeList } from '@/api/node'

const nodeList = ref([])
const tunnelList = ref([])
const loading = ref(false)
const page = ref(1)
const pageSize = ref(10)
const total = ref(0)

const searchKeyword = ref('')
const searchNodeId = ref('')
const searchStatus = ref('')

const dialogVisible = ref(false)
const isEdit = ref(false)
const editId = ref(null)
const submitLoading = ref(false)
const formRef = ref(null)

const protocolOptions = [
  { label: 'WebSocket (ws)', value: 'ws' },
  { label: 'WebSocket Secure (wss)', value: 'wss' },
  { label: 'Multiplex WebSocket (mws)', value: 'mws' },
  { label: 'Multiplex WSS (mwss)', value: 'mwss' },
  { label: 'HTTP/2 (h2)', value: 'h2' },
  { label: 'gRPC', value: 'grpc' },
  { label: 'QUIC', value: 'quic' },
  { label: 'KCP', value: 'kcp' },
  { label: 'TLS', value: 'tls' },
  { label: 'Multiplex TLS (mtls)', value: 'mtls' },
  { label: 'SSH', value: 'ssh' },
  { label: 'TCP (tcp)', value: 'tcp' }
]

const form = reactive({
  name: '',
  entry_node_id: '',
  hops: [{ node_id: '', protocol: 'ws', relay_port: 8443 }],
  remark: ''
})

const validateHops = (rule, value, callback) => {
  if (!form.hops.length) {
    callback(new Error('请至少添加一个跳点'))
    return
  }
  const used = new Set([form.entry_node_id])
  for (const hop of form.hops) {
    if (!hop.node_id) {
      callback(new Error('请选择跳点节点'))
      return
    }
    if (used.has(hop.node_id)) {
      callback(new Error('链路节点不能重复'))
      return
    }
    used.add(hop.node_id)
    if (!hop.protocol || !hop.relay_port) {
      callback(new Error('请填写跳点协议和端口'))
      return
    }
  }
  callback()
}

const formRules = {
  name: [{ required: true, message: '请输入隧道名称', trigger: 'blur' }],
  entry_node_id: [{ required: true, message: '请选择入口节点', trigger: 'change' }],
  hops: [{ validator: validateHops, trigger: 'change' }]
}

const getStatusType = (status) => {
  const map = { running: 'success', stopped: 'info', error: 'danger' }
  return map[status] || 'info'
}

const getStatusText = (status) => {
  const map = { running: '运行中', stopped: '已停止', error: '错误' }
  return map[status] || status
}

const getNodeName = (id) => {
  return nodeList.value.find(node => node.id === id)?.name || '-'
}

const getTunnelHops = (tunnel) => {
  if (tunnel.hops && tunnel.hops.length) return tunnel.hops
  if (tunnel.exit_node_id) {
    return [{ node_id: tunnel.exit_node_id, protocol: tunnel.protocol || 'ws', relay_port: tunnel.relay_port || 8443 }]
  }
  return []
}

const getTunnelPath = (tunnel) => {
  const names = [tunnel.entry_node?.name || getNodeName(tunnel.entry_node_id)]
  for (const hop of getTunnelHops(tunnel)) {
    names.push(getNodeName(hop.node_id))
  }
  return names.join(' -> ')
}

const isNodeUsedInHops = (nodeId) => {
  return form.hops.some(hop => hop.node_id === nodeId)
}

const isHopNodeDisabled = (nodeId, index) => {
  if (nodeId === form.entry_node_id) return true
  return form.hops.some((hop, hopIndex) => hopIndex !== index && hop.node_id === nodeId)
}

const fetchNodes = async () => {
  try {
    const res = await getNodeList({ pageSize: 100 })
    nodeList.value = res.data.list || []
  } catch (error) {
    console.error('获取节点列表失败:', error)
  }
}

const fetchData = async (isSilent = false) => {
  if (!isSilent) loading.value = true
  try {
    const res = await getTunnelList({
      page: page.value,
      pageSize: pageSize.value,
      node_id: searchNodeId.value,
      status: searchStatus.value,
      keyword: searchKeyword.value
    })
    tunnelList.value = res.data.list || []
    total.value = res.data.total || 0
  } catch (error) {
    console.error('获取隧道列表失败:', error)
  } finally {
    if (!isSilent) loading.value = false
  }
}

const handleSearch = () => {
  page.value = 1
  fetchData()
}

const toFormHops = (row) => {
  const hops = getTunnelHops(row)
  if (!hops.length) return [{ node_id: '', protocol: 'ws', relay_port: 8443 }]
  return hops.map(hop => ({
    node_id: hop.node_id,
    protocol: hop.protocol || row.protocol || 'ws',
    relay_port: hop.relay_port || row.relay_port || 8443
  }))
}

const openDialog = (row = null) => {
  if (row?.status === 'running') {
    ElMessage.warning('请先停止隧道再编辑')
    return
  }

  isEdit.value = !!row
  editId.value = row?.id || null

  if (row) {
    Object.assign(form, {
      name: row.name,
      entry_node_id: row.entry_node_id,
      hops: toFormHops(row),
      remark: row.remark || ''
    })
  } else {
    Object.assign(form, {
      name: '',
      entry_node_id: '',
      hops: [{ node_id: '', protocol: 'ws', relay_port: 8443 }],
      remark: ''
    })
  }

  dialogVisible.value = true
}

const addHop = () => {
  form.hops.push({ node_id: '', protocol: 'ws', relay_port: 8443 })
}

const removeHop = (index) => {
  form.hops.splice(index, 1)
}

const moveHop = (index, direction) => {
  const target = index + direction
  if (target < 0 || target >= form.hops.length) return
  const item = form.hops.splice(index, 1)[0]
  form.hops.splice(target, 0, item)
}

const handleSubmit = async () => {
  if (!formRef.value) return

  await formRef.value.validate(async (valid) => {
    if (!valid) return

    submitLoading.value = true
    try {
      const hops = form.hops.map(hop => ({
        node_id: hop.node_id,
        protocol: hop.protocol,
        relay_port: hop.relay_port
      }))
      const lastHop = hops[hops.length - 1]
      const submitData = {
        name: form.name,
        entry_node_id: form.entry_node_id,
        exit_node_id: lastHop.node_id,
        protocol: lastHop.protocol,
        relay_port: lastHop.relay_port,
        hops,
        remark: form.remark
      }

      if (isEdit.value) {
        await updateTunnel(editId.value, submitData)
        ElMessage.success('更新成功')
      } else {
        await createTunnel(submitData)
        ElMessage.success('创建成功')
      }
      dialogVisible.value = false
      fetchData()
    } catch (error) {
      console.error('操作失败:', error)
    } finally {
      submitLoading.value = false
    }
  })
}

const handleDelete = async (row) => {
  try {
    await ElMessageBox.confirm(`确定要删除隧道 "${row.name}" 吗？如果有规则正在使用此隧道，将无法删除。`, '提示', {
      confirmButtonText: '确定',
      cancelButtonText: '取消',
      type: 'warning'
    })
    await deleteTunnel(row.id)
    ElMessage.success('删除成功')
    fetchData()
  } catch (error) {
    if (error !== 'cancel') {
      console.error('删除失败:', error)
    }
  }
}

const handleStart = async (row) => {
  try {
    await startTunnel(row.id)
    ElMessage.success('启动成功')
    fetchData()
  } catch (error) {
    console.error('启动失败:', error)
  }
}

const handleStop = async (row) => {
  try {
    await stopTunnel(row.id)
    ElMessage.success('停止成功')
    fetchData()
  } catch (error) {
    console.error('停止失败:', error)
  }
}

let refreshTimer = null

onMounted(() => {
  fetchNodes()
  fetchData()
  refreshTimer = setInterval(() => {
    fetchData(true)
  }, 5000)
})

onBeforeUnmount(() => {
  if (refreshTimer) {
    clearInterval(refreshTimer)
  }
})
</script>

<style scoped>
.page-container {
  display: flex;
  flex-direction: column;
  gap: 20px;
}

.page-header h3 {
  margin: 0 0 16px 0;
  font-size: 18px;
  font-weight: 600;
  color: #303133;
}

.search-bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: 20px;
}

.filters {
  display: flex;
  gap: 12px;
}

.pagination {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}

.form-hint {
  color: #909399;
  font-size: 12px;
  margin-top: 4px;
}

.hop-actions {
  width: 100%;
  margin-top: 10px;
  text-align: center;
  border: 1px dashed #dcdfe6;
  border-radius: 4px;
}

:deep(.el-table .el-table__cell) {
  padding: 12px 0;
}
</style>
