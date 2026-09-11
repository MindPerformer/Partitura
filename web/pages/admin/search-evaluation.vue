<!-- pages/admin/search-evaluation.vue — Search Evaluation
//
// 引入动机：design/04-WEB-API.md §页面 要求 Search Evaluation 页面。
// 仅 system_admin 可访问。支持数据集管理、添加评测条目、运行评测、查看结果。
//
// Phase 3 接入：
//   - 数据集卡片"展开"展示条目列表（listItems 分页）与评测结果（listEvaluationResults）
//   - 条目可删除（deleteItem + UModal 确认）
//   - 数据集可归档（archiveDataset + UModal 确认；软删保留全部数据，归档后不可再评测/加条目）
//   - handleAddItem 成功后刷新当前展开数据集的条目列表
-->
<script setup lang="ts">
import type { ApiError, EvaluationDataset, EvaluationItem, EvaluationResult } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { formatDate } = useFormatDate()
const { datasetStatusLabel } = useEnumLabels()
const toast = useToast()

const {
  datasets,
  datasetsTotal,
  profiles,
  loading,
  error,
  load: loadEvaluationData,
  listResults,
  listItems,
  deleteItem,
  archiveDataset
} = useSearchEvaluation()
const limit = ref(20)
const offset = ref(0)

const showCreateDataset = ref(false)
const datasetForm = ref({ name: '', description: '' })
const datasetLoading = ref(false)

const showAddItem = ref(false)
const selectedDataset = ref<EvaluationDataset | null>(null)
const itemForm = ref({ query: '', relevance_grade: 1, query_class: 'general', expected_documents: '' })
const itemLoading = ref(false)
const itemError = ref<string | null>(null)
/** expected_documents 字段级校验错误（UUID 格式），与提交错误分离。 */
const expectedDocsError = ref<string | null>(null)

const showRunEval = ref(false)
const runForm = ref({ dataset_id: '', profile_id: '' })
const runLoading = ref(false)
const runResult = ref<string | null>(null)
const runResultJobId = ref<string | null>(null)

// 展开的数据集 → 评测条目（items）+ 评测结果（results）
const expandedDatasetId = ref<string | null>(null)

// 条目列表状态
const items = ref<EvaluationItem[]>([])
const itemsTotal = ref(0)
const itemsLimit = ref(20)
const itemsOffset = ref(0)
const itemsLoading = ref(false)
const itemsError = ref<string | null>(null)

// 评测结果状态
const results = ref<EvaluationResult[]>([])
const resultsTotal = ref(0)
const resultsLoading = ref(false)
const resultsError = ref<string | null>(null)

// 删除确认状态
const deleteTarget = ref<{ kind: 'item'; datasetId: string; itemId: string; label: string } | { kind: 'dataset'; datasetId: string; label: string } | null>(null)
const deleteLoading = ref(false)

// 数据集状态筛选：'' = 全部，'active' = 活跃，'archived' = 已归档
const statusFilter = ref('')
const statusFilterItems = computed<SelectItem[]>(() => [
  { label: t('admin.filterAllStatuses'), value: '' },
  { label: t('admin.statusActive'), value: 'active' },
  { label: t('admin.statusArchived'), value: 'archived' }
])

async function loadDatasets() {
  if (!isSystemAdmin.value) return
  await loadEvaluationData({ limit: limit.value, offset: offset.value, status: statusFilter.value || undefined })
}

onMounted(loadDatasets)

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadDatasets()
}

function handleStatusFilterChange() {
  offset.value = 0
  loadDatasets()
}

/** 展开/收起数据集面板：展示条目列表与评测结果。 */
async function toggleDataset(ds: EvaluationDataset) {
  if (expandedDatasetId.value === ds.id) {
    expandedDatasetId.value = null
    return
  }
  expandedDatasetId.value = ds.id
  itemsOffset.value = 0
  await Promise.all([loadItems(ds.id), loadResults(ds.id)])
}

async function loadItems(datasetId: string) {
  itemsLoading.value = true
  itemsError.value = null
  try {
    const res = await listItems(datasetId, { limit: itemsLimit.value, offset: itemsOffset.value })
    items.value = res.items
    itemsTotal.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    items.value = []
    itemsTotal.value = 0
    itemsError.value = apiErr.error || t('admin.loadItemsFailed')
  } finally {
    itemsLoading.value = false
  }
}

function handleItemsOffsetChange(newOffset: number) {
  itemsOffset.value = newOffset
  if (expandedDatasetId.value) {
    loadItems(expandedDatasetId.value)
  }
}

async function loadResults(datasetId: string) {
  resultsLoading.value = true
  resultsError.value = null
  try {
    const res = await listResults(datasetId, { limit: 50, offset: 0 })
    results.value = res.results
    resultsTotal.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    results.value = []
    resultsTotal.value = 0
    resultsError.value = apiErr.error || t('admin.loadEvaluationFailed')
  } finally {
    resultsLoading.value = false
  }
}

/** 打开删除确认弹窗。 */
function requestDelete(target: NonNullable<typeof deleteTarget.value>) {
  deleteTarget.value = target
}

/** 确认删除：条目删除（物理）或数据集归档（软删）。 */
async function confirmDelete() {
  const target = deleteTarget.value
  if (!target) return
  deleteLoading.value = true
  try {
    if (target.kind === 'item') {
      await deleteItem(target.datasetId, target.itemId)
      toast.add({ title: t('admin.itemDeleted'), color: 'success' })
      // 刷新当前展开的条目列表
      if (expandedDatasetId.value === target.datasetId) {
        await loadItems(target.datasetId)
      }
    } else {
      // 归档数据集（软删）：status → archived，保留全部数据
      await archiveDataset(target.datasetId)
      toast.add({ title: t('admin.datasetArchived'), color: 'success' })
      // 本地更新该数据集 status，无需刷新即可反映归档态
      const ds = datasets.value.find(d => d.id === target.datasetId)
      if (ds) ds.status = 'archived'
      if (expandedDatasetId.value === target.datasetId) {
        expandedDatasetId.value = null
      }
    }
    deleteTarget.value = null
  } catch (err) {
    const apiErr = err as ApiError
    toast.add({ title: apiErr.error || t('admin.deleteFailed'), color: 'error' })
  } finally {
    deleteLoading.value = false
  }
}

async function handleCreateDataset() {
  if (!datasetForm.value.name) return
  datasetLoading.value = true
  try {
    const api = useSearchAdminApi()
    await api.createDataset(datasetForm.value)
    showCreateDataset.value = false
    datasetForm.value = { name: '', description: '' }
    await loadDatasets()
  } catch (err) {
    const apiErr = err as ApiError
    toast.add({ title: apiErr.error || t('admin.createDatasetFailed'), color: 'error' })
  } finally {
    datasetLoading.value = false
  }
}

/** 校验 expected_documents 输入：每个 token 必须是合法 UUID。返回错误消息或 null。 */
function validateExpectedDocuments(input: string): string | null {
  const trimmed = input.trim()
  if (!trimmed) return null
  const uuidRe = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/
  const tokens = trimmed.split(',').map(s => s.trim()).filter(s => s.length > 0)
  for (const token of tokens) {
    if (!uuidRe.test(token)) {
      return t('admin.expectedDocumentInvalid')
    }
  }
  return null
}

async function handleAddItem() {
  if (!selectedDataset.value || !itemForm.value.query) return
  itemError.value = null
  expectedDocsError.value = null

  const docErr = validateExpectedDocuments(itemForm.value.expected_documents)
  if (docErr) {
    expectedDocsError.value = docErr
    return
  }

  itemLoading.value = true
  try {
    const api = useSearchAdminApi()
    const docs = itemForm.value.expected_documents
      ? itemForm.value.expected_documents.split(',').map(s => ({ document_id: s.trim() })).filter(d => d.document_id)
      : []
    await api.addItem(selectedDataset.value.id, {
      query: itemForm.value.query,
      expected_documents: docs,
      relevance_grade: itemForm.value.relevance_grade,
      query_class: itemForm.value.query_class
    })
    showAddItem.value = false
    itemForm.value = { query: '', relevance_grade: 1, query_class: 'general', expected_documents: '' }
    toast.add({ title: t('admin.itemAdded'), color: 'success' })
    // 若该数据集当前展开，刷新条目列表（新增条目应立即可见）
    if (expandedDataset.value?.id === selectedDataset.value.id) {
      await loadItems(selectedDataset.value.id)
    }
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 409) {
      itemError.value = t('admin.datasetArchivedError')
    } else {
      itemError.value = apiErr.error || t('admin.addItemFailed')
    }
  } finally {
    itemLoading.value = false
  }
}

async function handleRunEval() {
  if (!runForm.value.dataset_id || !runForm.value.profile_id) return
  runLoading.value = true
  runResult.value = null
  runResultJobId.value = null
  try {
    const api = useSearchAdminApi()
    const res = await api.runEvaluation({
      dataset_id: runForm.value.dataset_id,
      profile_id: runForm.value.profile_id
    })
    runResultJobId.value = res.job_id
    runResult.value = t('admin.evaluationJobEnqueued', { jobId: res.job_id })
    showRunEval.value = false
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 409) {
      toast.add({ title: t('admin.datasetArchivedError'), color: 'error' })
    } else {
      toast.add({ title: apiErr.error || t('admin.runEvaluationFailed'), color: 'error' })
    }
  } finally {
    runLoading.value = false
  }
}

const datasetItems = computed<SelectItem[]>(() =>
  datasets.value
    .filter(d => d.status === 'active')
    .map(d => ({ label: d.name, value: d.id }))
)

const profileItems = computed<SelectItem[]>(() =>
  profiles.value.map(p => ({ label: p.name, value: p.id }))
)

/** 当前展开的数据集对象（供模板判断与渲染）。 */
const expandedDataset = computed(() =>
  datasets.value.find(d => d.id === expandedDatasetId.value) ?? null
)

// ---- 键盘导航：j/k 上下移动选中行，Enter/o 展开/收起条目面板，Escape 清除 ----
const selectedIndex = ref(-1)
const listEl = ref<HTMLElement | null>(null)

// 数据变化时清选中，避免指向已不存在的行。
watch(datasets, () => { selectedIndex.value = -1 })

function moveSelection(delta: number) {
  if (datasets.value.length === 0) return
  const next = selectedIndex.value < 0
    ? (delta > 0 ? 0 : datasets.value.length - 1)
    : Math.min(Math.max(selectedIndex.value + delta, 0), datasets.value.length - 1)
  selectedIndex.value = next
  listEl.value?.querySelectorAll('li')[next]?.scrollIntoView({ block: 'nearest' })
}

function openSelected() {
  const ds = datasets.value[selectedIndex.value]
  if (ds) toggleDataset(ds)
}

function clearSelection() {
  selectedIndex.value = -1
}

useHotkey('j', () => moveSelection(1))
useHotkey('k', () => moveSelection(-1))
useHotkey('enter', openSelected)
useHotkey('o', openSelected)
useHotkey('escape', clearSelection)

useHead({ title: () => t('admin.searchEvaluation') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-6xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.searchEvaluation') }}</h1>
        <div class="flex items-center gap-2">
          <USelect
            v-model="statusFilter"
            :items="statusFilterItems"
            value-key="value"
            label-key="label"
            size="sm"
            class="w-32"
            :placeholder="t('admin.filterByStatus')"
            @update:model-value="handleStatusFilterChange"
          />
          <UButton variant="outline" @click="showRunEval = true">{{ t('admin.runEvaluation') }}</UButton>
          <UButton icon="i-lucide-plus" @click="showCreateDataset = true">{{ t('admin.newDataset') }}</UButton>
        </div>
      </div>

      <EmptyState
        v-if="!isSystemAdmin"
        icon="i-lucide-lock"
        :title="t('admin.systemAdminRequired')"
      />

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />
        <div v-if="runResult" class="rounded-xl border border-success/20 bg-success/10 p-3 mb-4 text-sm text-success">
          {{ runResult }}
          <NuxtLink
            v-if="runResultJobId"
            to="/admin/jobs"
            class="ml-2 underline text-primary"
          >{{ t('admin.viewJob') }}</NuxtLink>
        </div>

        <div v-if="loading" class="flex justify-center py-8" role="status">
          <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-8 h-8 animate-spin text-muted" />
          <span class="sr-only">{{ t('common.loading') }}</span>
        </div>

        <ul ref="listEl" v-else-if="datasets.length > 0" class="space-y-3" role="list">
          <li
            v-for="(ds, idx) in datasets"
            :key="ds.id"
            class="border border-default rounded-xl p-4 transition-colors"
            :class="idx === selectedIndex ? 'ring-1 ring-primary/60 bg-elevated' : ''"
            :aria-selected="idx === selectedIndex"
          >
            <div class="flex items-start justify-between">
              <div>
                <div class="flex items-center gap-2">
                  <h3 class="font-medium text-highlighted" :class="{ 'text-muted': ds.status === 'archived' }">{{ ds.name }}</h3>
                  <UBadge
                    :color="ds.status === 'active' ? 'success' : 'neutral'"
                    variant="subtle"
                    size="sm"
                  >{{ datasetStatusLabel(ds.status) }}</UBadge>
                </div>
                <p class="text-xs text-muted mt-1">{{ ds.description }}</p>
              </div>
              <div class="flex gap-2">
                <UButton
                  size="xs"
                  variant="ghost"
                  :icon="expandedDatasetId === ds.id ? 'i-lucide-chevron-up' : 'i-lucide-chevron-down'"
                  :aria-label="expandedDatasetId === ds.id ? t('admin.collapseItems') : t('admin.expandItems')"
                  @click="toggleDataset(ds)"
                >{{ t('admin.datasetItems') }}</UButton>
                <UButton
                  size="xs"
                  variant="ghost"
                  icon="i-lucide-plus"
                  :disabled="ds.status === 'archived'"
                  :title="ds.status === 'archived' ? t('admin.datasetArchivedError') : undefined"
                  @click="selectedDataset = ds; showAddItem = true"
                >{{ t('admin.addItem') }}</UButton>
                <UButton
                  v-if="ds.status !== 'archived'"
                  size="xs"
                  variant="ghost"
                  color="warning"
                  icon="i-lucide-archive"
                  :aria-label="t('admin.archiveDataset')"
                  @click="requestDelete({ kind: 'dataset', datasetId: ds.id, label: ds.name })"
                />
              </div>
            </div>

            <!-- 展开：评测条目 + 评测结果 -->
            <div v-if="expandedDatasetId === ds.id" class="mt-3 border-t border-default pt-3">
              <!-- 条目列表 -->
              <div class="flex items-center justify-between mb-2">
                <h4 class="text-sm font-medium text-highlighted">{{ t('admin.datasetItems') }}</h4>
                <span class="text-xs text-muted">{{ itemsTotal }} {{ t('common.results') }}</span>
              </div>
              <div v-if="itemsLoading" class="flex justify-center py-4" role="status">
                <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-5 h-5 animate-spin text-muted" />
                <span class="sr-only">{{ t('common.loading') }}</span>
              </div>
              <ErrorDisplay v-else-if="itemsError" :message="itemsError" />
              <template v-else-if="items.length > 0">
                <ul class="space-y-2" role="list">
                  <li
                    v-for="item in items"
                    :key="item.id"
                    class="border border-default rounded p-2 text-xs"
                  >
                    <div class="flex items-start justify-between gap-2">
                      <div class="min-w-0">
                        <p class="font-medium text-highlighted truncate">{{ item.query }}</p>
                        <p class="text-muted mt-0.5">
                          {{ t('admin.relevanceGradeShort') }}: {{ item.relevance_grade }}
                          <template v-if="item.query_class"> · {{ item.query_class }}</template>
                        </p>
                        <p v-if="item.expected_documents.length > 0" class="text-dimmed mt-0.5 break-all">
                          {{ t('admin.expectedDocumentsShort') }}: {{ item.expected_documents.map(d => d.document_id).join(', ') }}
                        </p>
                      </div>
                      <UButton
                        size="xs"
                        variant="ghost"
                        color="error"
                        icon="i-lucide-trash-2"
                        :aria-label="t('admin.deleteItem')"
                        @click="requestDelete({ kind: 'item', datasetId: ds.id, itemId: item.id, label: item.query })"
                      />
                    </div>
                  </li>
                </ul>
                <Pagination
                  v-if="itemsTotal > itemsLimit"
                  :total="itemsTotal"
                  :limit="itemsLimit"
                  :offset="itemsOffset"
                  @update:offset="handleItemsOffsetChange"
                />
              </template>
              <EmptyState v-else icon="i-lucide-list" :title="t('admin.noItems')" />

              <!-- 评测结果 -->
              <div class="flex items-center justify-between mt-4 mb-2 border-t border-default pt-3">
                <h4 class="text-sm font-medium text-highlighted">{{ t('admin.evaluationResults') }}</h4>
                <span class="text-xs text-muted">{{ resultsTotal }} {{ t('common.results') }}</span>
              </div>
              <div v-if="resultsLoading" class="flex justify-center py-4" role="status">
                <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-5 h-5 animate-spin text-muted" />
                <span class="sr-only">{{ t('common.loading') }}</span>
              </div>
              <ErrorDisplay v-else-if="resultsError" :message="resultsError" />
              <template v-else-if="results.length > 0">
                <ul class="space-y-2" role="list">
                  <li
                    v-for="r in results"
                    :key="r.id"
                    class="border border-default rounded p-2 text-xs"
                  >
                    <div class="flex items-center justify-between gap-2">
                      <span class="text-muted">{{ formatDate(r.created_at) }}</span>
                      <NuxtLink
                        v-if="r.job_id"
                        to="/admin/jobs"
                        class="text-primary underline"
                      >{{ r.job_id.substring(0, 8) }}...</NuxtLink>
                    </div>
                    <pre class="mt-1 bg-elevated rounded p-2 overflow-x-auto">{{ JSON.stringify(r.metrics, null, 2) }}</pre>
                  </li>
                </ul>
              </template>
              <EmptyState v-else icon="i-lucide-bar-chart-3" :title="t('admin.noEvaluationResults')" />
            </div>
          </li>
        </ul>

        <EmptyState v-else icon="i-lucide-database" :title="t('admin.noDatasets')" />

        <Pagination
          v-if="datasetsTotal > limit"
          :total="datasetsTotal"
          :limit="limit"
          :offset="offset"
          @update:offset="handleOffsetChange"
        />
      </template>
    </div>

    <!-- Create Dataset Modal -->
    <UModal v-model:open="showCreateDataset">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.createDataset') }}</h3>
          <form @submit.prevent="handleCreateDataset" class="space-y-4">
            <UFormField :label="t('workspace.name')" name="name">
              <UInput v-model="datasetForm.name" class="w-full" />
            </UFormField>
            <UFormField :label="t('workspace.description')" name="description">
              <UTextarea v-model="datasetForm.description" class="w-full" />
            </UFormField>
            <div class="flex justify-end gap-2">
              <UButton color="neutral" variant="ghost" @click="showCreateDataset = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="datasetLoading">{{ t('common.create') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>

    <!-- Add Item Modal -->
    <UModal v-model:open="showAddItem">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.addItemTo', { name: selectedDataset?.name }) }}</h3>
          <form @submit.prevent="handleAddItem" class="space-y-4">
            <UFormField :label="t('admin.query')" name="query">
              <UInput v-model="itemForm.query" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.expectedDocuments')" name="expected_documents" :error="expectedDocsError ?? undefined">
              <UInput v-model="itemForm.expected_documents" placeholder="uuid1, uuid2" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.relevanceGrade')" name="relevance_grade">
              <UInput v-model.number="itemForm.relevance_grade" type="number" min="0" max="3" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.queryClass')" name="query_class">
              <UInput v-model="itemForm.query_class" class="w-full" />
            </UFormField>
            <ErrorDisplay v-if="itemError" :message="itemError" />
            <div class="flex justify-end gap-2">
              <UButton color="neutral" variant="ghost" @click="showAddItem = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="itemLoading">{{ t('common.add') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>

    <!-- Run Evaluation Modal -->
    <UModal v-model:open="showRunEval">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.runEvaluation') }}</h3>
          <form @submit.prevent="handleRunEval" class="space-y-4">
            <UFormField :label="t('admin.searchEvaluation')" name="dataset_id">
              <USelect v-model="runForm.dataset_id" :items="datasetItems" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.searchProfiles')" name="profile_id">
              <USelect v-model="runForm.profile_id" :items="profileItems" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <div class="flex justify-end gap-2">
              <UButton color="neutral" variant="ghost" @click="showRunEval = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="runLoading">{{ t('common.run') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>

    <!-- Delete/Archive Confirmation Modal（条目删除 / 数据集归档） -->
    <UModal :open="deleteTarget !== null" @update:open="(v) => { if (!v) deleteTarget = null }">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-2">
            {{ deleteTarget?.kind === 'dataset' ? t('admin.archiveDataset') : t('admin.deleteItem') }}
          </h3>
          <!-- 数据集归档是软删：保留全部数据但不可再写入，需明确提示 -->
          <p v-if="deleteTarget?.kind === 'dataset'" class="text-sm text-warning mb-1">
            {{ t('admin.archiveDatasetWarn') }}
          </p>
          <p class="text-sm text-muted mb-4">
            {{ deleteTarget?.kind === 'dataset'
              ? t('admin.archiveDatasetConfirm', { name: deleteTarget?.label })
              : t('admin.deleteItemConfirm', { query: deleteTarget?.label }) }}
          </p>
          <div class="flex justify-end gap-2">
            <UButton color="neutral" variant="ghost" @click="deleteTarget = null">{{ t('common.cancel') }}</UButton>
            <UButton
              :color="deleteTarget?.kind === 'dataset' ? 'warning' : 'error'"
              :loading="deleteLoading"
              @click="confirmDelete"
            >{{ deleteTarget?.kind === 'dataset' ? t('common.archive') : t('common.delete') }}</UButton>
          </div>
        </div>
      </template>
    </UModal>
  </div>
</template>
