<template>
  <div class="system-container">
    <el-card class="box-card">
      <template #header>
        <div class="card-header">
          <span>系统设置</span>
        </div>
      </template>

      <el-tabs v-model="activeTab" class="demo-tabs">
        <!-- 面板配置 -->
        <el-tab-pane label="面板配置" name="config">
          <el-form ref="configFormRef" :model="configForm" label-width="120px" class="setting-form">
            <el-form-item label="面板地址" prop="panelUrl">
               <el-input v-model="configForm.panelUrl" placeholder="你当前的面板地址" />
            </el-form-item>
             <el-form-item label="站点标题" prop="siteTitle">
              <el-input v-model="configForm.siteTitle" placeholder="Gost Panel" />
            </el-form-item>

            <el-form-item label="Logo地址" prop="logoUrl">
              <el-input v-model="configForm.logoUrl" placeholder="请输入Logo URL" />
            </el-form-item>
            <el-form-item label="版权信息" prop="copyright">
              <el-input v-model="configForm.copyright" placeholder="请输入版权信息" />
            </el-form-item>
            <el-form-item>
              <el-button type="primary" :loading="loading" @click="handleSave('config')">保存设置</el-button>
            </el-form-item>
          </el-form>
        </el-tab-pane>

        <!-- 登录防护 -->
        <el-tab-pane label="登录防护" name="login">
          <el-form ref="loginFormRef" :model="loginForm" label-width="120px" class="setting-form">
            <el-form-item label="启用 Turnstile" prop="turnstileEnabled">
              <el-switch v-model="loginForm.turnstileEnabled" />
            </el-form-item>
            <el-form-item label="Site Key" prop="turnstileSiteKey">
              <el-input v-model="loginForm.turnstileSiteKey" placeholder="请输入 Cloudflare Turnstile Site Key" />
            </el-form-item>
            <el-form-item label="Secret Key" prop="turnstileSecretKey">
              <!-- 服务端只回传占位符表示"已设置"，原样提交即保持不变；填新值才会覆盖 -->
              <el-input
                v-model="loginForm.turnstileSecretKey"
                type="password"
                show-password
                :placeholder="secretIsSet('turnstileSecretKey') ? '已设置，留空表示不修改' : '请输入 Cloudflare Turnstile Secret Key'"
              />
            </el-form-item>
            <el-form-item>
              <el-button type="primary" :loading="loading" @click="handleSave('login')">保存设置</el-button>
            </el-form-item>
          </el-form>
        </el-tab-pane>

        <!-- 邮箱配置 -->
        <el-tab-pane label="邮箱配置" name="email">
          <el-form ref="emailFormRef" :model="emailForm" label-width="120px" class="setting-form">
            <el-form-item label="SMTP服务器" prop="host">
              <el-input v-model="emailForm.host" placeholder="例如: smtp.gmail.com" />
            </el-form-item>
            <el-form-item label="SMTP端口" prop="port">
              <el-input v-model.number="emailForm.port" placeholder="例如: 465 或 587" />
            </el-form-item>
            <el-form-item label="用户名" prop="username">
              <el-input v-model="emailForm.username" placeholder="请输入SMTP用户名" />
            </el-form-item>
            <el-form-item label="密码" prop="password">
              <el-input
                v-model="emailForm.password"
                type="password"
                show-password
                :placeholder="secretIsSet('smtpPassword') ? '已设置，留空表示不修改' : '请输入SMTP密码'"
              />
            </el-form-item>
            <el-form-item label="发件人邮箱" prop="fromEmail">
              <el-input v-model="emailForm.fromEmail" placeholder="例如: noreply@example.com" />
            </el-form-item>
            <el-form-item>
              <el-button type="primary" :loading="loading" @click="handleSave('email')">保存设置</el-button>
              <el-button @click="handleTestEmail">测试邮件</el-button>
            </el-form-item>
          </el-form>
        </el-tab-pane>



        <!-- 备份 -->
        <el-tab-pane label="备份" name="backup">
          <el-form ref="backupFormRef" :model="backupForm" label-width="120px" class="setting-form">
            <el-form-item label="自动备份" prop="autoBackup">
              <el-switch v-model="backupForm.autoBackup" />
            </el-form-item>
            <el-form-item label="保留份数" prop="retentionCount">
              <el-input-number v-model="backupForm.retentionCount" :min="1" :max="100" />
            </el-form-item>
            <el-form-item>
              <el-button type="primary" :loading="loading" @click="handleSave('backup')">保存设置</el-button>
              <el-divider direction="vertical" />
              <el-button type="success" @click="handleBackupNow">立即备份</el-button>
            </el-form-item>
          </el-form>
        </el-tab-pane>

        <!-- 版本与更新 -->
        <el-tab-pane label="版本与更新" name="update">
          <div class="update-pane" v-loading="updateLoading">
            <el-descriptions :column="2" border size="small" class="update-desc">
              <el-descriptions-item label="当前版本">
                {{ updateInfo.current_version || '未知' }}
              </el-descriptions-item>
              <el-descriptions-item label="最新版本">
                <span v-if="updateInfo.latest_version">{{ updateInfo.latest_version }}</span>
                <span v-else>—</span>
                <el-tag v-if="updateInfo.has_update" type="warning" size="small" style="margin-left: 8px">
                  有新版本
                </el-tag>
                <!-- 只有版本可比较时才敢说"已是最新"。
                     dev 构建的版本号无法与发布版本比较，此时标"已是最新"是误导。 -->
                <el-tag
                  v-else-if="updateInfo.updatable && updateInfo.latest_version"
                  type="success"
                  size="small"
                  style="margin-left: 8px"
                >
                  已是最新
                </el-tag>
                <el-tag v-else-if="updateInfo.latest_version" type="info" size="small" style="margin-left: 8px">
                  无法比较
                </el-tag>
              </el-descriptions-item>
            </el-descriptions>

            <!-- 环境不支持时说明原因，而不是让按钮点了报错 -->
            <el-alert
              v-if="updateInfo.reason"
              type="info"
              :closable="false"
              show-icon
              class="update-alert"
              :title="updateInfo.reason"
            />
            <el-alert
              v-if="updateInfo.warning"
              type="warning"
              :closable="false"
              show-icon
              class="update-alert"
              :title="updateInfo.warning"
            />

            <div class="update-actions">
              <el-button :loading="updateLoading" @click="handleCheckUpdate(true)">检查更新</el-button>
              <el-button
                type="primary"
                :disabled="!canUpdate"
                :loading="updating"
                @click="handleUpdate"
              >升级到 {{ updateInfo.latest_version || '最新版' }}</el-button>
              <el-button
                v-if="updateInfo.can_rollback"
                type="warning"
                plain
                :loading="updating"
                @click="handleRollbackBackup"
              >回滚上一版本</el-button>
              <el-button
                v-if="updateInfo.updatable"
                plain
                @click="handleOpenRollbackDialog"
              >回滚到指定版本</el-button>
            </div>

            <!-- 更新日志：刻意用纯文本渲染。
                 release body 是 Markdown，引入渲染器等于把外部内容变成 HTML，
                 既要放宽 CSP 又多一处 XSS 面，收益不成比例。 -->
            <div v-if="updateInfo.release && updateInfo.release.body" class="release-notes">
              <div class="release-notes-title">
                更新日志（{{ updateInfo.release.version }}）
                <a v-if="updateInfo.release.html_url" :href="updateInfo.release.html_url" target="_blank" rel="noopener noreferrer">
                  在 GitHub 查看
                </a>
              </div>
              <pre class="release-notes-body">{{ updateInfo.release.body }}</pre>
            </div>

            <el-alert
              v-if="needRestart"
              type="success"
              :closable="false"
              show-icon
              class="update-alert"
              title="更新已写入，需要重启面板才能生效"
            >
              <template #default>
                <div style="margin-top: 8px">
                  <el-button
                    v-if="restartSupported"
                    type="primary"
                    size="small"
                    :loading="restarting"
                    @click="handleRestart"
                  >立即重启</el-button>
                  <span v-else>当前平台不支持自动重启，请手动重启面板服务。</span>
                </div>
              </template>
            </el-alert>
          </div>
        </el-tab-pane>
      </el-tabs>
    </el-card>

    <!-- 回滚到指定版本 -->
    <el-dialog v-model="rollbackVisible" title="回滚到指定版本" width="520px">
      <el-alert
        type="warning"
        :closable="false"
        show-icon
        style="margin-bottom: 16px"
        title="回滚到旧版本可能与当前数据库结构不兼容，建议先执行一次备份。"
      />
      <el-table
        v-loading="rollbackLoading"
        :data="rollbackVersions"
        highlight-current-row
        @current-change="(row) => (selectedRollback = row)"
        size="small"
        border
      >
        <el-table-column prop="version" label="版本" width="140" />
        <el-table-column prop="published_at" label="发布时间">
          <template #default="{ row }">{{ formatTime(row.published_at) }}</template>
        </el-table-column>
      </el-table>
      <template #footer>
        <el-button @click="rollbackVisible = false">取消</el-button>
        <el-button
          type="warning"
          :disabled="!selectedRollback"
          :loading="updating"
          @click="handleRollbackVersion"
        >回滚到 {{ selectedRollback ? selectedRollback.version : '…' }}</el-button>
      </template>
    </el-dialog>

    <!-- 测试邮件弹窗 -->
    <el-dialog v-model="testEmailVisible" title="发送测试邮件" width="400px">
      <el-form :model="testEmailForm" label-width="80px">
        <el-form-item label="收件人">
          <el-input v-model="testEmailForm.toEmail" placeholder="请输入接收测试邮件的邮箱" />
        </el-form-item>
      </el-form>
      <template #footer>
        <span class="dialog-footer">
          <el-button @click="testEmailVisible = false">取消</el-button>
          <el-button type="primary" :loading="testEmailLoading" @click="confirmTestEmail">
            发送
          </el-button>
        </span>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { ref, reactive, computed, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
    getSystemConfig, updateSystemConfig, sendTestEmail, backupSystem,
    checkUpdate, performUpdate, getRollbackVersions, rollback, restartPanel
} from '@/api/system'

// 服务端用该占位符表示"此密钥已设置但不回显真实值"。
// 前端在载入时把它从表单值里剥离（否则用户点"显示密码"会看到这串内部标记），
// 只保留一个"是否已设置"的标志用于提示文案。
// 提交时留空 = 保持不变，填入新值才会覆盖 —— 这既避免把 SMTP 密码 /
// Turnstile Secret 明文下发到浏览器，也修掉了"保存任意设置就清空 Secret"的问题。
const SECRET_PLACEHOLDER = '__GOSTPANEL_UNCHANGED__'

const secretsSet = reactive({ smtpPassword: false, turnstileSecretKey: false })
const secretIsSet = (key) => secretsSet[key]

// 把响应中的占位符转换为「空值 + 已设置标志」
const stripSecretPlaceholder = (form, field, flagKey) => {
    if (form[field] === SECRET_PLACEHOLDER) {
        secretsSet[flagKey] = true
        form[field] = ''
    } else {
        secretsSet[flagKey] = false
    }
}

const activeTab = ref('config')
const loading = ref(false)
const testEmailVisible = ref(false)
const testEmailLoading = ref(false)
const testEmailForm = reactive({
    toEmail: ''
})


const emailForm = reactive({
  host: '',
  port: 465,
  username: '',
  password: '',
  fromEmail: ''
})

const loginForm = reactive({
  turnstileEnabled: false,
  turnstileSiteKey: '',
  turnstileSecretKey: ''
})

const configForm = reactive({
  panelUrl: '',
  siteTitle: 'Gost Panel',
  logoUrl: '',
  copyright: ''
})



const backupForm = reactive({
  autoBackup: false,
  retentionCount: 7
})

// ==================== 版本与在线更新 ====================

const updateInfo = ref({})
const updateLoading = ref(false)
const updating = ref(false)
const restarting = ref(false)
const needRestart = ref(false)
const restartSupported = ref(false)

const rollbackVisible = ref(false)
const rollbackLoading = ref(false)
const rollbackVersions = ref([])
const selectedRollback = ref(null)

// 三个条件同时满足才允许点升级：环境支持、确有新版本、当前没有更新在跑
const canUpdate = computed(() =>
    !!updateInfo.value.updatable && !!updateInfo.value.has_update && !updateInfo.value.in_progress
)

const formatTime = (value) => {
    if (!value) return '—'
    const d = new Date(value)
    return isNaN(d.getTime()) ? value : d.toLocaleString()
}

const handleCheckUpdate = async (force = false) => {
    updateLoading.value = true
    try {
        const res = await checkUpdate(force)
        updateInfo.value = res.data || {}
        restartSupported.value = !!updateInfo.value.restart_supported || restartSupported.value
        if (force) {
            ElMessage.success(updateInfo.value.has_update ? '发现新版本' : '当前已是最新版本')
        }
    } catch (error) {
        console.error('检查更新失败:', error)
    } finally {
        updateLoading.value = false
    }
}

const handleUpdate = async () => {
    try {
        await ElMessageBox.confirm(
            `将从 ${updateInfo.value.current_version} 升级到 ${updateInfo.value.latest_version}。` +
            '下载与校验可能需要几分钟，完成后需要重启面板生效。',
            '确认升级',
            { confirmButtonText: '开始升级', cancelButtonText: '取消', type: 'warning' }
        )
    } catch {
        return
    }

    updating.value = true
    try {
        const res = await performUpdate()
        needRestart.value = !!res.data?.need_restart
        restartSupported.value = !!res.data?.restart_supported
        ElMessage.success(res.message || '升级完成')
        await handleCheckUpdate(false)
    } catch (error) {
        console.error('升级失败:', error)
    } finally {
        updating.value = false
    }
}

const handleRollbackBackup = async () => {
    try {
        await ElMessageBox.confirm(
            '将恢复上一次更新前保留的二进制备份，完成后需要重启面板生效。',
            '确认回滚',
            { confirmButtonText: '回滚', cancelButtonText: '取消', type: 'warning' }
        )
    } catch {
        return
    }

    updating.value = true
    try {
        const res = await rollback()
        needRestart.value = true
        restartSupported.value = !!res.data?.restart_supported
        ElMessage.success(res.message || '回滚完成')
        await handleCheckUpdate(false)
    } catch (error) {
        console.error('回滚失败:', error)
    } finally {
        updating.value = false
    }
}

const handleOpenRollbackDialog = async () => {
    rollbackVisible.value = true
    selectedRollback.value = null
    rollbackLoading.value = true
    try {
        const res = await getRollbackVersions()
        rollbackVersions.value = res.data || []
        if (!rollbackVersions.value.length) {
            ElMessage.info('没有可回滚的历史版本')
        }
    } catch (error) {
        console.error('获取历史版本失败:', error)
    } finally {
        rollbackLoading.value = false
    }
}

const handleRollbackVersion = async () => {
    if (!selectedRollback.value) return

    updating.value = true
    try {
        const res = await rollback(selectedRollback.value.version)
        needRestart.value = true
        restartSupported.value = !!res.data?.restart_supported
        rollbackVisible.value = false
        ElMessage.success(res.message || '回滚完成')
        await handleCheckUpdate(false)
    } catch (error) {
        console.error('回滚失败:', error)
    } finally {
        updating.value = false
    }
}

const handleRestart = async () => {
    restarting.value = true
    try {
        await restartPanel()
        ElMessage.success('面板正在重启，请稍候…')
        // 轮询健康检查，恢复后刷新页面拿到新版本的前端资源
        await waitForPanelBack()
    } catch (error) {
        console.error('重启失败:', error)
        restarting.value = false
    }
}

// 轮询直到面板重新可用。进程会先退出再由 systemd 拉起，
// 期间请求必然失败，这里只关心"什么时候重新变得可用"。
const waitForPanelBack = async () => {
    const deadline = Date.now() + 60_000
    // 先等一下，避免在旧进程尚未退出时就探到"可用"
    await new Promise((r) => setTimeout(r, 2000))

    while (Date.now() < deadline) {
        try {
            const resp = await fetch('/api/v1/health', { cache: 'no-store' })
            if (resp.ok) {
                ElMessage.success('面板已恢复，即将刷新页面')
                setTimeout(() => window.location.reload(), 800)
                return
            }
        } catch {
            // 重启窗口内失败是预期的，继续等
        }
        await new Promise((r) => setTimeout(r, 2000))
    }

    restarting.value = false
    ElMessage.warning('等待面板恢复超时，请手动刷新页面确认状态')
}

// 获取配置
const fetchConfig = async () => {
    loading.value = true
    try {
        const res = await getSystemConfig()
        if (res.data) {
            // 根据后端返回的数据结构填充表单
          const { panel, email, config, login, backup } = res.data
            if (panel) {
                configForm.panelUrl = panel.panelUrl
            }
            if (email) Object.assign(emailForm, email)
            if (config) Object.assign(configForm, config)
            if (login) Object.assign(loginForm, login)
            if (backup) Object.assign(backupForm, backup)

            // 密钥字段只保留"已设置"标志，不把占位符留在输入框里
            stripSecretPlaceholder(emailForm, 'password', 'smtpPassword')
            stripSecretPlaceholder(loginForm, 'turnstileSecretKey', 'turnstileSecretKey')
        }
    } catch (error) {
        console.error('获取系统配置失败:', error)
    } finally {
        loading.value = false
    }
}

// 保存配置
const handleSave = async (type) => {
    loading.value = true
    try {
        let data = {}
        // 根据当前tab类型构建提交数据
        const payload = {
            panel: { panelUrl: configForm.panelUrl },
            email: emailForm,
            config: {
                siteTitle: configForm.siteTitle,
                logoUrl: configForm.logoUrl,
                copyright: configForm.copyright
            },
            login: loginForm,
            backup: backupForm
        }
        
        await updateSystemConfig(payload)
        ElMessage.success('保存成功')
    } catch (error) {
        console.error('保存失败:', error)
    } finally {
        loading.value = false
    }
}

const handleTestEmail = () => {
    // 简单校验
    if (!emailForm.host || !emailForm.port || !emailForm.fromEmail) {
        ElMessage.warning('请先完善邮箱配置信息')
        return
    }
    testEmailForm.toEmail = emailForm.fromEmail
    testEmailVisible.value = true
}

const confirmTestEmail = async () => {
    if (!testEmailForm.toEmail) {
        ElMessage.warning('请输入收件人邮箱')
        return
    }

    testEmailLoading.value = true
    try {
        const payload = {
            ...emailForm,
            toEmail: testEmailForm.toEmail
        }
        await sendTestEmail(payload)
        ElMessage.success('测试邮件发送成功')
        testEmailVisible.value = false
    } catch (error) {
        console.error('测试邮件发送失败:', error)
    } finally {
        testEmailLoading.value = false
    }
}

const handleBackupNow = async () => {
    try {
        await ElMessageBox.confirm('确定要立即执行数据库备份吗？', '提示', {
            confirmButtonText: '确定',
            cancelButtonText: '取消',
            type: 'warning'
        })
        
        loading.value = true
        await backupSystem()
        ElMessage.success('备份成功')
    } catch (error) {
        if (error !== 'cancel') {
            console.error('备份失败:', error)
        }
    } finally {
        loading.value = false
    }
}

onMounted(() => {
    fetchConfig()
    // 走缓存，不强制打 GitHub（未认证 API 每小时仅 60 次）
    handleCheckUpdate(false)
})
</script>

<style scoped>
.update-pane {
    max-width: 900px;
}

.update-desc {
    margin-bottom: 16px;
}

.update-alert {
    margin-bottom: 16px;
}

.update-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    margin-bottom: 16px;
}

.release-notes {
    border: 1px solid var(--el-border-color-lighter);
    border-radius: 4px;
    overflow: hidden;
    margin-bottom: 16px;
}

.release-notes-title {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 8px 12px;
    background: var(--el-fill-color-light);
    font-size: 13px;
    font-weight: 600;
}

.release-notes-title a {
    font-weight: 400;
    font-size: 12px;
    color: var(--el-color-primary);
    text-decoration: none;
}

/* 更新日志按纯文本展示：release body 是外部内容，
   不做 Markdown 渲染以避免引入 XSS 面并放宽 CSP */
.release-notes-body {
    margin: 0;
    padding: 12px;
    max-height: 320px;
    overflow: auto;
    font-size: 12px;
    line-height: 1.6;
    white-space: pre-wrap;
    word-break: break-word;
    font-family: inherit;
    color: var(--el-text-color-regular);
}

.system-container {
  padding: 20px;
}
.setting-form {
    max-width: 600px;
    margin-top: 20px;
}
.ml-2 {
  margin-left: 8px;
}
</style>
