<!-- pages/admin/audit.vue — Audit Log
//
// 引入动机：design/04-WEB-API.md §页面 要求 Audit Log 页面。
// 仅 system_admin 可访问。
-->
<script setup lang="ts">
import type { AuditEntry, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { formatDate: formatDateUtil } = useFormatDate()

const entries = ref<AuditEntry[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

/**
 * 审计筛选（服务端过滤）。
 *
 * 后端 GET /api/admin/audit 支持 user_id/action/resource_type/workspace_id/
 * from/to 查询参数（from/to 为 RFC3339；非法值返回 400）。筛选作用于服务端
 * 全量数据，返回真实 total。变更筛选时重置 offset 回到第一页。
 */
const filterUserId = ref('')
const filterAction = ref('')
const filterResourceType = ref('')
const filterFrom = ref('')
const filterTo = ref('')

function applyFilters() {
  offset.value = 0
  loadAudit()
}

async function loadAudit() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useAdminApi()
    const res = await api.listAudit({
      limit: limit.value,
      offset: offset.value,
      user_id: filterUserId.value.trim() || undefined,
      action: filterAction.value.trim() || undefined,
      resource_type: filterResourceType.value.trim() || undefined,
      from: filterFrom.value ? new Date(filterFrom.value).toISOString() : undefined,
      to: filterTo.value ? new Date(filterTo.value).toISOString() : undefined
    })
    entries.value = res.entries
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadAuditFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadAudit)

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadAudit()
}

function formatDate(s: string): string {
  return formatDateUtil(s)
}

function formatDetail(detail: unknown): string {
  if (!detail) return ''
  try {
    return JSON.stringify(detail, null, 2)
  } catch {
    return String(detail)
  }
}

// ---- 键盘导航：j/k 上下移动选中行，Enter/o 展开详情，Escape 清除选中 ----
// 审计条目主操作是"展开/收起 detail"；无 detail 的行展开即清除选中。
const selectedIndex = ref(-1)
const expandedEntryId = ref<number | null>(null)
const listEl = ref<HTMLElement | null>(null)

// 数据变化时清选中，避免 selectedIndex 指向已不存在的行。
watch(entries, () => { selectedIndex.value = -1; expandedEntryId.value = null })

function moveSelection(delta: number) {
  if (entries.value.length === 0) return
  const next = selectedIndex.value < 0
    ? (delta > 0 ? 0 : entries.value.length - 1)
    : Math.min(Math.max(selectedIndex.value + delta, 0), entries.value.length - 1)
  selectedIndex.value = next
  // 滚动使选中行可见
  listEl.value?.querySelectorAll('li')[next]?.scrollIntoView({ block: 'nearest' })
}

function toggleSelected() {
  const entry = entries.value[selectedIndex.value]
  if (!entry) return
  // 无 detail 的行无可展开内容，清除选中
  if (!entry.detail) { selectedIndex.value = -1; return }
  expandedEntryId.value = expandedEntryId.value === entry.id ? null : entry.id
}

function clearSelection() {
  selectedIndex.value = -1
  expandedEntryId.value = null
}

useHotkey('j', () => moveSelection(1))
useHotkey('k', () => moveSelection(-1))
useHotkey('enter', toggleSelected)
useHotkey('o', toggleSelected)
useHotkey('escape', clearSelection)

useHead({ title: () => t('admin.auditLog') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-6xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('admin.auditLog') }}</h1>

      <EmptyState
        v-if="!isSystemAdmin"
        icon="i-lucide-lock"
        :title="t('admin.systemAdminRequired')"
      />

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <!-- 筛选：服务端过滤全量数据（后端支持 user_id/action/resource_type/from/to） -->
        <div class="grid grid-cols-2 sm:grid-cols-5 gap-2 mb-4">
          <UInput v-model="filterUserId" :placeholder="t('admin.auditFilterUserId')" class="w-full" @keyup.enter="applyFilters" />
          <UInput v-model="filterAction" :placeholder="t('admin.auditFilterAction')" class="w-full" @keyup.enter="applyFilters" />
          <UInput v-model="filterResourceType" :placeholder="t('admin.auditFilterResourceType')" class="w-full" @keyup.enter="applyFilters" />
          <UInput v-model="filterFrom" type="datetime-local" :aria-label="t('admin.auditFilterFrom')" class="w-full" @change="applyFilters" />
          <UInput v-model="filterTo" type="datetime-local" :aria-label="t('admin.auditFilterTo')" class="w-full" @change="applyFilters" />
        </div>
        <div class="mb-4">
          <UButton size="sm" variant="outline" @click="applyFilters">{{ t('admin.applyFilters') }}</UButton>
        </div>

        <div v-if="loading" class="flex justify-center py-8" role="status">
          <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-8 h-8 animate-spin text-muted" />
          <span class="sr-only">{{ t('common.loading') }}</span>
        </div>

        <UCard v-else-if="entries.length > 0">
          <ul ref="listEl" class="space-y-2" role="list">
            <li
              v-for="(entry, idx) in entries"
              :key="entry.id"
              class="border border-default rounded-xl p-3 transition-colors"
              :class="idx === selectedIndex ? 'ring-1 ring-primary/60 bg-elevated' : ''"
              :aria-selected="idx === selectedIndex"
            >
              <div class="flex items-start justify-between mb-1">
                <div class="flex items-center gap-2">
                  <UBadge variant="subtle" size="sm">{{ entry.action }}</UBadge>
                  <span class="text-xs text-muted">{{ entry.resource_type }}</span>
                </div>
                <span class="text-xs text-dimmed">{{ formatDate(entry.created_at) }}</span>
              </div>
              <p class="text-xs text-muted">
                User: {{ entry.user_id.substring(0, 8) }}...
                <template v-if="entry.workspace_id"> · WS: {{ entry.workspace_id.substring(0, 8) }}...</template>
                <template v-if="entry.resource_id"> · Resource: {{ entry.resource_id.substring(0, 8) }}...</template>
              </p>
              <pre
                v-if="entry.detail"
                class="text-xs text-dimmed mt-1 bg-elevated rounded p-2 overflow-x-auto"
                :class="expandedEntryId === entry.id ? '' : 'max-h-24 line-clamp-3'"
              >{{ formatDetail(entry.detail) }}</pre>
            </li>
          </ul>
        </UCard>

        <EmptyState v-else icon="i-lucide-file-text" :title="t('admin.noAuditEntries')" />

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="handleOffsetChange"
        />
      </template>
    </div>
  </div>
</template>
