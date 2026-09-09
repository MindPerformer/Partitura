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

    // 循环分页直至返回数量达到 total，或一次返回为空时安全停止。
    while (true) {
      const res = await api.list(workspaceId.value, { limit, offset })
      total = res.total
      all.push(...res.documents)
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

watch(workspace, () => {
  loadDocuments()
}, { immediate: true })

const sidebarOpen = ref(true)

function toggleSidebar() {
  sidebarOpen.value = !sidebarOpen.value
}

function handleSelectDoc(path: string) {
  navigateTo(`/workspaces/${workspaceId.value}/documents/read?path=${encodeURIComponent(path)}`)
}
</script>

<template>
  <div class="min-h-screen flex flex-col">
    <AppHeader
      :workspace-id="workspaceId"
      :workspace-name="workspace?.display_name"
    />

    <!-- Workspace loading -->
    <div v-if="loading" class="flex-1 flex items-center justify-center">
      <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
    </div>

    <!-- Workspace error -->
    <div v-else-if="error" class="flex-1 flex items-center justify-center p-8">
      <ErrorDisplay :message="error" :title="t('workspace.workspaceInaccessible')" />
    </div>

    <!-- Workspace content -->
    <div v-else-if="workspace" class="flex-1 flex overflow-hidden">
      <!-- Sidebar toggle button -->
      <button
        class="flex-shrink-0 w-6 flex items-center justify-center border-r border-default bg-default hover:bg-elevated transition-colors"
        :aria-label="sidebarOpen ? t('workspace.collapseSidebar') : t('workspace.expandSidebar')"
        @click="toggleSidebar"
      >
        <UIcon :name="sidebarOpen ? 'i-lucide-panel-left-close' : 'i-lucide-panel-left-open'" class="w-4 h-4 text-muted" />
      </button>
      <!-- Sidebar -->
      <aside
        v-if="sidebarOpen"
        class="w-64 flex-shrink-0 border-r border-default bg-default overflow-y-auto"
      >
        <!-- 主页 / 统计 固定入口 -->
        <nav class="px-3 py-2 border-b border-default space-y-1">
          <NuxtLink
            :to="`/workspaces/${workspaceId}`"
            class="flex items-center gap-2 px-2 py-1.5 rounded-md text-sm transition-colors"
            :class="route.path === `/workspaces/${workspaceId}` ? 'bg-primary/10 text-primary font-medium' : 'text-default hover:bg-elevated'"
          >
            <UIcon name="i-lucide-home" class="w-4 h-4" />
            {{ t('workspace.home') }}
          </NuxtLink>
          <NuxtLink
            :to="`/workspaces/${workspaceId}`"
            class="flex items-center gap-2 px-2 py-1.5 rounded-md text-sm transition-colors"
            :class="route.path === `/workspaces/${workspaceId}` ? 'bg-primary/10 text-primary font-medium' : 'text-default hover:bg-elevated'"
          >
            <UIcon name="i-lucide-bar-chart-3" class="w-4 h-4" />
            {{ t('workspace.stats') }}
          </NuxtLink>
        </nav>

        <!-- 文档树 -->
        <div class="px-3 py-2 border-b border-default flex items-center justify-between">
          <span class="text-xs font-medium text-muted uppercase">{{ t('document.documents') }}</span>
          <UButton
            v-if="canEdit"
            size="xs"
            variant="ghost"
            icon="i-lucide-plus"
            :to="`/workspaces/${workspaceId}/documents/edit`"
          />
        </div>
        <div class="px-3 py-2">
          <ErrorDisplay v-if="docsError" :message="docsError" />
          <div v-if="docsLoading" class="flex items-center justify-center py-4">
            <UIcon name="i-lucide-loader-circle" class="w-4 h-4 animate-spin text-muted" />
          </div>
        </div>
        <AppSidebar
          :documents="documents"
          :workspace-id="workspaceId"
          :current-path="route.query.path as string"
          @select="handleSelectDoc"
        />
      </aside>

      <!-- Main content -->
      <main class="flex-1 overflow-y-auto">
        <slot :workspace="workspace" :can-edit="canEdit" :can-read="canRead" :current-role="currentMemberRole" />
      </main>
    </div>

    <!-- Fallback -->
    <div v-else class="flex-1 flex items-center justify-center">
      <p class="text-muted">{{ t('workspace.workspaceNotFound') }}</p>
    </div>
  </div>
</template>
