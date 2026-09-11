<!-- pages/workspaces/[id]/search.vue — Search (仅当前 workspace)
//
// 引入动机：design/04-WEB-API.md §Search 要求 Web search 只搜索当前打开的 Workspace。
// 支持 mode/结果定位/degraded 状态。
-->
<script setup lang="ts">
import type { SearchResponse, SearchResult, ApiError } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const workspaceId = computed(() => route.params.id as string)

const { workspace } = useWorkspaceContext(workspaceId)

const query = ref((route.query.q as string) || '')
const mode = ref<'hybrid' | 'lexical' | 'semantic'>('hybrid')
const results = ref<SearchResult[]>([])
const total = ref(0)
const limit = ref(10)
const offset = ref(0)
const degraded = ref(false)
const degradationReason = ref('')
const searchId = ref('')
const rerankerUsed = ref(false)
const loading = ref(false)
const error = ref<string | null>(null)
const hasSearched = ref(false)

async function performSearch() {
  if (!query.value.trim() || !workspaceId.value) return
  loading.value = true
  error.value = null
  hasSearched.value = true

  // 搜索词写回 URL ?q=，便于刷新/分享后恢复同一查询。
  router.replace({ query: { ...route.query, q: query.value.trim() } })

  try {
    const api = useSearchApi()
    const res = await api.search(workspaceId.value, {
      query: query.value.trim(),
      mode: mode.value,
      limit: limit.value,
      offset: offset.value
    })
    results.value = res.results || []
    total.value = res.total
    degraded.value = res.degraded
    degradationReason.value = res.degradation_reason || ''
    searchId.value = res.search_id
    rerankerUsed.value = res.reranker_used
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('search.searchFailed')
    results.value = []
  } finally {
    loading.value = false
  }
}

// 从 URL query 初始化搜索
watch(() => route.query.q, (q) => {
  if (q && q !== query.value) {
    query.value = q as string
    performSearch()
  }
}, { immediate: true })

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  performSearch()
}

function navigateToResult(result: SearchResult) {
  navigateTo(`/workspaces/${workspaceId.value}/documents/read?path=${encodeURIComponent(result.path)}`)
}

// ============================ 页面级快捷键 ============================
// escape：清空 query + 结果并回焦输入框。
// - 输入框内 Escape：由 UInput 的 @keydown.escape 直接处理（useHotkey 对无修饰单键
//   在输入框中默认抑制，但这里我们希望在输入态也能 esc 清空）。
// - 非输入态 Escape：走 useHotkey（此时 focus 不在输入框，esc 才有意义）。
// UInput 通过 defineExpose 暴露内部 inputRef；autofocus prop 负责初始聚焦。
const searchInputRef = ref<{ inputRef?: HTMLInputElement | null } | null>(null)

function focusSearchInput() {
  searchInputRef.value?.inputRef?.focus()
}

function clearSearch() {
  query.value = ''
  results.value = []
  total.value = 0
  hasSearched.value = false
  degraded.value = false
  degradationReason.value = ''
  error.value = null
  // 清除 URL 中的 ?q=，避免刷新后恢复旧查询。
  router.replace({ query: { ...route.query, q: undefined } })
  focusSearchInput()
}

useHotkey('escape', () => {
  // 仅当有可清内容时才消费 esc；空态下让位给浏览器/其它 esc 语义（如关模态）。
  if (query.value || hasSearched.value) clearSearch()
})

// j/k 结果导航 + enter/o 打开：选中项用 selectedIndex 跟踪并滚动到可视区。
// 单键在输入框聚焦时由 useHotkey 内置规则自动抑制，不干扰打字。
const selectedIndex = ref(-1)

watch(results, () => { selectedIndex.value = -1 })

function moveSelection(delta: number) {
  if (results.value.length === 0) return
  const next = selectedIndex.value + delta
  selectedIndex.value = Math.max(0, Math.min(results.value.length - 1, next))
  // 滚动选中项进入可视区
  nextTick(() => {
    document.querySelectorAll<HTMLElement>('[data-search-result]')[selectedIndex.value]
      ?.scrollIntoView({ block: 'nearest' })
  })
}

function openSelected() {
  const r = results.value[selectedIndex.value]
  if (r) navigateToResult(r)
}

useHotkey('j', () => moveSelection(1))
useHotkey('k', () => moveSelection(-1))
useHotkey('enter', openSelected)
useHotkey('o', openSelected)

const modeOptions = computed<SelectItem[]>(() => [
  { label: t('search.modeHybrid'), value: 'hybrid' },
  { label: t('search.modeLexical'), value: 'lexical' },
  { label: t('search.modeSemantic'), value: 'semantic' }
])

useHead({ title: () => t('search.title') + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <div class="max-w-4xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('search.title') }}</h1>

      <!-- Search bar -->
      <div class="flex gap-2 mb-4">
        <UInput
          ref="searchInputRef"
          v-model="query"
          :placeholder="t('search.searchInWorkspace')"
          class="flex-1"
          icon="i-lucide-search"
          autofocus
          @keyup.enter="performSearch"
          @keydown.escape="clearSearch"
        />
        <USelect
          v-model="mode"
          :items="modeOptions"
          value-key="value"
          label-key="label"
          class="w-32"
        />
        <UButton @click="performSearch" :loading="loading">{{ t('common.search') }}</UButton>
      </div>

      <p class="text-xs text-muted mb-4">{{ t('search.searchingIn', { name: workspace?.display_name || '...' }) }}</p>

      <ErrorDisplay v-if="error" :message="error" />

      <!-- Degraded warning -->
      <div v-if="degraded" class="rounded-lg border border-warning/20 bg-warning/10 p-3 mb-4">
        <div class="flex items-center gap-2 text-sm text-warning">
          <UIcon name="i-lucide-triangle-alert" class="w-4 h-4" />
          <span>{{ t('search.degraded') }}: {{ degradationReason }}</span>
        </div>
      </div>

      <!-- Results -->
      <div v-if="loading" class="flex justify-center py-8">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <!-- 初始引导态：尚未发起任何搜索时给操作指引 -->
      <EmptyState
        v-if="!hasSearched && !loading"
        icon="i-lucide-search"
        :title="t('search.hint')"
        :description="t('search.hintModes')"
      />

      <EmptyState
        v-else-if="hasSearched && results.length === 0 && !error"
        icon="i-lucide-search-x"
        :title="t('search.noResults')"
      />

      <div v-else-if="results.length > 0" class="space-y-3">
        <div class="text-sm text-muted mb-2">
          {{ t('search.resultsCount', { total, mode: rerankerUsed ? t('search.reranked') : t('search.rrfOnly') }) }}
        </div>
        <div
          v-for="(result, i) in results"
          :key="result.document_id + result.rank"
          data-search-result
          class="border rounded-lg p-4 hover:border-primary/50 cursor-pointer transition-colors"
          :class="i === selectedIndex ? 'border-primary/60 bg-elevated ring-1 ring-primary/40' : 'border-default'"
          :aria-selected="i === selectedIndex"
          @click="navigateToResult(result)"
        >
          <div class="flex items-start justify-between mb-1">
            <h3 class="font-medium text-highlighted">{{ result.title }}</h3>
            <span class="text-xs text-muted">{{ t('search.score') }}: {{ result.score.toFixed(4) }}</span>
          </div>
          <p class="text-xs text-muted mb-2">{{ result.path }} · {{ t('search.lines', { start: result.start_line, end: result.end_line }) }}</p>
          <p class="text-sm text-muted line-clamp-3">{{ result.snippet }}</p>
          <div v-if="result.section_path && result.section_path.length > 0" class="flex items-center gap-1 mt-2">
            <UBadge
              v-for="section in result.section_path"
              :key="section"
              variant="subtle"
              size="sm"
            >{{ section }}</UBadge>
          </div>
        </div>

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="handleOffsetChange"
        />
      </div>
    </div>
  </WorkspaceLayout>
</template>
