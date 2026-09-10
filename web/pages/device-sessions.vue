<!-- pages/device-sessions.vue — API / Device Sessions -->
<script setup lang="ts">
import type { ApiError, DeviceAuthStartResponse, DeviceSession } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()

const sessions = ref<DeviceSession[]>([])
const sessionsLoading = ref(false)
const sessionsError = ref<string | null>(null)

const deviceName = ref('')
const startResult = ref<DeviceAuthStartResponse | null>(null)
const startLoading = ref(false)
const startError = ref<string | null>(null)

const userCode = ref('')
const approveLoading = ref(false)
const approveError = ref<string | null>(null)
const approveSuccess = ref(false)

async function loadSessions() {
  sessionsLoading.value = true
  sessionsError.value = null
  try {
    const response = await useAuthApi().listSessions({ limit: 100 })
    if (!Array.isArray(response.sessions) || typeof response.total !== 'number') {
      console.error('[device-sessions] 设备会话列表响应非法', response)
      sessions.value = []
      sessionsError.value = t('device.invalidListResponse')
      return
    }
    sessions.value = response.sessions
  } catch (err) {
    const apiErr = err as ApiError
    sessionsError.value = apiErr.error || t('device.loadSessionsFailed')
  } finally {
    sessionsLoading.value = false
  }
}

onMounted(loadSessions)

async function handleStart() {
  startLoading.value = true
  startError.value = null
  try {
    startResult.value = await useAuthApi().deviceStart(deviceName.value)
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
    await useAuthApi().deviceApprove(userCode.value)
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
    await useAuthApi().deviceDeny(userCode.value)
    userCode.value = ''
  } catch (err) {
    const apiErr = err as ApiError
    approveError.value = apiErr.error || t('device.denyFailed')
  }
}

async function handleRevoke(session: DeviceSession) {
  if (session.revoked_at || !window.confirm(t('device.revokeConfirm', { name: session.device_name }))) return
  try {
    await useAuthApi().revoke(session.id)
    await loadSessions()
  } catch (err) {
    const apiErr = err as ApiError
    sessionsError.value = apiErr.error || t('device.revokeFailed')
  }
}

function formatDate(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? t('common.timeUnavailable') : date.toLocaleString()
}

useHead({ title: () => t('device.title') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-3xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('device.title') }}</h1>

      <UCard class="mb-6">
        <template #header>
          <div class="flex items-center justify-between">
            <h2 class="font-semibold">{{ t('device.loggedInDevices') }}</h2>
            <UButton size="sm" variant="outline" :loading="sessionsLoading" @click="loadSessions">{{ t('common.retry') }}</UButton>
          </div>
        </template>
        <ErrorDisplay v-if="sessionsError" :message="sessionsError" class="mb-3" />
        <div v-if="sessionsLoading" class="flex justify-center py-6">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>
        <div v-else-if="sessions.length" class="space-y-3">
          <div v-for="session in sessions" :key="session.id" class="rounded-lg border border-default p-4">
            <div class="flex items-start justify-between gap-4">
              <div class="min-w-0">
                <p class="font-medium text-highlighted">{{ session.device_name }}</p>
                <p class="text-xs text-muted mt-1">{{ t('device.createdAt') }}: {{ formatDate(session.created_at) }}</p>
                <p class="text-xs text-muted">{{ t('device.expiresAt') }}: {{ formatDate(session.refresh_expires_at) }}</p>
                <p v-if="session.revoked_at" class="text-xs text-error mt-1">{{ t('device.revokedAt') }}: {{ formatDate(session.revoked_at) }}</p>
              </div>
              <UButton color="error" variant="outline" size="sm" :disabled="Boolean(session.revoked_at)" @click="handleRevoke(session)">
                {{ session.revoked_at ? t('device.revoked') : t('common.revoke') }}
              </UButton>
            </div>
          </div>
        </div>
        <p v-else class="text-center text-muted py-6">{{ t('device.noSessions') }}</p>
      </UCard>

      <UCard class="mb-6">
        <template #header><h2 class="font-semibold">{{ t('device.authorizeNewDevice') }}</h2></template>
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

      <UCard>
        <template #header><h2 class="font-semibold">{{ t('device.approveOrDeny') }}</h2></template>
        <p class="text-sm text-muted mb-4">{{ t('device.approveOrDenyDesc') }}</p>
        <form @submit.prevent="handleApprove" class="space-y-3">
          <UInput v-model="userCode" :placeholder="t('device.userCodePlaceholder')" class="w-full" />
          <div class="flex gap-2">
            <UButton type="submit" :loading="approveLoading">{{ t('common.approve') }}</UButton>
            <UButton color="error" variant="outline" @click="handleDeny">{{ t('common.deny') }}</UButton>
          </div>
        </form>
        <ErrorDisplay v-if="approveError" :message="approveError" class="mt-3" />
        <div v-if="approveSuccess" class="mt-3 rounded border border-success/20 bg-success/10 p-2 text-sm text-success">{{ t('device.deviceApproved') }}</div>
      </UCard>
    </div>
  </div>
</template>
