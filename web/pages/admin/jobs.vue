<!-- pages/admin/jobs.vue — Index Jobs
//
// 引入动机：design/04-WEB-API.md §页面 要求 Index Jobs 页面。
// 仅 system_admin 可访问。支持列表、重试、触发 rebuild。
-->
<script setup lang="ts">
import type { ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { formatDate: formatDateUtil } = useFormatDate()

// 修复说明：列表状态（jobs/total/loading/error）改由 useIndexJobs 提供，
// API boundary 的契约校验、错误收敛都在 composable 内完成。
// 页面不再直接消费未校验的响应，避免脏数据进入模板后在渲染期抛异常导致整页白屏。
const { jobs, total, loading, error, load } = useIndexJobs()

const limit = ref(20)
const offset = ref(0)
const statusFilter = ref('')

const showRebuildModal = ref(false)
const rebuildLoading = ref(false)

async function loadJobs() {
  if (!isSystemAdmin.value) return
  await load({ limit: limit.value, offset: offset.value, status: statusFilter.value || undefined })
}

onMounted(loadJobs)

async function handleRetry(jobId?: string) {
  // id 缺失说明响应契约已被破坏：记录日志并停止，不发起 /jobs/undefined/retry 这类非法请求。
  if (!jobId) {
    console.error('[jobs.vue] job 缺少 id，无法重试')
    error.value = t('admin.invalidJobsResponse')
    return
  }
  try {
    const api = useSearchAdminApi()
    await api.retryJob(jobId)
    await loadJobs()
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.retryJobFailed')
  }
}

async function handleRebuild() {
  rebuildLoading.value = true
  try {
    const api = useSearchAdminApi()
    await api.rebuildIndex({})
    showRebuildModal.value = false
    await loadJobs()
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.rebuildFailed')
  } finally {
    rebuildLoading.value = false
  }
}

// shortId 引入动机：契约被破坏时（如后端返回 Go 字段名）job.id 会是 undefined，
// 直接调用 job.id.substring 会在渲染期抛 TypeError，中断整个列表渲染并白屏。
function shortId(id?: string): string {
  if (!id) return '-'
  return id.substring(0, 8)
}

// formatDate 接受可选值：useFormatDate 对 null/undefined/'' 返回"时间不可用"文案，
// 避免缺失时间字段进入日期解析路径。
function formatDate(s?: string): string {
  return formatDateUtil(s)
}

const statusColors: Record<string, 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error' | 'neutral'> = {
  pending: 'neutral',
  running: 'info',
  completed: 'success',
  failed: 'error',
  dead: 'error'
}

useHead({ title: () => t('admin.indexJobs') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-4xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.indexJobs') }}</h1>
        <UButton v-if="isSystemAdmin" variant="outline" @click="showRebuildModal = true">{{ t('admin.triggerRebuild') }}</UButton>
      </div>

      <div v-if="!isSystemAdmin" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('admin.systemAdminRequired') }}</p>
      </div>

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <div class="flex gap-2 mb-4">
          <UInput v-model="statusFilter" :placeholder="t('admin.filterByStatus')" class="w-48" @keyup.enter="loadJobs" />
          <UButton size="sm" @click="loadJobs">{{ t('common.filter') }}</UButton>
        </div>

        <div v-if="loading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <UCard v-else-if="jobs.length > 0">
          <div class="space-y-2">
            <div
              v-for="(job, index) in jobs"
              :key="job.id ?? index"
              class="flex items-center justify-between border border-default rounded-lg p-3"
            >
              <div class="flex-1">
                <div class="flex items-center gap-2 mb-1">
                  <UBadge :color="statusColors[job.status] ?? 'neutral'" variant="subtle" size="sm">{{ job.status }}</UBadge>
                  <span class="text-sm font-medium">{{ job.type }}</span>
                </div>
                <p class="text-xs text-muted">ID: {{ shortId(job.id) }}... · {{ t('admin.attempts') }}: {{ job.attempts }}/{{ job.max_attempts }}</p>
                <p class="text-xs text-dimmed">{{ formatDate(job.updated_at) }}</p>
                <p v-if="job.error" class="text-xs text-error mt-1">{{ job.error }}</p>
              </div>
              <UButton
                v-if="job.status === 'failed' || job.status === 'dead'"
                size="xs"
                variant="ghost"
                icon="i-lucide-refresh-cw"
                @click="handleRetry(job.id)"
              >{{ t('common.retry') }}</UButton>
            </div>
          </div>
        </UCard>

        <p v-else class="text-center text-muted py-8">{{ t('admin.noJobs') }}</p>

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="(o: number) => { offset = o; loadJobs() }"
        />
      </template>
    </div>

    <UModal v-model:open="showRebuildModal">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-2">{{ t('admin.triggerFullRebuild') }}</h3>
          <p class="text-sm text-muted mb-4">{{ t('admin.rebuildConfirm') }}</p>
          <div class="flex justify-end gap-2">
            <UButton color="neutral" variant="ghost" @click="showRebuildModal = false">{{ t('common.cancel') }}</UButton>
            <UButton color="warning" :loading="rebuildLoading" @click="handleRebuild">{{ t('common.trigger') }}</UButton>
          </div>
        </div>
      </template>
    </UModal>
  </div>
</template>
