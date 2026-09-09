<!-- pages/device-sessions.vue — API / Device Sessions
//
// 引入动机：design/04-WEB-API.md §页面 要求 API / Device Sessions 页面。
// server 提供 device authorization flow（start/approve/deny/poll）和 device/authorize、revoke。
// 但 server 没有 list device sessions 端点，因此只能展示 device authorization flow。
-->
<script setup lang="ts">
import type { ApiError, DeviceAuthStartResponse } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { currentUser } = useAuth()

// Device authorization flow
const deviceName = ref('')
const startResult = ref<DeviceAuthStartResponse | null>(null)
const startLoading = ref(false)
const startError = ref<string | null>(null)

// Approve flow
const userCode = ref('')
const approveLoading = ref(false)
const approveError = ref<string | null>(null)
const approveSuccess = ref(false)

// Revoke flow
const revokeSessionId = ref('')
const revokeLoading = ref(false)
const revokeError = ref<string | null>(null)

async function handleStart() {
  // device_name 可选：为空时服务端会生成安全 fallback。
  startLoading.value = true
  startError.value = null
  try {
    const api = useAuthApi()
    startResult.value = await api.deviceStart(deviceName.value)
  } catch (err) {
    const apiErr = err as ApiError
    startError.value = apiErr.error || t('device.startFailed')
  } finally {
    startLoading.value = false
  }
}

async function handleApprove() {
  if (!userCode.value) {
    approveError.value = t('device.missingCode')
    return
  }
  approveLoading.value = true
  approveError.value = null
  approveSuccess.value = false
  try {
    const api = useAuthApi()
    await api.deviceApprove(userCode.value)
    approveSuccess.value = true
    userCode.value = ''
  } catch (err) {
    const apiErr = err as ApiError
    approveError.value = apiErr.error || t('device.approveFailed')
  } finally {
    approveLoading.value = false
  }
}

async function handleDeny() {
  if (!userCode.value) return
  try {
    const api = useAuthApi()
    await api.deviceDeny(userCode.value)
    userCode.value = ''
  } catch (err) {
    const apiErr = err as ApiError
    approveError.value = apiErr.error || t('device.denyFailed')
  }
}

async function handleRevoke() {
  if (!revokeSessionId.value) {
    revokeError.value = t('device.sessionIdRequired')
    return
  }
  revokeLoading.value = true
  revokeError.value = null
  try {
    const api = useAuthApi()
    await api.revoke(revokeSessionId.value)
    revokeSessionId.value = ''
  } catch (err) {
    const apiErr = err as ApiError
    revokeError.value = apiErr.error || t('device.revokeFailed')
  } finally {
    revokeLoading.value = false
  }
}

useHead({ title: () => t('device.title') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-3xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('device.title') }}</h1>

      <!-- Info banner about missing list endpoint -->
      <div class="rounded-lg border border-info/20 bg-info/10 p-4 mb-6">
        <div class="flex items-start gap-2 text-sm text-info">
          <UIcon name="i-lucide-info" class="w-5 h-5 flex-shrink-0 mt-0.5" />
          <div>
            <p class="font-medium">{{ t('device.listNotAvailable') }}</p>
            <p class="mt-1">{{ t('device.listNotAvailableDesc') }}</p>
          </div>
        </div>
      </div>

      <!-- Authorize new device -->
      <UCard class="mb-6">
        <template #header>
          <h2 class="font-semibold">{{ t('device.authorizeNewDevice') }}</h2>
        </template>
        <p class="text-sm text-muted mb-4">{{ t('device.authorizeDesc') }}</p>
        <form @submit.prevent="handleStart" class="flex gap-2">
          <UInput v-model="deviceName" :placeholder="t('device.deviceNamePlaceholder')" class="flex-1" />
          <UButton type="submit" :loading="startLoading">{{ t('common.start') }}</UButton>
        </form>
        <ErrorDisplay v-if="startError" :message="startError" class="mt-3" />
        <div v-if="startResult" class="mt-4 space-y-2 text-sm">
          <div class="bg-elevated rounded p-3">
            <p><span class="font-medium">{{ t('device.userCode') }}:</span> <code class="text-primary">{{ startResult.user_code }}</code></p>
            <p><span class="font-medium">{{ t('device.verificationUrl') }}:</span> <code class="text-primary break-all">{{ startResult.verification_url }}</code></p>
            <p><span class="font-medium">{{ t('device.expiresIn') }}:</span> {{ startResult.expires_in }}s</p>
            <p><span class="font-medium">{{ t('device.pollInterval') }}:</span> {{ startResult.interval }}s</p>
          </div>
        </div>
      </UCard>

      <!-- Approve/Deny device -->
      <UCard class="mb-6">
        <template #header>
          <h2 class="font-semibold">{{ t('device.approveOrDeny') }}</h2>
        </template>
        <p class="text-sm text-muted mb-4">{{ t('device.approveOrDenyDesc') }}</p>
        <form @submit.prevent="handleApprove" class="space-y-3">
          <UInput v-model="userCode" :placeholder="t('device.userCodePlaceholder')" class="w-full" />
          <div class="flex gap-2">
            <UButton type="submit" :loading="approveLoading">{{ t('common.approve') }}</UButton>
            <UButton color="error" variant="outline" @click="handleDeny">{{ t('common.deny') }}</UButton>
          </div>
        </form>
        <ErrorDisplay v-if="approveError" :message="approveError" class="mt-3" />
        <div v-if="approveSuccess" class="mt-3 rounded border border-success/20 bg-success/10 p-2 text-sm text-success">
          {{ t('device.deviceApproved') }}
        </div>
      </UCard>

      <!-- Revoke session -->
      <UCard>
        <template #header>
          <h2 class="font-semibold text-error">{{ t('device.revokeDeviceSession') }}</h2>
        </template>
        <p class="text-sm text-muted mb-4">{{ t('device.revokeDesc') }}</p>
        <form @submit.prevent="handleRevoke" class="flex gap-2">
          <UInput v-model="revokeSessionId" :placeholder="t('device.sessionIdPlaceholder')" class="flex-1" />
          <UButton type="submit" color="error" :loading="revokeLoading">{{ t('common.revoke') }}</UButton>
        </form>
        <ErrorDisplay v-if="revokeError" :message="revokeError" class="mt-3" />
      </UCard>
    </div>
  </div>
</template>
