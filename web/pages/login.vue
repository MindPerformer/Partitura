<!-- pages/login.vue — 登录页面
//
// 引入动机：design/04-WEB-API.md §页面 要求 Login 页面。
// 调用 POST /api/auth/login，成功后存储用户信息并重定向。
//
// Phase 6 i18n：所有用户可见文案使用 t() 翻译。
-->
<script setup lang="ts">
import type { ApiError } from '~/types/api'

definePageMeta({
  layout: 'auth',
  middleware: [] // login 页面不需要 auth middleware
})

const { t } = useI18n()
const { setAuth, isAuthenticated } = useAuth()
const route = useRoute()
const router = useRouter()

const username = ref('')
const password = ref('')
const loading = ref(false)
const errorMsg = ref<string | null>(null)

// 已登录用户访问 login 页面时重定向
onMounted(() => {
  if (isAuthenticated.value) {
    const redirect = (route.query.redirect as string) || '/'
    router.push(redirect)
  }
})

// 设置页面标题
useHead({ title: () => t('auth.signIn') + ' · ' + t('common.appName') })

async function handleLogin() {
  if (!username.value || !password.value) {
    errorMsg.value = t('auth.usernamePasswordRequired')
    return
  }

  loading.value = true
  errorMsg.value = null

  try {
    const api = useAuthApi()
    const result = await api.login(username.value, password.value)
    setAuth(result)

    const redirect = (route.query.redirect as string) || '/'
    router.push(redirect)
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 401) {
      errorMsg.value = t('auth.invalidCredentials')
    } else if (apiErr.status === 400) {
      errorMsg.value = t('auth.badRequest')
    } else {
      errorMsg.value = apiErr.error || t('auth.loginFailed')
    }
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="w-full max-w-sm">
    <div class="text-center mb-8">
      <UIcon name="i-lucide-book-open" class="w-12 h-12 text-primary mx-auto mb-3" />
      <h1 class="text-2xl font-bold text-highlighted">{{ t('common.appName') }}</h1>
      <p class="text-sm text-muted mt-2">{{ t('auth.signInToAccount') }}</p>
    </div>

    <UCard>
      <form @submit.prevent="handleLogin" class="space-y-4">
        <UFormField :label="t('auth.username')" name="username">
          <UInput
            v-model="username"
            :placeholder="t('auth.enterUsername')"
            autocomplete="username"
            autofocus
            class="w-full"
          />
        </UFormField>

        <UFormField :label="t('auth.password')" name="password">
          <UInput
            v-model="password"
            type="password"
            :placeholder="t('auth.enterPassword')"
            autocomplete="current-password"
            class="w-full"
          />
        </UFormField>

        <ErrorDisplay v-if="errorMsg" :message="errorMsg" />

        <UButton
          type="submit"
          block
          :loading="loading"
          :disabled="loading"
        >
          {{ t('auth.signIn') }}
        </UButton>
      </form>
    </UCard>
  </div>
</template>
