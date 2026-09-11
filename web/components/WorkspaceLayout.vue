<!-- WorkspaceLayout.vue — workspace 上下文布局
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 VitePress/GitBook 风格布局。
// Topbar(workspace/search/user) + Sidebar(document tree) + Main content。
// 在 workspace scoped 页面中使用此布局。
-->
<script setup lang="ts">
import type { DocumentListItem } from '~/types/api'

const route = useRoute()
const workspaceId = computed(() => route.params.id as string)

const { workspace, currentMemberRole, loading, error, canEdit, canRead } = useWorkspaceContext(workspaceId)

const { t } = useI18n()

// 加载文档列表：支持分页加载全部数据或继续分页，错误可见。
const documents = ref<DocumentListItem[]>([])
const docsLoading = ref(false)
const docsError = ref<string | null>(null)
const docsHasMore = ref(false)

// 文档列表分页上限：防止后端 total 异常导致 while(true) 死循环。
// 50 页 * 100/页 = 5000 条文档已远超一般 workspace 规模。
const MAX_DOC_PAGES = 50

async function loadDocuments() {
  if (!workspaceId.value || !workspace.value) {
    documents.value = []
    docsError.value = null
    docsHasMore.value = false
    return
  }

  docsLoading.value = true
  docsError.value = null
  documents.value = []
  docsHasMore.value = false

  try {
    const api = useDocumentApi()
    const limit = 100
    let offset = 0
    const all: DocumentListItem[] = []
    let total = 0
    let pages = 0

    // 循环分页直至返回数量达到 total，或一次返回为空时安全停止；
    // MAX_DOC_PAGES 作为后端 total 异常时的硬性熔断上限。
    while (pages < MAX_DOC_PAGES) {
      const res = await api.list(workspaceId.value, { limit, offset })
      total = res.total
      all.push(...res.documents)
      pages++
      if (all.length >= res.total || res.documents.length === 0) {
        break
      }
      offset += res.limit
    }

    documents.value = all
    docsHasMore.value = all.length < total
  } catch (err) {
    const apiErr = err as { error?: string; status?: number }
    docsError.value = apiErr.error || t('document.loadFailed')
    if (import.meta.dev) {
      console.error('[WorkspaceLayout] 加载文档列表失败', err)
    }
    documents.value = []
    docsHasMore.value = false
  } finally {
    docsLoading.value = false
  }
}

// 「加载更多」：在已加载列表末尾继续分页拉取剩余文档，直到 total 或上限。
async function loadMoreDocuments() {
  if (!workspaceId.value || !workspace.value || !docsHasMore.value) return

  docsLoading.value = true
  docsError.value = null

  try {
    const api = useDocumentApi()
    const limit = 100
    // 从当前已加载数量继续，避免重复拉取。
    let offset = documents.value.length
    const all = [...documents.value]
    let total = all.length
    let pages = 0

    while (pages < MAX_DOC_PAGES) {
      const res = await api.list(workspaceId.value, { limit, offset })
      total = res.total
      all.push(...res.documents)
      pages++
      if (all.length >= res.total || res.documents.length === 0) {
        break
      }
      offset += res.limit
    }

    documents.value = all
    docsHasMore.value = all.length < total
  } catch (err) {
    const apiErr = err as { error?: string; status?: number }
    docsError.value = apiErr.error || t('document.loadFailed')
    if (import.meta.dev) {
      console.error('[WorkspaceLayout] 加载更多文档失败', err)
    }
  } finally {
    docsLoading.value = false
  }
}

watch(workspace, () => {
  loadDocuments()
}, { immediate: true })

const sidebarOpen = ref(true)
const mobileSidebarOpen = ref(false)

function toggleSidebar() {
  sidebarOpen.value = !sidebarOpen.value
}

// ---------------------------------------------------------------------------
// 侧栏宽度拖拽调宽（展开态）
// - 宽度持久化到 localStorage（key: pkw_sidebar_w），范围 200–480px。
// - 拖拽 handle 为键盘可达的 role="separator"，方向键 ←/→ 步进 16px。
// ---------------------------------------------------------------------------
const SIDEBAR_MIN_W = 200
const SIDEBAR_MAX_W = 480
const SIDEBAR_DEFAULT_W = 256
const SIDEBAR_KEY_STEP = 16
const SIDEBAR_LS_KEY = 'pkw_sidebar_w'

const sidebarWidth = ref(SIDEBAR_DEFAULT_W)

function clampSidebarWidth(w: number): number {
  return Math.min(SIDEBAR_MAX_W, Math.max(SIDEBAR_MIN_W, Math.round(w)))
}

function persistSidebarWidth() {
  try {
    localStorage.setItem(SIDEBAR_LS_KEY, String(sidebarWidth.value))
  } catch (err) {
    // localStorage 可能因隐私模式/配额失败，仅记录不中断交互。
    if (import.meta.dev) {
      console.warn('[WorkspaceLayout] 侧栏宽度持久化失败', err)
    }
  }
}

onMounted(() => {
  try {
    const raw = localStorage.getItem(SIDEBAR_LS_KEY)
    if (raw !== null) {
      const n = parseInt(raw, 10)
      if (Number.isFinite(n)) sidebarWidth.value = clampSidebarWidth(n)
    }
  } catch (err) {
    if (import.meta.dev) {
      console.warn('[WorkspaceLayout] 读取侧栏宽度失败', err)
    }
  }
})

// 拖拽状态：记录起始 x 与起始宽度，move 时按 delta 更新。
const resizing = ref(false)
let resizeStartX = 0
let resizeStartW = 0

function onResizePointerDown(e: PointerEvent) {
  if (!sidebarOpen.value) return
  resizing.value = true
  resizeStartX = e.clientX
  resizeStartW = sidebarWidth.value
  window.addEventListener('pointermove', onResizePointerMove)
  window.addEventListener('pointerup', onResizePointerUp, { once: true })
  // 防止拖拽过程中选中文本
  e.preventDefault()
}

function onResizePointerMove(e: PointerEvent) {
  if (!resizing.value) return
  const delta = e.clientX - resizeStartX
  sidebarWidth.value = clampSidebarWidth(resizeStartW + delta)
}

function onResizePointerUp() {
  resizing.value = false
  window.removeEventListener('pointermove', onResizePointerMove)
  persistSidebarWidth()
}

function onResizeKeydown(e: KeyboardEvent) {
  if (!sidebarOpen.value) return
  if (e.key === 'ArrowLeft') {
    sidebarWidth.value = clampSidebarWidth(sidebarWidth.value - SIDEBAR_KEY_STEP)
    persistSidebarWidth()
    e.preventDefault()
  } else if (e.key === 'ArrowRight') {
    sidebarWidth.value = clampSidebarWidth(sidebarWidth.value + SIDEBAR_KEY_STEP)
    persistSidebarWidth()
    e.preventDefault()
  }
}

onBeforeUnmount(() => {
  window.removeEventListener('pointermove', onResizePointerMove)
  window.removeEventListener('pointerup', onResizePointerUp)
})

// 收起态 rail：聚焦顶栏搜索框（与 CommandPalette 的 `/` 行为一致）。
function focusHeaderSearch() {
  if (typeof document === 'undefined') return
  const input = document.querySelector<HTMLElement>('header input')
  input?.focus()
}

function handleSelectDoc(path: string) {
  mobileSidebarOpen.value = false
  navigateTo(`/workspaces/${workspaceId.value}/documents/read?path=${encodeURIComponent(path)}`)
}
</script>

<template>
  <div class="flex h-screen min-h-0 flex-col overflow-hidden">
    <AppHeader
      :workspace-id="workspaceId"
      :workspace-name="workspace?.display_name"
    >
      <template #leading>
        <UButton
          class="md:hidden"
          color="neutral"
          variant="ghost"
          icon="i-lucide-menu"
          :aria-label="t('workspace.openSidebar')"
          @click="mobileSidebarOpen = true"
        />
      </template>
    </AppHeader>

    <!-- Workspace loading -->
    <div v-if="loading" class="flex-1 flex items-center justify-center">
      <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
    </div>

    <!-- Workspace error -->
    <div v-else-if="error" class="flex-1 flex items-center justify-center p-8">
      <ErrorDisplay :message="error" :title="t('workspace.workspaceInaccessible')" />
    </div>

    <!-- Workspace content -->
    <div v-else-if="workspace" class="flex min-h-0 min-w-0 flex-1 overflow-hidden">
      <!-- Desktop sidebar：展开态为可拖宽文档树（贴屏幕左边框），收起态为 ~48px 图标 rail -->
      <aside
        v-if="sidebarOpen"
        class="relative hidden h-full min-h-0 flex-shrink-0 overflow-hidden border-r border-default bg-default md:flex md:flex-col"
        :style="{ width: `${sidebarWidth}px` }"
        :aria-label="t('document.documents')"
      >
        <WorkspaceSidebar
          class="min-h-0 flex-1"
          :workspace-id="workspaceId"
          :documents="documents"
          :current-path="route.query.path as string"
          :can-edit="canEdit"
          @select="handleSelectDoc"
          @collapse="toggleSidebar"
        />
        <div class="shrink-0 border-t border-default px-3 py-2">
          <ErrorDisplay v-if="docsError" :message="docsError" />
          <div v-if="docsLoading" class="flex items-center justify-center py-4">
            <UIcon name="i-lucide-loader-circle" class="h-4 w-4 animate-spin text-muted" />
          </div>
          <UButton
            v-else-if="docsHasMore"
            size="xs"
            variant="ghost"
            block
            icon="i-lucide-chevron-down"
            @click="loadMoreDocuments"
          >{{ t('common.loadMore') }}</UButton>
        </div>

        <!-- 拖拽调宽 handle：键盘可达（role=separator + 方向键），鼠标/触控按住拖动 -->
        <div
          role="separator"
          aria-orientation="vertical"
          tabindex="0"
          :aria-label="t('workspace.resizeSidebar')"
          :aria-valuenow="sidebarWidth"
          :aria-valuemin="SIDEBAR_MIN_W"
          :aria-valuemax="SIDEBAR_MAX_W"
          class="absolute inset-y-0 right-0 w-1.5 cursor-col-resize touch-none select-none outline-none transition-colors hover:bg-primary/30 focus-visible:bg-primary/40"
          :class="resizing ? 'bg-primary/40' : 'bg-transparent'"
          @pointerdown="onResizePointerDown"
          @keydown="onResizeKeydown"
        />
      </aside>

      <!-- 收起态：贴边图标 rail -->
      <nav
        v-else
        class="hidden h-full w-12 flex-shrink-0 flex-col items-center gap-1 border-r border-default bg-default py-2 md:flex"
        :aria-label="t('workspace.sidebarRail')"
      >
        <UTooltip :text="t('workspace.expandSidebar')">
          <UButton
            color="neutral"
            variant="ghost"
            size="sm"
            icon="i-lucide-panel-right-open"
            :aria-label="t('workspace.expandSidebar')"
            :title="t('workspace.expandSidebar')"
            @click="toggleSidebar"
          />
        </UTooltip>
        <UTooltip :text="t('workspace.goWorkspaceHome')">
          <UButton
            color="neutral"
            variant="ghost"
            size="sm"
            icon="i-lucide-home"
            :to="`/workspaces/${workspaceId}`"
            :aria-label="t('workspace.goWorkspaceHome')"
            :title="t('workspace.goWorkspaceHome')"
          />
        </UTooltip>
        <UTooltip v-if="canEdit" :text="t('document.newDocument')">
          <UButton
            color="neutral"
            variant="ghost"
            size="sm"
            icon="i-lucide-plus"
            :to="`/workspaces/${workspaceId}/documents/edit`"
            :aria-label="t('document.newDocument')"
            :title="t('document.newDocument')"
          />
        </UTooltip>
        <UTooltip :text="t('workspace.focusSearch')">
          <UButton
            color="neutral"
            variant="ghost"
            size="sm"
            icon="i-lucide-search"
            :aria-label="t('workspace.focusSearch')"
            :title="t('workspace.focusSearch')"
            @click="focusHeaderSearch"
          />
        </UTooltip>
      </nav>

      <!-- 主内容区：外层 layouts/default.vue 已提供 <main id="main-content">，
           此处用 div + role="main" 避免双 main 地标（skip-link 仍锚定外层）。 -->
      <div role="main" class="min-h-0 min-w-0 flex-1 overflow-y-auto overflow-x-hidden">
        <div class="min-w-0">
          <slot :workspace="workspace" :can-edit="canEdit" :can-read="canRead" :current-role="currentMemberRole" />
        </div>
      </div>
    </div>

    <!-- Fallback -->
    <div v-else class="flex-1 flex items-center justify-center">
      <p class="text-muted">{{ t('workspace.workspaceNotFound') }}</p>
    </div>

    <!-- Mobile sidebar drawer -->
    <USlideover
      v-model:open="mobileSidebarOpen"
      side="left"
      :title="workspace?.display_name || t('workspace.home')"
      :ui="{ content: 'w-[min(20rem,calc(100vw-2rem))]' }"
    >
      <template #body>
        <WorkspaceSidebar
          :workspace-id="workspaceId"
          :documents="documents"
          :current-path="route.query.path as string"
          :can-edit="canEdit"
          @select="handleSelectDoc"
        />
        <div class="border-t border-default px-3 py-2">
          <ErrorDisplay v-if="docsError" :message="docsError" />
          <div v-if="docsLoading" class="flex items-center justify-center py-4">
            <UIcon name="i-lucide-loader-circle" class="h-4 w-4 animate-spin text-muted" />
          </div>
          <UButton
            v-else-if="docsHasMore"
            size="xs"
            variant="ghost"
            block
            icon="i-lucide-chevron-down"
            @click="loadMoreDocuments"
          >{{ t('common.loadMore') }}</UButton>
        </div>
      </template>
    </USlideover>
  </div>
</template>
