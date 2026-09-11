<!-- pages/device-authorize.vue — Device Authorization 授权页面
//
// 引入动机：design/02-MCP.md §登录 要求 browser 批准设备授权流程。
// MCP 客户端生成非敏感 device_name，server 生成 device_code/user_code，
// 用户在此页面批准或拒绝授权请求，MCP 通过 poll 一次性获取凭据。
//
// 安全约束：
// - 只读展示授权码，不输出/打印 token、device_code 或用户身份。
// - 批准/拒绝通过 POST /api/auth/device/{approve,deny}，带 CSRF。
// - 未登录时由 auth middleware 安全站内重定向到 /login 并回跳。
// - 状态为非 pending 时显示对应终端状态，不重复发起动作。
-->
<script setup lang="ts">
import type { DeviceAuthInfoResponse } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isAuthenticated } = useAuth()
const route = useRoute()
const router = useRouter()

const code = computed(() => String(route.query.code || ''))
const loading = ref(false)
const error = ref<string | null>(null)
const info = ref<DeviceAuthInfoResponse | null>(null)
const actionLoading = ref(false)

const statusLabel = computed(() => {
  if (!info.value) return ''
  const map: Record<string, string> = {
    pending: t('device.pending'),
    authorized: t('device.authorized'),
    completed: t('device.completed'),
    denied: t('device.denied'),
    expired: t('device.expired')
  }
  return map[info.value.status] || info.value.status
})

const isPending = computed(() => info.value?.status === 'pending')

async function loadInfo() {
  if (!code.value) {
    error.value = t('device.missingCode')
    return
  }
  loading.value = true
  error.value = null
  try {
    const api = useAuthApi()
    info.value = await api.deviceInfo(code.value)
  } catch (err) {
    const apiErr = err as { status?: number; error?: string }
    if (apiErr.status === 404) {
      error.value = t('device.codeNotFound')
    } else if (apiErr.status === 401) {
      // 未认证时 middleware 已处理，此处兜底
      error.value = t('auth.loginFailed')
    } else {
      error.value = apiErr.error || t('device.loadInfoFailed')
    }
  } finally {
    loading.value = false
  }
}

async function handleApprove() {
  if (!code.value || actionLoading.value) return
  actionLoading.value = true
  error.value = null
  try {
    const api = useAuthApi()
    await api.deviceApprove(code.value)
    info.value = { status: 'authorized', device_name: info.value?.device_name || '', expires_in: info.value?.expires_in || 0 }
  } catch (err) {
    const apiErr = err as { status?: number; error?: string }
    error.value = apiErr.error || t('device.approveFailed')
  } finally {
    actionLoading.value = false
  }
}

async function handleDeny() {
  if (!code.value || actionLoading.value) return
  actionLoading.value = true
  error.value = null
  try {
    const api = useAuthApi()
    await api.deviceDeny(code.value)
    info.value = { status: 'denied', device_name: info.value?.device_name || '', expires_in: info.value?.expires_in || 0 }
  } catch (err) {
    const apiErr = err as { status?: number; error?: string }
    error.value = apiErr.error || t('device.denyFailed')
  } finally {
    actionLoading.value = false
  }
}

onMounted(() => {
  if (!isAuthenticated.value) {
    router.push({
      path: '/login',
      query: { redirect: route.fullPath }
    })
    return
  }
  loadInfo()
})

useHead({ title: () => t('device.authorize') + ' · ' + t('common.appName') })
</script>

<template>
  <!-- 顶栏由 layouts/default.vue 统一注入；内容区保留居中卡片布局 -->
  <div class="flex items-center justify-center px-4 py-8 min-h-[calc(100vh-3.5rem)]">
    <UCard class="w-full max-w-md">
      <template #header>
        <div class="flex items-center gap-3">
          <UIcon name="i-lucide-shield-check" class="w-6 h-6 text-primary" />
          <h1 class="text-lg font-semibold text-highlighted">{{ t('device.authorize') }}</h1>
        </div>
      </template>

      <div class="space-y-4">
        <ErrorDisplay v-if="error" :message="error" />

        <div v-if="loading" class="flex justify-center py-6">
          <UIcon name="i-lucide-loader-circle" class="w-6 h-6 animate-spin text-muted" />
        </div>

        <template v-else-if="info">
          <div class="space-y-2">
            <div>
              <p class="text-xs text-muted mb-1">{{ t('device.code') }}</p>
              <p class="font-mono text-sm bg-muted/50 rounded px-2 py-1 break-all">{{ code }}</p>
            </div>
            <div>
              <p class="text-xs text-muted mb-1">{{ t('device.deviceName') }}</p>
              <p class="text-sm">{{ info.device_name }}</p>
            </div>
            <div>
              <p class="text-xs text-muted mb-1">{{ t('device.status') }}</p>
              <UBadge :color="info.status === 'pending' ? 'warning' : info.status === 'authorized' || info.status === 'completed' ? 'success' : 'error'" variant="subtle" size="sm">
                {{ statusLabel }}
              </UBadge>
            </div>
            <div v-if="info.status === 'pending'">
              <p class="text-xs text-muted mb-1">{{ t('device.expiresIn') }}</p>
              <p class="text-sm">{{ info.expires_in }} {{ t('common.seconds') }}</p>
            </div>
          </div>

          <div v-if="isPending" class="flex gap-3 pt-2">
            <UButton color="success" class="flex-1" :loading="actionLoading" @click="handleApprove">
              {{ t('common.approve') }}
            </UButton>
            <UButton color="error" variant="outline" class="flex-1" :loading="actionLoading" @click="handleDeny">
              {{ t('common.deny') }}
            </UButton>
          </div>

          <div v-else class="pt-2 text-sm text-muted">
            <p v-if="info.status === 'authorized' || info.status === 'completed'">{{ t('device.approvedDesc') }}</p>
            <p v-else-if="info.status === 'denied'">{{ t('device.deniedDesc') }}</p>
            <p v-else-if="info.status === 'expired'">{{ t('device.expiredDesc') }}</p>
          </div>
        </template>

        <div v-else-if="!error" class="text-sm text-muted text-center py-6">
          {{ t('common.noData') }}
        </div>

        <UButton color="neutral" variant="ghost" class="w-full" @click="loadInfo">
          {{ t('common.retry') }}
        </UButton>
      </div>
    </UCard>
  </div>
</template>
