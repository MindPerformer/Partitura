<!-- pages/admin/jobs.vue — Index Jobs
//
// 引入动机：design/04-WEB-API.md §页面 要求 Index Jobs 页面。
// 仅 system_admin 可访问。支持列表、重试、触发 rebuild。
-->
<script setup lang="ts">
import type { Job, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { formatDate: formatDateUtil } = useFormatDate()

const jobs = ref<Job[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)
const statusFilter = ref('')

const showRebuildModal = ref(false)
const rebuildLoading = ref(false)

async function loadJobs() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useSearchAdminApi()
    const res = await api.listJobs({ limit: limit.value, offset: offset.value, status: statusFilter.value || undefined })
    jobs.value = res.jobs
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadJobsFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadJobs)

async function handleRetry(jobId: string) {
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

function formatDate(s: string): string {
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
        <UButton variant="outline" @click="showRebuildModal = true">{{ t('admin.triggerRebuild') }}</UButton>
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
              v-for="job in jobs"
              :key="job.id"
              class="flex items-center justify-between border border-default rounded-lg p-3"
            >
              <div class="flex-1">
                <div class="flex items-center gap-2 mb-1">
                  <UBadge :color="statusColors[job.status] ?? 'neutral'" variant="subtle" size="xs">{{ job.status }}</UBadge>
                  <span class="text-sm font-medium">{{ job.type }}</span>
                </div>
                <p class="text-xs text-muted">ID: {{ job.id.substring(0, 8) }}... · {{ t('admin.attempts') }}: {{ job.attempts }}/{{ job.max_attempts }}</p>
                <p class="text-xs text-dimmed">{{ formatDate(job.updated_at) }}</p>
                <p v-if="job.error" class="text-xs text-error mt-1">{{ job.error }}</p>
              </div>
              <UButton
                v-if="job.status === 'failed'"
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
