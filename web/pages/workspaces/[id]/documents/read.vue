<!-- pages/workspaces/[id]/documents/read.vue — Document Reader
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 VitePress/GitBook 风格 Document Reader。
// Topbar workspace/search/user, Sidebar document tree, Main Markdown, Right ToC/metadata/sources/revision。
-->
<script setup lang="ts">
import type { Document, Source, ApiError } from '~/types/api'
import type { MarkdownHeading } from '~/utils/markdown'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToast()
const workspaceId = computed(() => route.params.id as string)
const docPath = computed(() => route.query.path as string)

const { canEdit, canArchive } = useWorkspaceContext(workspaceId)
const { documentTypeLabel } = useEnumLabels()
const { formatDate: formatDateUtil } = useFormatDate()

// 文档内容
const doc = ref<Document | null>(null)
const headings = ref<MarkdownHeading[]>([])
const sources = ref<Source[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

async function loadDocument() {
  if (!workspaceId.value) return
  // path 缺失属非法访问：显示明确错误态而非静默空白，并给返回入口。
  if (!docPath.value) {
    error.value = t('document.pathMissing')
    loading.value = false
    doc.value = null
    return
  }
  loading.value = true
  error.value = null
  doc.value = null
  headings.value = []
  sources.value = []

  try {
    const api = useDocumentApi()
    const [docRes, sourcesRes] = await Promise.all([
      api.read(workspaceId.value, docPath.value),
      api.listSources(workspaceId.value, docPath.value, { limit: 50 }).catch(() => ({ sources: [], total: 0, limit: 50, offset: 0 }))
    ])
    doc.value = docRes
    sources.value = sourcesRes.sources
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 404) {
      error.value = t('document.notFound')
    } else if (apiErr.status === 403) {
      error.value = t('document.noPermission')
    } else {
      error.value = apiErr.error || t('document.loadFailed')
    }
  } finally {
    loading.value = false
  }
}

watch([workspaceId, docPath], () => loadDocument(), { immediate: true })

function handleHeadings(value: MarkdownHeading[]) {
  headings.value = value
}

function handleTocClick(id: string) {
  const target = document.getElementById(id)
  if (!target) {
    console.error(`[read.vue] 目录目标不存在: ${id}`)
    return
  }
  target.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

function handleMobileTocClick(id: string) {
  handleTocClick(id)
  mobilePanelOpen.value = false
}

function handleSelectDoc(path: string) {
  navigateTo(`/workspaces/${workspaceId.value}/documents/read?path=${encodeURIComponent(path)}`)
}

const mobilePanelOpen = ref(false)
// 窄屏头部菜单打开态：escape 快捷键需让位给菜单自身的 Esc 关闭行为。
const headerMenuOpen = ref(false)

function formatDate(s: string): string {
  return formatDateUtil(s)
}

// 归档/恢复
const showArchiveConfirm = ref(false)

// 窄屏（<sm）下页头按钮组折叠为下拉菜单；宽屏保持平铺按钮。
// ToC 项仅在窄屏出现（桌面右栏已有 ToC），触发打开移动端面板。
const headerMenuItems = computed(() => {
  const items: any[] = []
  if (canEdit.value) {
    items.push({
      label: t('common.edit'),
      icon: 'i-lucide-pencil',
      to: `/workspaces/${workspaceId.value}/documents/edit?path=${encodeURIComponent(docPath.value!)}`
    })
  }
  items.push({
    label: t('document.history'),
    icon: 'i-lucide-clock',
    to: `/workspaces/${workspaceId.value}/documents/history?path=${encodeURIComponent(docPath.value!)}`
  })
  items.push({
    label: t('document.tableOfContents'),
    icon: 'i-lucide-list',
    onSelect: () => { mobilePanelOpen.value = true }
  })
  if (canArchive.value && doc.value?.status !== 'archived') {
    items.push({
      label: t('common.archive'),
      icon: 'i-lucide-archive',
      color: 'warning',
      onSelect: () => { showArchiveConfirm.value = true }
    })
  }
  return items
})

// ============================ 页面级快捷键 ============================
// e 进入编辑（仅 canEdit）、h 查看历史、escape 返回 workspace 首页。
// 单键在无修饰键时触发；输入框聚焦时自动抑制（useHotkey 内置规则）。
// 模态/菜单/抽屉打开时让位给各组件自身的 Esc 处理，避免误触发导航。
const overlayOpen = computed(() => showArchiveConfirm.value || mobilePanelOpen.value || headerMenuOpen.value)
useHotkey('e', () => {
  if (!canEdit.value || !docPath.value) return
  if (overlayOpen.value) return
  navigateTo(`/workspaces/${workspaceId.value}/documents/edit?path=${encodeURIComponent(docPath.value)}`)
})
useHotkey('h', () => {
  if (!docPath.value) return
  if (overlayOpen.value) return
  navigateTo(`/workspaces/${workspaceId.value}/documents/history?path=${encodeURIComponent(docPath.value)}`)
})
useHotkey('escape', () => {
  if (overlayOpen.value) return
  navigateTo(`/workspaces/${workspaceId.value}`)
})

async function handleArchive() {
  if (!doc.value) return
  try {
    const api = useDocumentApi()
    await api.archive(workspaceId.value, docPath.value!, {
      expected_revision: doc.value.revision_number,
      expected_hash: doc.value.content_hash
    })
    showArchiveConfirm.value = false
    toast.add({ title: t('document.archived'), color: 'success' })
    navigateTo(`/workspaces/${workspaceId.value}`)
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.archiveFailed')
  }
}

useHead({ title: () => (doc.value?.title || t('document.documents')) + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <div class="flex min-h-0 min-w-0 h-full overflow-hidden">
      <!-- Main content -->
      <div class="min-h-0 min-w-0 flex-1 overflow-y-auto overflow-x-hidden">
        <div v-if="loading" class="flex justify-center py-12">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <ErrorDisplay v-else-if="error" :message="error" />

        <div v-else-if="doc" class="mx-auto min-w-0 max-w-3xl px-6 py-8">
          <!-- Document header -->
          <div class="mb-6 pb-4 border-b border-default">
            <div class="flex items-start justify-between gap-2">
              <div class="min-w-0">
                <!-- 页面级 h1：文档标题是此页唯一一级标题；
                     markdown 正文中的 h1 位于 <article> 内，属于内容结构。 -->
                <h1 class="text-2xl font-bold text-highlighted break-words">{{ doc.title }}</h1>
                <DocBreadcrumb :workspace-id="workspaceId" :path="doc.path" class="mt-1" />
              </div>
              <!-- 宽屏（sm+）平铺按钮组 -->
              <div class="hidden sm:flex flex-wrap items-center justify-end gap-1">
                <!-- ToC 入口：sm–lg 区间右栏（DocumentSidePanel）尚未显示（lg:block），
                     需此按钮打开移动端 USlideover ToC；lg+ 右栏出现则隐藏按钮。 -->
                <UButton
                  class="lg:hidden"
                  size="xs"
                  variant="ghost"
                  icon="i-lucide-list"
                  :aria-label="t('document.tableOfContents')"
                  @click="mobilePanelOpen = true"
                >{{ t('document.tableOfContents') }}</UButton>
                <UButton
                  v-if="canEdit"
                  size="xs"
                  variant="ghost"
                  icon="i-lucide-pencil"
                  :to="`/workspaces/${workspaceId}/documents/edit?path=${encodeURIComponent(docPath!)}`"
                >{{ t('common.edit') }}</UButton>
                <UButton
                  size="xs"
                  variant="ghost"
                  icon="i-lucide-clock"
                  :to="`/workspaces/${workspaceId}/documents/history?path=${encodeURIComponent(docPath!)}`"
                >{{ t('document.history') }}</UButton>
                <UButton
                  v-if="canArchive && doc.status !== 'archived'"
                  size="xs"
                  variant="ghost"
                  color="warning"
                  icon="i-lucide-archive"
                  @click="showArchiveConfirm = true"
                >{{ t('common.archive') }}</UButton>
              </div>
              <!-- 窄屏（<sm）折叠为下拉菜单 -->
              <UDropdownMenu
                v-model:open="headerMenuOpen"
                class="sm:hidden"
                :items="headerMenuItems"
                :content="{ align: 'end' }"
              >
                <UButton
                  size="sm"
                  variant="outline"
                  icon="i-lucide-ellipsis-vertical"
                  :aria-label="t('common.actions')"
                />
              </UDropdownMenu>
            </div>
            <div class="flex items-center gap-3 mt-2 text-xs text-muted">
              <span>{{ t('document.revision') }} {{ doc.revision_number }}</span>
              <span>·</span>
              <span>{{ t('document.updated') }} {{ formatDate(doc.updated_at) }}</span>
              <UBadge v-if="doc.is_special" variant="subtle" size="sm" color="info">{{ t('document.special') }}</UBadge>
              <UBadge v-if="doc.type" variant="subtle" size="sm">{{ documentTypeLabel(doc.type) }}</UBadge>
            </div>
          </div>

          <!-- Markdown content -->
          <MarkdownRenderer :content="doc.content_markdown" @headings="handleHeadings" />
        </div>
      </div>

      <!-- Desktop right panel -->
      <aside v-if="doc" class="hidden h-full w-64 min-h-0 flex-shrink-0 overflow-hidden border-l border-default bg-default lg:block">
        <DocumentSidePanel
          :doc="doc"
          :headings="headings"
          :sources="sources"
          :workspace-id="workspaceId"
          :doc-path="docPath"
          :can-edit="canEdit"
          @toc-click="handleTocClick"
        />
      </aside>

      <!-- Mobile right panel -->
      <USlideover
        v-if="doc"
        v-model:open="mobilePanelOpen"
        side="right"
        :title="t('document.tableOfContents')"
        :ui="{ content: 'w-[min(24rem,calc(100vw-2rem))]' }"
      >
        <template #body>
          <DocumentSidePanel
            :doc="doc"
            :headings="headings"
            :sources="sources"
            :workspace-id="workspaceId"
            :doc-path="docPath"
            :can-edit="canEdit"
            @toc-click="handleMobileTocClick"
          />
        </template>
      </USlideover>
    </div>

    <!-- Archive confirm -->
    <UModal v-model:open="showArchiveConfirm">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-2">{{ t('document.archiveDocument') }}</h3>
          <p class="text-sm text-muted mb-4">{{ t('document.archiveConfirm', { title: doc?.title }) }}</p>
          <div class="flex justify-end gap-2">
            <UButton color="neutral" variant="ghost" @click="showArchiveConfirm = false">{{ t('common.cancel') }}</UButton>
            <UButton color="warning" @click="handleArchive">{{ t('common.archive') }}</UButton>
          </div>
        </div>
      </template>
    </UModal>
  </WorkspaceLayout>
</template>
