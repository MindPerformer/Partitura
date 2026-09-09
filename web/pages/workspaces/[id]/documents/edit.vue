<!-- pages/workspaces/[id]/documents/edit.vue — Document Editor
//
// 引入动机：design/04-WEB-API.md §页面 要求 Document Editor。
// 支持 create/edit/replace/patch、source 操作、archive/restore（按 RBAC）、并发 409 处理。
//
// 编辑保存携带 expected_revision/expected_hash；409 显示明确冲突界面。
-->
<script setup lang="ts">
import type { Document, Source, ApiError, ReplaceDocumentRequest, CreateDocumentRequest, AddSourceRequest } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)
const editPath = computed(() => route.query.path as string | undefined)
const isEditMode = computed(() => !!editPath.value)

const { workspace, canEdit, canArchive, isOwner } = useWorkspaceContext(workspaceId)

// 表单状态
const form = reactive({
  path: '',
  title: '',
  type: '',
  content_markdown: ''
})

const currentDoc = ref<Document | null>(null)
const loading = ref(false)
const saving = ref(false)
const error = ref<string | null>(null)
const showConflict = ref(false)
const conflictMsg = ref('')

// Source 管理
const sources = ref<Source[]>([])
const showSourceModal = ref(false)
const sourceForm = reactive({
  source_type: 'web',
  value: '',
  title: '',
  retrieved_at: '',
  content_hash: '',
  refresh_interval_days: 0,
  source_document_id: ''
})

async function loadDocument() {
  if (!editPath.value || !workspaceId.value) return
  loading.value = true
  error.value = null

  try {
    const api = useDocumentApi()
    currentDoc.value = await api.read(workspaceId.value, editPath.value)
    form.path = currentDoc.value.path
    form.title = currentDoc.value.title
    form.type = currentDoc.value.type || ''
    form.content_markdown = currentDoc.value.content_markdown

    // 加载 sources
    const sourcesRes = await api.listSources(workspaceId.value, editPath.value, { limit: 50 })
    sources.value = sourcesRes.sources
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.loadFailed')
  } finally {
    loading.value = false
  }
}

watch(editPath, () => loadDocument(), { immediate: true })

async function handleSave() {
  if (!form.path || !form.title || !form.content_markdown) {
    error.value = t('document.pathTitleContentRequired')
    return
  }

  saving.value = true
  error.value = null

  try {
    const api = useDocumentApi()

    if (isEditMode.value && currentDoc.value) {
      // Replace (PUT)
      const req: ReplaceDocumentRequest = {
        title: form.title,
        type: form.type,
        content_markdown: form.content_markdown,
        expected_revision: currentDoc.value.revision_number,
        expected_hash: currentDoc.value.content_hash
      }
      const updated = await api.replace(workspaceId.value, editPath.value!, req)
      currentDoc.value = updated
      form.content_markdown = updated.content_markdown
    } else {
      // Create (POST)
      const req: CreateDocumentRequest = {
        path: form.path,
        title: form.title,
        type: form.type,
        content_markdown: form.content_markdown
      }
      const created = await api.create(workspaceId.value, req)
      // 导航到编辑模式
      navigateTo(`/workspaces/${workspaceId.value}/documents/edit?path=${encodeURIComponent(created.path)}`)
    }
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 409) {
      conflictMsg.value = apiErr.error || t('errors.versionConflictDesc')
      showConflict.value = true
    } else if (apiErr.status === 403) {
      error.value = t('document.noPermissionAction')
    } else if (apiErr.status === 404) {
      error.value = t('document.notFound')
    } else {
      error.value = apiErr.error || t('document.saveFailed')
    }
  } finally {
    saving.value = false
  }
}

async function handleConflictReload() {
  showConflict.value = false
  await loadDocument()
}

async function handleAddSource() {
  if (!sourceForm.value || !sourceForm.source_type) return
  try {
    const api = useDocumentApi()
    const req: AddSourceRequest = {
      source_type: sourceForm.source_type,
      value: sourceForm.value,
      title: sourceForm.title,
      retrieved_at: sourceForm.retrieved_at,
      content_hash: sourceForm.content_hash,
      refresh_interval_days: sourceForm.refresh_interval_days,
      source_document_id: sourceForm.source_document_id
    }
    const src = await api.addSource(workspaceId.value, editPath.value!, req)
    sources.value.push(src)
    showSourceModal.value = false
    sourceForm.value = ''
    sourceForm.title = ''
    sourceForm.retrieved_at = ''
    sourceForm.content_hash = ''
    sourceForm.refresh_interval_days = 0
    sourceForm.source_document_id = ''
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.addSourceFailed')
  }
}

async function handleDeleteSource(sourceId: string) {
  try {
    const api = useDocumentApi()
    await api.deleteSource(workspaceId.value, sourceId)
    sources.value = sources.value.filter(s => s.id !== sourceId)
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.deleteSourceFailed')
  }
}

// Archive/Restore
async function handleArchive() {
  if (!currentDoc.value) return
  try {
    const api = useDocumentApi()
    await api.archive(workspaceId.value, editPath.value!, {
      expected_revision: currentDoc.value.revision_number,
      expected_hash: currentDoc.value.content_hash
    })
    navigateTo(`/workspaces/${workspaceId.value}`)
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 409) {
      conflictMsg.value = apiErr.error
      showConflict.value = true
    } else {
      error.value = apiErr.error || t('document.archiveFailed')
    }
  }
}

const documentTypes = computed<SelectItem[]>(() => [
  { label: t('document.typeNone'), value: '' },
  { label: t('document.typeArchitecture'), value: 'architecture' },
  { label: t('document.typeCodebase'), value: 'codebase' },
  { label: t('document.typeDevelopment'), value: 'development' },
  { label: t('document.typeDecision'), value: 'decision' },
  { label: t('document.typeIssue'), value: 'issue' },
  { label: t('document.typeRoadmap'), value: 'roadmap' },
  { label: t('document.typeResearch'), value: 'research' },
  { label: t('document.typeReference'), value: 'reference' },
  { label: t('document.typeOperation'), value: 'operation' },
  { label: t('document.typeStandard'), value: 'standard' },
  { label: t('document.typeGuide'), value: 'guide' },
  { label: t('document.typeOther'), value: 'other' }
])

const sourceTypes = computed<SelectItem[]>(() => [
  { label: t('document.sourceTypeWeb'), value: 'web' },
  { label: t('document.sourceTypeCode'), value: 'code' },
  { label: t('document.sourceTypeFile'), value: 'file' },
  { label: t('document.sourceTypeIssue'), value: 'issue' },
  { label: t('document.sourceTypeCommit'), value: 'commit' },
  { label: t('document.sourceTypeConversation'), value: 'conversation' },
  { label: t('document.sourceTypeManual'), value: 'manual' },
  { label: t('document.sourceTypeOther'), value: 'other' }
])

useHead({ title: () => (isEditMode.value ? t('document.editDocument') : t('document.newDocument')) + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <template #default>
    <div class="max-w-4xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">
          {{ isEditMode ? t('document.editDocument') : t('document.newDocument') }}
        </h1>
        <UButton
          variant="ghost"
          icon="i-lucide-arrow-left"
          :to="editPath ? `/workspaces/${workspaceId}/documents/read?path=${encodeURIComponent(editPath)}` : `/workspaces/${workspaceId}`"
        >{{ t('common.back') }}</UButton>
      </div>

      <div v-if="loading" class="flex justify-center py-12">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <div v-else>
        <ErrorDisplay v-if="error" :message="error" class="mb-4" />

        <UCard>
          <form @submit.prevent="handleSave" class="space-y-4">
            <!-- Path (only for new docs) -->
            <UFormField v-if="!isEditMode" :label="t('document.path')" name="path" :hint="t('document.pathHint')">
              <UInput v-model="form.path" placeholder="architecture/overview.md" class="w-full" />
            </UFormField>
            <div v-else class="text-sm text-muted">
              <span class="font-medium">{{ t('document.path') }}:</span> {{ form.path }}
            </div>

            <UFormField :label="t('document.title')" name="title">
              <UInput v-model="form.title" class="w-full" />
            </UFormField>

            <UFormField :label="t('document.type')" name="type">
              <USelect v-model="form.type" :items="documentTypes" value-key="value" label-key="label" class="w-full" />
            </UFormField>

            <UFormField :label="t('document.content')" name="content_markdown">
              <UTextarea
                v-model="form.content_markdown"
                :rows="20"
                class="w-full font-mono text-sm"
                :placeholder="t('document.contentPlaceholder')"
              />
            </UFormField>

            <div v-if="isEditMode && currentDoc" class="text-xs text-muted flex items-center gap-3">
              <span>{{ t('document.revision') }}: #{{ currentDoc.revision_number }}</span>
              <span>{{ t('document.hash') }}: {{ currentDoc.content_hash.substring(0, 12) }}...</span>
            </div>

            <div class="flex items-center justify-between pt-4 border-t border-default">
              <div class="flex gap-2">
                <UButton
                  v-if="isEditMode && canArchive"
                  color="warning"
                  variant="ghost"
                  icon="i-lucide-archive"
                  @click="handleArchive"
                >{{ t('common.archive') }}</UButton>
              </div>
              <div class="flex gap-2">
                <UButton
                  type="button"
                  color="neutral"
                  variant="ghost"
                  :to="editPath ? `/workspaces/${workspaceId}/documents/read?path=${encodeURIComponent(editPath)}` : `/workspaces/${workspaceId}`"
                >{{ t('common.cancel') }}</UButton>
                <UButton type="submit" :loading="saving" :disabled="saving">{{ t('common.save') }}</UButton>
              </div>
            </div>
          </form>
        </UCard>

        <!-- Sources section (edit mode only) -->
        <UCard v-if="isEditMode" class="mt-6">
          <template #header>
            <div class="flex items-center justify-between">
              <h2 class="font-semibold">{{ t('document.sources') }}</h2>
              <UButton
                v-if="canEdit"
                size="xs"
                variant="ghost"
                icon="i-lucide-plus"
                @click="showSourceModal = true"
              >{{ t('document.addSource') }}</UButton>
            </div>
          </template>

          <div v-if="sources.length > 0" class="space-y-2">
            <div
              v-for="src in sources"
              :key="src.id"
              class="flex items-start justify-between border border-default rounded p-3"
            >
              <div class="flex-1">
                <div class="flex items-center gap-2 mb-1">
                  <UBadge variant="subtle" size="xs">{{ src.source_type }}</UBadge>
                  <span v-if="src.title" class="text-sm font-medium">{{ src.title }}</span>
                </div>
                <p class="text-sm text-muted break-all">{{ src.value }}</p>
              </div>
              <UButton
                v-if="canArchive"
                size="xs"
                variant="ghost"
                color="error"
                icon="i-lucide-trash-2"
                @click="handleDeleteSource(src.id)"
              />
            </div>
          </div>
          <p v-else class="text-sm text-muted py-2">{{ t('document.noSources') }}</p>
        </UCard>
      </div>
    </div>

    <!-- Conflict Dialog -->
    <ConflictDialog
      :open="showConflict"
      :message="conflictMsg"
      @reload="handleConflictReload"
      @cancel="showConflict = false"
    />

    <!-- Add Source Modal -->
    <UModal v-model:open="showSourceModal">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('document.addSource') }}</h3>
          <form @submit.prevent="handleAddSource" class="space-y-4">
            <UFormField :label="t('document.sourceType')" name="source_type">
              <USelect v-model="sourceForm.source_type" :items="sourceTypes" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <UFormField :label="t('document.sourceValue')" name="value">
              <UInput v-model="sourceForm.value" placeholder="https://..." class="w-full" />
            </UFormField>
            <UFormField :label="t('document.sourceTitleOptional')" name="title">
              <UInput v-model="sourceForm.title" class="w-full" />
            </UFormField>
            <UFormField :label="t('document.retrievedAtOptional')" name="retrieved_at">
              <UInput v-model="sourceForm.retrieved_at" type="datetime-local" class="w-full" />
            </UFormField>
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="showSourceModal = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit">{{ t('common.add') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>
    </template>
  </WorkspaceLayout>
</template>
