<!-- pages/bootstrap.vue — 首次管理员创建页面
//
// 引入动机：计划要求 Compose 空数据库启动时提供一次性 Bootstrap 页面
// 创建第一个 system_admin。只要存在任意用户，Bootstrap 永久拒绝。
//
// 行为：
// - 页面加载时查询 GET /api/bootstrap 检查是否可用
// - 不可用时显示提示并引导到登录页
// - 可用时显示表单，POST /api/bootstrap 创建首个管理员
// - 创建成功后自动转入登录页面
// - 此页面不需要认证（首次使用时无用户）
//
// 安全原则：
// - 密码输入框 autocomplete="new-password"
// - 创建成功后不自动登录，引导用户手动登录
-->
<script setup lang="ts">
import type { ApiError, BootstrapStatusResponse } from '~/types/api'

definePageMeta({
  layout: 'auth',
  middleware: [] // bootstrap 页面不需要 auth middleware
})

const { t } = useI18n()
const router = useRouter()

const username = ref('')
const email = ref('')
const password = ref('')
const loading = ref(false)
const checking = ref(true)
const bootstrapAvailable = ref(false)
const errorMsg = ref<string | null>(null)
const successMsg = ref<string | null>(null)

useHead({ title: () => t('bootstrap.title') + ' · ' + t('common.appName') })

// 页面加载时检查 Bootstrap 是否可用
onMounted(async () => {
  try {
    const api = useBootstrapApi()
    const resp = await api.getStatus()
    bootstrapAvailable.value = resp.bootstrap_available
  } catch {
    // 查询失败时保守地显示错误，不暴露系统状态
    bootstrapAvailable.value = false
  } finally {
    checking.value = false
  }
})

async function handleCreateAdmin() {
  // 客户端校验
  if (!username.value.trim()) {
    errorMsg.value = t('bootstrap.usernameRequired')
    return
  }
  if (username.value.trim().length < 3 || username.value.trim().length > 100) {
    errorMsg.value = t('bootstrap.usernameLength')
    return
  }
  if (!email.value.trim()) {
    errorMsg.value = t('bootstrap.emailRequired')
    return
  }
  if (!isValidEmail(email.value.trim())) {
    errorMsg.value = t('bootstrap.emailInvalid')
    return
  }
  if (!password.value) {
    errorMsg.value = t('bootstrap.passwordRequired')
    return
  }
  if (password.value.length < 12) {
    errorMsg.value = t('bootstrap.passwordLength')
    return
  }

  loading.value = true
  errorMsg.value = null
  successMsg.value = null

  try {
    const api = useBootstrapApi()
    await api.createAdmin({
      username: username.value.trim(),
      email: email.value.trim(),
      password: password.value
    })

    successMsg.value = t('bootstrap.success')

    // 清空密码输入——安全原则：不保留密码在 DOM
    password.value = ''

    // 延迟跳转到登录页
    setTimeout(() => {
      router.push('/login')
    }, 2000)
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 403) {
      errorMsg.value = t('bootstrap.bootstrapClosed')
    } else if (apiErr.status === 400) {
      errorMsg.value = apiErr.error || t('bootstrap.failed')
    } else {
      errorMsg.value = apiErr.error || t('bootstrap.failed')
    }
  } finally {
    loading.value = false
  }
}

function isValidEmail(email: string): boolean {
  const at = email.indexOf('@')
  if (at <= 0 || at >= email.length - 1) return false
  const dot = email.lastIndexOf('.')
  return dot > at
}
</script>

<template>
  <div class="w-full max-w-sm">
    <div class="text-center mb-8">
      <UIcon name="i-lucide-shield-check" class="w-12 h-12 text-primary mx-auto mb-3" />
      <h1 class="text-2xl font-bold text-highlighted">{{ t('bootstrap.title') }}</h1>
      <p class="text-sm text-muted mt-2">{{ t('bootstrap.subtitle') }}</p>
    </div>

    <!-- 加载中 -->
    <UCard v-if="checking">
      <div class="flex items-center justify-center py-8">
        <UIcon name="i-lucide-loader-2" class="w-6 h-6 animate-spin text-primary" />
        <span class="ml-2 text-muted">{{ t('common.loading') }}</span>
      </div>
    </UCard>

    <!-- Bootstrap 不可用 -->
    <UCard v-else-if="!bootstrapAvailable">
      <div class="text-center py-6 space-y-4">
        <UIcon name="i-lucide-lock" class="w-10 h-10 text-muted mx-auto" />
        <p class="text-sm text-muted">{{ t('bootstrap.bootstrapClosed') }}</p>
        <UButton
          to="/login"
          color="primary"
          variant="outline"
          :label="t('bootstrap.goToLogin')"
        />
      </div>
    </UCard>

    <!-- Bootstrap 表单 -->
    <UCard v-else>
      <p class="text-sm text-muted mb-4">{{ t('bootstrap.description') }}</p>

      <form @submit.prevent="handleCreateAdmin" class="space-y-4">
        <UFormField :label="t('bootstrap.username')" name="username">
          <UInput
            v-model="username"
            :placeholder="t('bootstrap.usernamePlaceholder')"
            autocomplete="username"
            autofocus
            class="w-full"
          />
        </UFormField>

        <UFormField :label="t('bootstrap.email')" name="email">
          <UInput
            v-model="email"
            :placeholder="t('bootstrap.emailPlaceholder')"
            autocomplete="email"
            type="email"
            class="w-full"
          />
        </UFormField>

        <UFormField :label="t('bootstrap.password')" name="password">
          <UInput
            v-model="password"
            :placeholder="t('bootstrap.passwordPlaceholder')"
            autocomplete="new-password"
            type="password"
            class="w-full"
          />
        </UFormField>

        <!-- 错误提示 -->
        <UAlert
          v-if="errorMsg"
          color="error"
          variant="soft"
          :title="errorMsg"
          :icon="'i-lucide-alert-circle'"
        />

        <!-- 成功提示 -->
        <UAlert
          v-if="successMsg"
          color="success"
          variant="soft"
          :title="successMsg"
          :icon="'i-lucide-check-circle'"
        />

        <UButton
          type="submit"
          color="primary"
          block
          :loading="loading"
          :disabled="loading || !!successMsg"
          :label="loading ? t('bootstrap.creating') : t('bootstrap.createAdmin')"
        />
      </form>
    </UCard>
  </div>
</template>
