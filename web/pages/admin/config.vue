<!-- pages/admin/config.vue — System Settings
//
// 引入动机：Phase6 WP3 要求 system_admin 通过 Web 管理业务配置。
// 仅 system_admin 可访问，编辑后显示 restart_required 提示。
//
// Phase 2 修复：
//   - 按 setting.type 分支渲染控件（bool→USwitch、int/float→数字输入、duration→带单位提示、json→UTextarea 校验）
//   - 行级保存反馈：per-key saveState，成功/失败提示渲染在对应行内；restart_required 成功用 warning
//   - 提交前类型化校验：非法值给字段级错误，不发出请求
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

const runtimeLoading = ref(false)
const runtime = ref<RuntimeStatusResponse['runtime'] | null>(null)
const runtimeError = ref<string | null>(null)

/**
 * 行级保存状态：key → { loading, error, success, restartRequired }
 * 引入动机：原实现把 saveError/saveSuccess 作为页面级单槽，保存一行会污染/清空其它行的提示。
 * 改为 per-key map，每个配置的保存结果只渲染在自己那一行。
 */
interface SettingSaveState {
  loading: boolean
  error: string | null
  success: string | null
  restartRequired: boolean
}
const saveState = ref<Record<string, SettingSaveState>>({})

/** 每个 key 的自动消失定时器，成功后 3.5s 清除成功提示。 */
const successTimers = new Map<string, ReturnType<typeof setTimeout>>()

function getSaveState(key: string): SettingSaveState {
  let s = saveState.value[key]
  if (!s) {
    s = { loading: false, error: null, success: null, restartRequired: false }
    saveState.value[key] = s
  }
  return s
}

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

/**
 * 类型化校验：按 setting.type 校验 value，返回本地化错误消息或 null（通过）。
 * 提交前调用；非法值给字段级错误且不发出请求。
 */
function validateSetting(setting: SystemSetting): string | null {
  const value = setting.value
  // setting.type 的官方联合类型不含 'json'，但后端可能扩展返回；这里按 string 处理以兼容。
  const type: string = setting.type
  switch (type) {
    case 'bool':
      // bool 由 USwitch 直接写 'true'/'false'，无需校验
      return null
    case 'int':
      if (!/^-?\d+$/.test(value.trim())) return t('admin.settingValueInvalidInt')
      return null
    case 'float':
      if (value.trim() === '' || Number.isNaN(Number(value))) return t('admin.settingValueInvalidNumber')
      return null
    case 'duration':
      // 后端统一以整数秒存储时长
      if (!/^\d+$/.test(value.trim())) return t('admin.settingValueInvalidDuration')
      return null
    case 'json':
      try {
        JSON.parse(value)
        return null
      } catch {
        return t('admin.settingValueInvalidJson')
      }
    default:
      // string 及其它类型允许任意值
      return null
  }
}

async function saveSetting(setting: SystemSetting) {
  const state = getSaveState(setting.key)
  state.loading = true
  state.error = null
  state.success = null
  state.restartRequired = false

  // 字段级校验：非法值直接给出错误，不发出请求
  const validationError = validateSetting(setting)
  if (validationError) {
    state.loading = false
    state.error = validationError
    return
  }

  try {
    const api = useSearchAdminApi()
    const res = await api.updateSetting(setting.key, { value: setting.value })
    const idx = settings.value.findIndex(s => s.key === res.key)
    if (idx >= 0 && settings.value[idx]) {
      settings.value[idx]!.value = res.value
    }
    state.restartRequired = res.restart_required
    state.success = res.restart_required ? res.message : t('admin.settingsSaved')

    // 成功提示 3.5s 自动消失
    const existing = successTimers.get(setting.key)
    if (existing) clearTimeout(existing)
    const timer = setTimeout(() => {
      state.success = null
      state.restartRequired = false
      successTimers.delete(setting.key)
    }, 3500)
    successTimers.set(setting.key, timer)
  } catch (err) {
    const apiErr = err as ApiError
    state.error = apiErr.error || t('admin.updateSettingFailed')
  } finally {
    state.loading = false
  }
}

/**
 * bool 类型控件的双向桥接：setting.value 存字符串 'true'/'false'，
 * USwitch 需要 boolean。用 computed 在读写两侧转换。
 */
function boolModel(setting: SystemSetting) {
  return computed<boolean>({
    get: () => setting.value === 'true',
    set: (v: boolean) => { setting.value = v ? 'true' : 'false' }
  })
}

/** json 字段失焦时校验并把非法值标到该行（不阻止输入，但阻止提交）。 */
function onJsonBlur(setting: SystemSetting) {
  if ((setting.type as string) !== 'json') return
  const state = getSaveState(setting.key)
  try {
    JSON.parse(setting.value)
    state.error = null
  } catch {
    state.error = t('admin.settingValueInvalidJson')
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
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-6xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('admin.systemConfig') }}</h1>

      <EmptyState
        v-if="!isSystemAdmin"
        icon="i-lucide-lock"
        :title="t('admin.systemAdminRequired')"
      />

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <div v-if="loading" class="flex justify-center py-8" role="status">
          <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-8 h-8 animate-spin text-muted" />
          <span class="sr-only">{{ t('common.loading') }}</span>
        </div>

        <template v-else>
          <UCard class="mb-6">
            <template #header>
              <h2 class="font-semibold">{{ t('admin.settings') }}</h2>
            </template>

            <EmptyState v-if="settings.length === 0" icon="i-lucide-settings" :title="t('admin.noSettings')" />

            <ul v-else class="space-y-4" role="list">
              <li
                v-for="setting in settings"
                :key="setting.key"
                class="border border-default rounded-xl p-4"
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

                    <!-- 按类型分支渲染控件 -->
                    <template v-if="setting.type === 'bool'">
                      <USwitch :model-value="setting.value === 'true'" @update:model-value="boolModel(setting).value = $event" />
                    </template>
                    <UInput
                      v-else-if="setting.type === 'int'"
                      v-model="setting.value"
                      type="number"
                      :step="1"
                      class="w-full"
                    />
                    <UInput
                      v-else-if="setting.type === 'float'"
                      v-model="setting.value"
                      type="number"
                      step="any"
                      class="w-full"
                    />
                    <div v-else-if="setting.type === 'duration'" class="flex items-center gap-2">
                      <UInput
                        v-model="setting.value"
                        type="number"
                        :step="1"
                        class="w-full"
                      />
                      <span class="text-xs text-muted shrink-0">{{ t('admin.settingDurationHint') }}</span>
                    </div>
                    <UTextarea
                      v-else-if="(setting.type as string) === 'json'"
                      v-model="setting.value"
                      :rows="3"
                      class="w-full font-mono text-sm"
                      @blur="onJsonBlur(setting)"
                    />
                    <UInput v-else v-model="setting.value" class="w-full" />
                  </div>
                  <UButton
                    size="sm"
                    :loading="getSaveState(setting.key).loading"
                    @click="saveSetting(setting)"
                  >
                    {{ t('common.save') }}
                  </UButton>
                </div>

                <!-- 行级保存反馈 -->
                <ErrorDisplay
                  v-if="getSaveState(setting.key).error"
                  :message="getSaveState(setting.key).error!"
                  class="mt-3"
                />
                <UAlert
                  v-else-if="getSaveState(setting.key).success"
                  :color="getSaveState(setting.key).restartRequired ? 'warning' : 'success'"
                  variant="soft"
                  class="mt-3"
                  :title="getSaveState(setting.key).success!"
                />
              </li>
            </ul>
          </UCard>

          <UCard>
            <template #header>
              <h2 class="font-semibold">{{ t('admin.runtimeStatus') }}</h2>
            </template>

            <ErrorDisplay v-if="runtimeError" :message="runtimeError" />

            <div v-if="runtimeLoading" class="flex justify-center py-8" role="status">
              <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-6 h-6 animate-spin text-muted" />
              <span class="sr-only">{{ t('common.loading') }}</span>
            </div>

            <div v-else-if="runtime" class="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div
                v-for="(value, key) in runtime"
                :key="key"
                class="border border-default rounded-xl p-3"
              >
                <p class="text-xs text-muted mb-1">{{ key }}</p>
                <p class="text-sm font-mono break-words">{{ formatRuntimeValue(value) }}</p>
              </div>
            </div>

            <EmptyState v-else icon="i-lucide-activity" :title="t('admin.noRuntime')" />
          </UCard>
        </template>
      </template>
    </div>
  </div>
</template>
