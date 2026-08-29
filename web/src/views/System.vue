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
      </el-tabs>
    </el-card>

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
import { ref, reactive, onMounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { getSystemConfig, updateSystemConfig, sendTestEmail, backupSystem } from '@/api/system'

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
})
</script>

<style scoped>
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
