<!-- pages/workspaces/[id]/documents/read.vue — Document Reader
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 VitePress/GitBook 风格 Document Reader。
// Topbar workspace/search/user, Sidebar document tree, Main Markdown, Right ToC/metadata/sources/revision。
-->
<script setup lang="ts">
import type { Document, OutlineResponse, Heading, Source, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)
const docPath = computed(() => route.query.path as string)

const { workspace, canEdit, canArchive, currentMemberRole, isOwner } = useWorkspaceContext(workspaceId)
const { formatDate: formatDateUtil } = useFormatDate()

// 文档内容
const doc = ref<Document | null>(null)
const outline = ref<Heading[]>([])
const sources = ref<Source[]>([])
const loading = ref(false)
const error = ref<string | null>(null)

async function loadDocument() {
  if (!workspaceId.value || !docPath.value) return
  loading.value = true
  error.value = null
  doc.value = null
  outline.value = []
  sources.value = []

  try {
    const api = useDocumentApi()
    const [docRes, outlineRes, sourcesRes] = await Promise.all([
      api.read(workspaceId.value, docPath.value),
      api.outline(workspaceId.value, docPath.value),
      api.listSources(workspaceId.value, docPath.value, { limit: 50 }).catch(() => ({ sources: [], total: 0, limit: 50, offset: 0 }))
    ])
    doc.value = docRes
    outline.value = outlineRes.outline
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

function handleSelectDoc(path: string) {
  navigateTo(`/workspaces/${workspaceId.value}/documents/read?path=${encodeURIComponent(path)}`)
}

// 右侧面板标签
const rightTab = ref<'toc' | 'meta' | 'sources'>('toc')

function formatDate(s: string): string {
  return formatDateUtil(s)
}

// 归档/恢复
const showArchiveConfirm = ref(false)

async function handleArchive() {
  if (!doc.value) return
  try {
    const api = useDocumentApi()
    await api.archive(workspaceId.value, docPath.value!, {
      expected_revision: doc.value.revision_number,
      expected_hash: doc.value.content_hash
    })
    showArchiveConfirm.value = false
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
    <div class="flex h-full overflow-hidden">
      <!-- Main content -->
      <div class="flex-1 overflow-y-auto">
        <div v-if="loading" class="flex justify-center py-12">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <ErrorDisplay v-else-if="error" :message="error" />

        <div v-else-if="doc" class="max-w-4xl mx-auto px-6 py-8">
          <!-- Document header -->
          <div class="mb-6 pb-4 border-b border-default">
            <div class="flex items-start justify-between">
              <div>
                <h1 class="text-2xl font-bold text-highlighted">{{ doc.title }}</h1>
                <p class="text-sm text-muted mt-1">{{ doc.path }}</p>
              </div>
              <div class="flex items-center gap-1">
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
            </div>
            <div class="flex items-center gap-3 mt-2 text-xs text-muted">
              <span>{{ t('document.revision') }} {{ doc.revision_number }}</span>
              <span>·</span>
              <span>{{ t('document.updated') }} {{ formatDate(doc.updated_at) }}</span>
              <UBadge v-if="doc.is_special" variant="subtle" size="xs" color="info">{{ t('document.special') }}</UBadge>
              <UBadge v-if="doc.type" variant="subtle" size="xs">{{ doc.type }}</UBadge>
            </div>
          </div>

          <!-- Markdown content -->
          <MarkdownRenderer :content="doc.content_markdown" />
        </div>
      </div>

      <!-- Right panel: ToC / Metadata / Sources -->
      <aside v-if="doc" class="w-64 flex-shrink-0 border-l border-default bg-default overflow-y-auto hidden lg:block">
        <div class="px-3 py-2 border-b border-default">
          <div class="flex gap-1">
            <UButton size="xs" :variant="rightTab === 'toc' ? 'solid' : 'ghost'" @click="rightTab = 'toc'">{{ t('document.tableOfContents') }}</UButton>
            <UButton size="xs" :variant="rightTab === 'meta' ? 'solid' : 'ghost'" @click="rightTab = 'meta'">{{ t('document.info') }}</UButton>
            <UButton size="xs" :variant="rightTab === 'sources' ? 'solid' : 'ghost'" @click="rightTab = 'sources'">{{ t('document.sources') }}</UButton>
          </div>
        </div>

        <!-- ToC -->
        <div v-if="rightTab === 'toc'" class="p-3">
          <ul v-if="outline.length > 0" class="space-y-1 text-sm">
            <li
              v-for="heading in outline"
              :key="heading.start_line"
              :style="{ paddingLeft: `${(heading.level - 1) * 12 + 4}px` }"
              class="text-muted hover:text-primary cursor-pointer truncate"
            >
              {{ heading.text }}
            </li>
          </ul>
          <p v-else class="text-xs text-muted">{{ t('document.noHeadings') }}</p>
        </div>

        <!-- Metadata -->
        <div v-if="rightTab === 'meta'" class="p-3 space-y-2 text-sm">
          <div>
            <p class="text-xs text-muted uppercase">{{ t('document.path') }}</p>
            <p class="text-default break-all">{{ doc.path }}</p>
          </div>
          <div>
            <p class="text-xs text-muted uppercase">{{ t('document.type') }}</p>
            <p class="text-default">{{ doc.type || 'N/A' }}</p>
          </div>
          <div>
            <p class="text-xs text-muted uppercase">{{ t('workspace.status') }}</p>
            <p class="text-default">{{ doc.status }}</p>
          </div>
          <div>
            <p class="text-xs text-muted uppercase">{{ t('document.revision') }}</p>
            <p class="text-default">#{{ doc.revision_number }}</p>
          </div>
          <div>
            <p class="text-xs text-muted uppercase">{{ t('document.hash') }}</p>
            <p class="text-default font-mono text-xs break-all">{{ doc.content_hash.substring(0, 16) }}...</p>
          </div>
          <div>
            <p class="text-xs text-muted uppercase">{{ t('document.created') }}</p>
            <p class="text-default">{{ formatDate(doc.created_at) }}</p>
          </div>
          <div>
            <p class="text-xs text-muted uppercase">{{ t('document.updated') }}</p>
            <p class="text-default">{{ formatDate(doc.updated_at) }}</p>
          </div>
        </div>

        <!-- Sources -->
        <div v-if="rightTab === 'sources'" class="p-3">
          <div v-if="sources.length > 0" class="space-y-2">
            <div
              v-for="src in sources"
              :key="src.id"
              class="text-sm border border-default rounded p-2"
            >
              <div class="flex items-center gap-1 mb-1">
                <UBadge variant="subtle" size="xs">{{ src.source_type }}</UBadge>
              </div>
              <p class="text-default break-all text-xs">{{ src.value }}</p>
              <p v-if="src.title" class="text-muted text-xs mt-1">{{ src.title }}</p>
            </div>
          </div>
          <p v-else class="text-xs text-muted">{{ t('document.noSources') }}</p>
          <UButton
            v-if="canEdit"
            size="xs"
            variant="ghost"
            block
            class="mt-3"
            :to="`/workspaces/${workspaceId}/documents/edit?path=${encodeURIComponent(docPath!)}&tab=sources`"
          >{{ t('document.addSource') }}</UButton>
        </div>
      </aside>
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
