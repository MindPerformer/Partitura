<!-- pages/admin/config.vue — System Settings
//
// 引入动机：Phase6 WP3 要求 system_admin 通过 Web 管理业务配置。
// 仅 system_admin 可访问，编辑后显示 restart_required 提示。
-->
<script setup lang="ts">
import type { SystemSetting, ApiError, RuntimeStatusResponse } from '~/types/api'


definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { settingTypeLabel } = useEnumLabels()

const settings = ref<SystemSetting[]>([])
const loading = ref(false)
const error = ref<string | null>(null)
const savingKey = ref<string | null>(null)
const saveError = ref<string | null>(null)
const saveSuccess = ref<string | null>(null)

const runtimeLoading = ref(false)
const runtime = ref<RuntimeStatusResponse['runtime'] | null>(null)
const runtimeError = ref<string | null>(null)

async function loadSettings() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useSearchAdminApi()
    const res = await api.listSettings()
    settings.value = res.settings
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadSettingsFailed')
  } finally {
    loading.value = false
  }
}

async function loadRuntime() {
  if (!isSystemAdmin.value) return
  runtimeLoading.value = true
  runtimeError.value = null
  try {
    const api = useSearchAdminApi()
    const res = await api.getRuntimeStatus()
    runtime.value = res.runtime
  } catch (err) {
    const apiErr = err as ApiError
    runtimeError.value = apiErr.error || t('admin.loadRuntimeFailed')
  } finally {
    runtimeLoading.value = false
  }
}

onMounted(() => {
  loadSettings()
  loadRuntime()
})

async function saveSetting(setting: SystemSetting) {
  savingKey.value = setting.key
  saveError.value = null
  saveSuccess.value = null
  try {
    const api = useSearchAdminApi()
    const res = await api.updateSetting(setting.key, { value: setting.value })
    const idx = settings.value.findIndex(s => s.key === res.key)
    if (idx >= 0 && settings.value[idx]) {
      settings.value[idx]!.value = res.value
    }
    saveSuccess.value = res.restart_required ? res.message : t('admin.settingsSaved')
  } catch (err) {
    const apiErr = err as ApiError
    saveError.value = apiErr.error || t('admin.updateSettingFailed')
  } finally {
    savingKey.value = null
  }
}

function formatRuntimeValue(v: unknown): string {
  if (typeof v === 'boolean') return v ? t('common.yes') : t('common.no')
  if (typeof v === 'object') return JSON.stringify(v)
  return String(v)
}

useHead({ title: () => t('admin.systemConfig') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-4xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('admin.systemConfig') }}</h1>

      <div v-if="!isSystemAdmin" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('admin.systemAdminRequired') }}</p>
      </div>

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <div v-if="loading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <template v-else>
          <ErrorDisplay v-if="saveError" :message="saveError" />
          <UAlert
            v-else-if="saveSuccess"
            color="success"
            variant="soft"
            class="mb-4"
            :title="saveSuccess"
          />

          <UCard class="mb-6">
            <template #header>
              <h2 class="font-semibold">{{ t('admin.settings') }}</h2>
            </template>

            <div v-if="settings.length === 0" class="text-center text-muted py-8">
              {{ t('admin.noSettings') }}
            </div>

            <div v-else class="space-y-4">
              <div
                v-for="setting in settings"
                :key="setting.key"
                class="border border-default rounded-lg p-4"
              >
                <div class="flex items-start justify-between gap-4">
                  <div class="flex-1 min-w-0">
                    <div class="flex items-center gap-2 mb-1">
                      <span class="font-medium text-sm text-highlighted">{{ setting.key }}</span>
                      <UBadge size="sm" variant="subtle">{{ settingTypeLabel(setting.type) }}</UBadge>
                      <UBadge v-if="setting.restart_required" color="warning" size="sm" variant="subtle">
                        {{ t('admin.restartRequired') }}
                      </UBadge>
                    </div>
                    <p class="text-xs text-muted mb-2">{{ setting.description }}</p>
                    <UInput v-model="setting.value" class="w-full" />
                  </div>
                  <UButton
                    size="sm"
                    :loading="savingKey === setting.key"
                    @click="saveSetting(setting)"
                  >
                    {{ t('common.save') }}
                  </UButton>
                </div>
              </div>
            </div>
          </UCard>

          <UCard>
            <template #header>
              <h2 class="font-semibold">{{ t('admin.runtimeStatus') }}</h2>
            </template>

            <ErrorDisplay v-if="runtimeError" :message="runtimeError" />

            <div v-if="runtimeLoading" class="flex justify-center py-8">
              <UIcon name="i-lucide-loader-circle" class="w-6 h-6 animate-spin text-muted" />
            </div>

            <div v-else-if="runtime" class="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div
                v-for="(value, key) in runtime"
                :key="key"
                class="border border-default rounded p-3"
              >
                <p class="text-xs text-muted mb-1">{{ key }}</p>
                <p class="text-sm font-mono break-words">{{ formatRuntimeValue(value) }}</p>
              </div>
            </div>

            <p v-else class="text-center text-muted py-4">{{ t('admin.noRuntime') }}</p>
          </UCard>
        </template>
      </template>
    </div>
  </div>
</template>
