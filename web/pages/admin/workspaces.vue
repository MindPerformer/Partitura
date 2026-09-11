<!-- pages/admin/workspaces.vue — Admin Workspaces
//
// 引入动机：design/04-WEB-API.md §页面 要求 Admin Workspaces 页面。
// 仅 system_admin 可访问，列出全部 workspace。
-->
<script setup lang="ts">
import type { Workspace, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { workspaceStatusLabel } = useEnumLabels()

const workspaces = ref<Workspace[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

// 服务端筛选：后端 /admin/workspaces 支持 status + q 查询参数
const searchQuery = ref('')
const statusFilter = ref('')

function applyFilters() {
  offset.value = 0
  loadWorkspaces()
}

const statusFilterOptions = computed(() => [
  { label: t('admin.all'), value: '' },
  { label: t('workspace.statusActive'), value: 'active' },
  { label: t('workspace.statusArchived'), value: 'archived' }
])

async function loadWorkspaces() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useAdminApi()
    const res = await api.listAllWorkspaces({
      limit: limit.value,
      offset: offset.value,
      status: statusFilter.value || undefined,
      q: searchQuery.value.trim() || undefined
    })
    workspaces.value = res.workspaces
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadWorkspacesFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadWorkspaces)

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadWorkspaces()
}

// ---- 键盘导航：j/k 上下移动选中行，Enter/o 查看 workspace，Escape 清除选中 ----
const selectedIndex = ref(-1)
const tableEl = ref<HTMLElement | null>(null)

// 数据变化时清选中，避免指向已不存在的行。
watch(workspaces, () => { selectedIndex.value = -1 })

function moveSelection(delta: number) {
  if (workspaces.value.length === 0) return
  const next = selectedIndex.value < 0
    ? (delta > 0 ? 0 : workspaces.value.length - 1)
    : Math.min(Math.max(selectedIndex.value + delta, 0), workspaces.value.length - 1)
  selectedIndex.value = next
  tableEl.value?.querySelectorAll('tbody tr')[next]?.scrollIntoView({ block: 'nearest' })
}

function openSelected() {
  const ws = workspaces.value[selectedIndex.value]
  if (ws) navigateTo(`/workspaces/${ws.id}`)
}

function clearSelection() {
  selectedIndex.value = -1
}

useHotkey('j', () => moveSelection(1))
useHotkey('k', () => moveSelection(-1))
useHotkey('enter', openSelected)
useHotkey('o', openSelected)
useHotkey('escape', clearSelection)

useHead({ title: () => t('admin.adminWorkspaces') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-6xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('admin.adminWorkspaces') }}</h1>

      <EmptyState
        v-if="!isSystemAdmin"
        icon="i-lucide-lock"
        :title="t('admin.systemAdminRequired')"
      />

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <div class="flex gap-2 mb-4">
          <UInput
            v-model="searchQuery"
            :placeholder="t('admin.searchWorkspacesPlaceholder')"
            icon="i-lucide-search"
            class="w-64"
            @keyup.enter="applyFilters"
          />
          <USelect
            v-model="statusFilter"
            :items="statusFilterOptions"
            value-key="value"
            label-key="label"
            class="w-40"
            :aria-label="t('admin.filterByStatus')"
            @update:model-value="applyFilters"
          />
          <UButton size="sm" variant="outline" icon="i-lucide-search" @click="applyFilters">{{ t('common.search') }}</UButton>
        </div>

        <div v-if="loading" class="flex justify-center py-8" role="status">
          <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-8 h-8 animate-spin text-muted" />
          <span class="sr-only">{{ t('common.loading') }}</span>
        </div>

        <UCard v-else-if="workspaces.length > 0" class="overflow-x-auto">
          <table class="w-full text-sm" ref="tableEl">
            <thead>
              <tr class="text-left text-muted border-b border-default">
                <th class="py-2 pr-3 font-medium">{{ t('workspace.name') }}</th>
                <th class="py-2 pr-3 font-medium">{{ t('workspace.status') }}</th>
                <th class="py-2 font-medium sr-only">{{ t('common.view') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(ws, idx) in workspaces"
                :key="ws.id"
                class="border-b border-default last:border-0 transition-colors"
                :class="idx === selectedIndex ? 'bg-elevated' : ''"
                :aria-selected="idx === selectedIndex"
              >
                <td class="py-2 pr-3">
                  <p class="text-sm font-medium text-highlighted">{{ ws.display_name }}</p>
                  <p class="text-xs text-muted" :title="ws.created_by">{{ ws.name }} · {{ t('admin.createdBy') }} {{ ws.created_by_username || (ws.created_by ? ws.created_by.substring(0, 8) + '…' : '-') }}</p>
                </td>
                <td class="py-2 pr-3">
                  <UBadge :color="ws.status === 'active' ? 'success' : 'neutral'" variant="subtle" size="sm">{{ workspaceStatusLabel(ws.status) }}</UBadge>
                </td>
                <td class="py-2">
                  <UButton size="xs" variant="ghost" icon="i-lucide-eye" :aria-label="t('common.view')" :to="`/workspaces/${ws.id}`" />
                </td>
              </tr>
            </tbody>
          </table>
        </UCard>

        <EmptyState v-else icon="i-lucide-folder-open" :title="t('admin.noWorkspaces')" />

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
