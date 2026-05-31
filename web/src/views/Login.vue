<template>
  <div class="login-container">
    <div class="login-box">
      <div class="login-header">
        <h1>{{ systemStore.siteTitle }}</h1>
        <p>转发面板</p>
      </div>
      <el-form
        ref="loginFormRef"
        :model="loginForm"
        :rules="loginRules"
        class="login-form"
        @keyup.enter="handleLogin"
      >
        <el-form-item prop="username">
          <el-input
            v-model="loginForm.username"
            placeholder="用户名"
            :prefix-icon="User"
            size="large"
          />
        </el-form-item>
        <el-form-item prop="password">
          <el-input
            v-model="loginForm.password"
            type="password"
            placeholder="密码"
            :prefix-icon="Lock"
            size="large"
            show-password
          />
        </el-form-item>
        <el-form-item>
          <div v-if="systemStore.turnstileEnabled && systemStore.turnstileSiteKey" ref="turnstileRef" class="turnstile-box"></div>
        </el-form-item>
        <el-form-item>
          <el-button
            type="primary"
            size="large"
            :loading="loading"
            class="login-button"
            @click="handleLogin"
          >
            登录
          </el-button>
        </el-form-item>
      </el-form>
    </div>
  </div>
</template>

<script setup>
import { ref, reactive, onMounted, nextTick } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { User, Lock } from '@element-plus/icons-vue'
import { useAuthStore } from '@/store/auth'
import { useSystemStore } from '@/store/system'

const router = useRouter()
const route = useRoute()
const authStore = useAuthStore()
const systemStore = useSystemStore()

const loginFormRef = ref(null)
const loading = ref(false)
const turnstileRef = ref(null)
const turnstileWidgetId = ref(null)

const loginForm = reactive({
  username: '',
  password: '',
  turnstile_token: ''
})

const loginRules = {
  username: [
    { required: true, message: '请输入用户名', trigger: 'blur' },
    { min: 3, max: 50, message: '用户名长度为 3-50 个字符', trigger: 'blur' }
  ],
  password: [
    { required: true, message: '请输入密码', trigger: 'blur' },
    { min: 6, max: 50, message: '密码长度为 6-50 个字符', trigger: 'blur' }
  ]
}

const handleLogin = async () => {
  if (!loginFormRef.value) return
  
  await loginFormRef.value.validate(async (valid) => {
    if (!valid) return

    if (systemStore.turnstileEnabled && !loginForm.turnstile_token) {
      ElMessage.warning('请先完成人机验证')
      return
    }
    
    loading.value = true
    try {
      await authStore.login(loginForm)
      ElMessage.success('登录成功')
      // 跳转到之前的页面或仪表盘
      const redirect = route.query.redirect || '/dashboard'
      router.push(redirect)
    } catch (error) {
      console.error('登录失败:', error)
      resetTurnstile()
    } finally {
      loading.value = false
    }
  })
}

const loadTurnstileScript = () => {
  return new Promise((resolve, reject) => {
    if (window.turnstile) {
      resolve()
      return
    }

    const existingScript = document.querySelector('script[data-turnstile="true"]')
    if (existingScript) {
      existingScript.addEventListener('load', () => resolve(), { once: true })
      existingScript.addEventListener('error', () => reject(new Error('Turnstile script load failed')), { once: true })
      return
    }

    const script = document.createElement('script')
    script.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'
    script.async = true
    script.defer = true
    script.dataset.turnstile = 'true'
    script.onload = () => resolve()
    script.onerror = () => reject(new Error('Turnstile script load failed'))
    document.head.appendChild(script)
  })
}

const resetTurnstile = () => {
  loginForm.turnstile_token = ''
  if (window.turnstile && turnstileWidgetId.value !== null) {
    try {
      window.turnstile.reset(turnstileWidgetId.value)
    } catch (error) {
      console.warn('重置 Turnstile 失败:', error)
    }
  }
}

const renderTurnstile = async () => {
  if (!systemStore.turnstileEnabled || !systemStore.turnstileSiteKey) return

  await loadTurnstileScript()
  await nextTick()

  if (!turnstileRef.value || !window.turnstile) return

  turnstileRef.value.innerHTML = ''
  turnstileWidgetId.value = window.turnstile.render(turnstileRef.value, {
    sitekey: systemStore.turnstileSiteKey,
    callback: (token) => {
      loginForm.turnstile_token = token
    },
    'expired-callback': () => {
      loginForm.turnstile_token = ''
    },
    'error-callback': () => {
      loginForm.turnstile_token = ''
    }
  })
}

onMounted(() => {
  systemStore.fetchSystemConfig().then(() => {
    renderTurnstile()
  })
})
</script>

<style scoped>
.login-container {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
}

.login-box {
  width: 400px;
  padding: 40px;
  background: rgba(255, 255, 255, 0.95);
  border-radius: 16px;
  box-shadow: 0 20px 60px rgba(0, 0, 0, 0.3);
  backdrop-filter: blur(10px);
}

.login-header {
  text-align: center;
  margin-bottom: 30px;
}

.login-header h1 {
  font-size: 28px;
  color: #303133;
  margin: 0 0 8px 0;
  font-weight: 700;
}

.login-header p {
  color: #909399;
  font-size: 14px;
  margin: 0;
}

.login-form {
  margin-top: 20px;
}

.turnstile-box {
  width: 100%;
  min-height: 65px;
  display: flex;
  justify-content: center;
}

.login-button {
  width: 100%;

  font-size: 16px;
  border-radius: 8px;
  margin-top: 10px;
}

:deep(.el-input__wrapper) {
  border-radius: 8px;
  padding: 0 15px;
}


</style>
