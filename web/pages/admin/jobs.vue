<!-- pages/admin/jobs.vue — Index Jobs
//
// 引入动机：design/04-WEB-API.md §页面 要求 Index Jobs 页面。
// 仅 system_admin 可访问。支持列表、重试、触发 rebuild。
//
// Phase 2 修复：
//   - 状态筛选从自由文本改 USelect（选项 = jobStatusLabel 键集合 + 全部）
//   - 列表加载错误（listError）与操作错误（actionError）分离；操作错误用 useToast
//   - 手动刷新按钮；rebuild 弹窗文案明确"全量重建"
//   - 列表改语义化 <table>
-->
<script setup lang="ts">
import type { ApiError } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { jobStatusLabel, jobTypeLabel } = useEnumLabels()
const { formatDate: formatDateUtil } = useFormatDate()
const toast = useToast()

// 修复说明：列表状态（jobs/total/loading/error）改由 useIndexJobs 提供，
// API boundary 的契约校验、错误收敛都在 composable 内完成。
// 页面不再直接消费未校验的响应，避免脏数据进入模板后在渲染期抛异常导致整页白屏。
const { jobs, total, loading, error: listError, load } = useIndexJobs()

const limit = ref(20)
const offset = ref(0)
const statusFilter = ref('')

const showRebuildModal = ref(false)
const rebuildLoading = ref(false)

const statusOptions = computed<SelectItem[]>(() => [
  { label: t('admin.all'), value: '' },
  { label: t('admin.statusPending'), value: 'pending' },
  { label: t('admin.statusRunning'), value: 'running' },
  { label: t('admin.statusCompleted'), value: 'completed' },
  { label: t('admin.statusFailed'), value: 'failed' },
  { label: t('admin.statusDead'), value: 'dead' }
])

async function loadJobs() {
  if (!isSystemAdmin.value) return
  await load({ limit: limit.value, offset: offset.value, status: statusFilter.value || undefined })
}

onMounted(loadJobs)

// 筛选/翻页变更重置 offset
function onStatusChange() {
  offset.value = 0
  loadJobs()
}

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadJobs()
}

async function handleRetry(jobId?: string) {
  // id 缺失说明响应契约已被破坏：记录日志并停止，不发起 /jobs/undefined/retry 这类非法请求。
  if (!jobId) {
    console.error('[jobs.vue] job 缺少 id，无法重试')
    toast.add({ title: t('admin.invalidJobsResponse'), color: 'error' })
    return
  }
  try {
    const api = useSearchAdminApi()
    await api.retryJob(jobId)
    await loadJobs()
  } catch (err) {
    const apiErr = err as ApiError
    toast.add({ title: apiErr.error || t('admin.retryJobFailed'), color: 'error' })
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
    toast.add({ title: apiErr.error || t('admin.rebuildFailed'), color: 'error' })
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

// ---- 键盘导航：j/k 上下移动选中行，Enter/o 触发主操作（failed/dead 行=重试），Escape 清除 ----
const selectedIndex = ref(-1)
const tableEl = ref<HTMLElement | null>(null)

// 数据变化时清选中，避免指向已不存在的行。
watch(jobs, () => { selectedIndex.value = -1 })

function moveSelection(delta: number) {
  if (jobs.value.length === 0) return
  const next = selectedIndex.value < 0
    ? (delta > 0 ? 0 : jobs.value.length - 1)
    : Math.min(Math.max(selectedIndex.value + delta, 0), jobs.value.length - 1)
  selectedIndex.value = next
  tableEl.value?.querySelectorAll('tbody tr')[next]?.scrollIntoView({ block: 'nearest' })
}

function openSelected() {
  const job = jobs.value[selectedIndex.value]
  if (!job) return
  // 主操作：仅 failed/dead 行可重试；其它行无可触发动作，保持选中即可。
  if (job.status === 'failed' || job.status === 'dead') {
    handleRetry(job.id)
  }
}

function clearSelection() {
  selectedIndex.value = -1
}

useHotkey('j', () => moveSelection(1))
useHotkey('k', () => moveSelection(-1))
useHotkey('enter', openSelected)
useHotkey('o', openSelected)
useHotkey('escape', clearSelection)

useHead({ title: () => t('admin.indexJobs') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-6xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.indexJobs') }}</h1>
        <div class="flex gap-2">
          <UButton
            v-if="isSystemAdmin"
            variant="ghost"
            icon="i-lucide-refresh-cw"
            :aria-label="t('admin.refresh')"
            @click="loadJobs"
          />
          <UButton v-if="isSystemAdmin" variant="outline" @click="showRebuildModal = true">{{ t('admin.triggerRebuild') }}</UButton>
        </div>
      </div>

      <EmptyState
        v-if="!isSystemAdmin"
        icon="i-lucide-lock"
        :title="t('admin.systemAdminRequired')"
      />

      <template v-else>
        <ErrorDisplay v-if="listError" :message="listError" />

        <div class="flex gap-2 mb-4">
          <USelect
            v-model="statusFilter"
            :items="statusOptions"
            value-key="value"
            label-key="label"
            class="w-48"
            :aria-label="t('admin.filterByStatus')"
            @update:model-value="onStatusChange"
          />
        </div>

        <div v-if="loading" class="flex justify-center py-8" role="status">
          <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-8 h-8 animate-spin text-muted" />
          <span class="sr-only">{{ t('common.loading') }}</span>
        </div>

        <UCard v-else-if="jobs.length > 0" class="overflow-x-auto">
          <table class="w-full text-sm" ref="tableEl">
            <thead>
              <tr class="text-left text-muted border-b border-default">
                <th class="py-2 pr-3 font-medium">{{ t('admin.providerStatus') }}</th>
                <th class="py-2 pr-3 font-medium">{{ t('admin.providerType') }}</th>
                <th class="py-2 pr-3 font-medium">ID</th>
                <th class="py-2 pr-3 font-medium">{{ t('admin.attempts') }}</th>
                <th class="py-2 pr-3 font-medium">{{ t('admin.lastUpdated') }}</th>
                <th class="py-2 font-medium sr-only">{{ t('common.view') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(job, index) in jobs"
                :key="job.id ?? index"
                class="border-b border-default last:border-0 align-top transition-colors"
                :class="index === selectedIndex ? 'bg-elevated' : ''"
                :aria-selected="index === selectedIndex"
              >
                <td class="py-2 pr-3">
                  <UBadge :color="statusColors[job.status] ?? 'neutral'" variant="subtle" size="sm">{{ jobStatusLabel(job.status) }}</UBadge>
                </td>
                <td class="py-2 pr-3">
                  <span class="font-medium">{{ jobTypeLabel(job.type) }}</span>
                  <p v-if="job.error" class="text-xs text-error mt-1">{{ job.error }}</p>
                </td>
                <td class="py-2 pr-3 font-mono text-xs">{{ shortId(job.id) }}...</td>
                <td class="py-2 pr-3 text-xs">{{ job.attempts }}/{{ job.max_attempts }}</td>
                <td class="py-2 pr-3 text-xs text-dimmed">{{ formatDate(job.updated_at) }}</td>
                <td class="py-2">
                  <UButton
                    v-if="job.status === 'failed' || job.status === 'dead'"
                    size="xs"
                    variant="ghost"
                    icon="i-lucide-refresh-cw"
                    @click="handleRetry(job.id)"
                  >{{ t('common.retry') }}</UButton>
                </td>
              </tr>
            </tbody>
          </table>
        </UCard>

        <EmptyState v-else icon="i-lucide-cog" :title="t('admin.noJobs')" />

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="handleOffsetChange"
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
