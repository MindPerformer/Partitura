<!-- pages/workspaces/[id]/documents/edit.vue — Document Editor
//
// 引入动机：design/04-WEB-API.md §页面 要求 Document Editor。
// 支持 create/edit/replace/patch、source 操作、archive/restore（按 RBAC）、并发 409 处理。
//
// 编辑保存携带 expected_revision/expected_hash；409 显示明确冲突界面。
// 未保存修改通过 onBeforeRouteLeave + beforeunload + localStorage 草稿三层防护。
-->
<script setup lang="ts">
import type { Document, Source, ApiError, ReplaceDocumentRequest, CreateDocumentRequest, AddSourceRequest } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToast()
const workspaceId = computed(() => route.params.id as string)
const editPath = computed(() => route.query.path as string | undefined)
const isEditMode = computed(() => !!editPath.value)

const { workspace, canEdit, canArchive } = useWorkspaceContext(workspaceId)

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
// 字段级校验错误（value/retrieved_at），非法时通过 UFormField :error 展示、不发请求。
const sourceErrors = reactive<{ value?: string; retrieved_at?: string }>({})

// ============================ 未保存修改追踪 ============================
// dirty 规则：
// - 编辑态：title/type/content_markdown 与已加载文档不一致
// - 创建态：path/title/type/content_markdown 任一非空
// 提交成功后重置基准，离开不再拦截。
const isDirty = computed(() => {
  if (isEditMode.value && currentDoc.value) {
    return form.title !== currentDoc.value.title
      || form.type !== (currentDoc.value.type || '')
      || form.content_markdown !== currentDoc.value.content_markdown
  }
  return !!(form.path || form.title || form.type || form.content_markdown)
})

// localStorage 草稿：key 含 workspaceId + path（新建为 __new__），
// 加载时若存在草稿提示恢复，保存成功后清除。
const draftKey = computed(() => `docDraft:${workspaceId.value}:${editPath.value || '__new__'}`)
const draftAvailable = ref(false)

function saveDraft() {
  if (!isDirty.value) return
  try {
    localStorage.setItem(draftKey.value, JSON.stringify({
      path: form.path,
      title: form.title,
      type: form.type,
      content_markdown: form.content_markdown
    }))
  } catch (e) {
    // 写入失败（如存储配额）不阻断编辑，仅记录日志。
    console.error('[edit.vue] 草稿写入失败', e)
  }
}

let draftTimer: ReturnType<typeof setTimeout> | undefined
watch(form, () => {
  if (draftTimer) clearTimeout(draftTimer)
  draftTimer = setTimeout(saveDraft, 800)
}, { deep: true })

function clearDraft() {
  try {
    localStorage.removeItem(draftKey.value)
  } catch (e) {
    // 清理失败不阻断流程，仅记录日志（如隐私模式禁止写 storage）。
    console.error('[edit.vue] 草稿清理失败', e)
  }
  draftAvailable.value = false
}

function restoreDraft() {
  try {
    const raw = localStorage.getItem(draftKey.value)
    if (!raw) return
    const draft = JSON.parse(raw) as Partial<typeof form>
    form.path = draft.path ?? form.path
    form.title = draft.title ?? form.title
    form.type = draft.type ?? form.type
    form.content_markdown = draft.content_markdown ?? form.content_markdown
    toast.add({ title: t('document.draftRestored'), color: 'success' })
  } catch (e) {
    console.error('[edit.vue] 草稿恢复失败', e)
  }
  draftAvailable.value = false
}

function checkDraft() {
  try {
    const raw = localStorage.getItem(draftKey.value)
    draftAvailable.value = !!raw
  } catch (e) {
    console.error('[edit.vue] 草稿检测失败', e)
    draftAvailable.value = false
  }
}

// ============================ 文档加载 ============================
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

watch(editPath, () => {
  loadDocument()
  checkDraft()
}, { immediate: true })

// ============================ 保存 ============================
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
      clearDraft()
      toast.add({ title: t('document.saveSuccess'), color: 'success' })
    } else {
      // Create (POST)
      const req: CreateDocumentRequest = {
        path: form.path,
        title: form.title,
        type: form.type,
        content_markdown: form.content_markdown
      }
      const created = await api.create(workspaceId.value, req)
      clearDraft()
      toast.add({ title: t('document.createSuccess'), color: 'success' })
      // 创建成功后导航到编辑模式；跳过离开守卫——内容已持久化不算未保存。
      suppressLeaveGuard = true
      await router.replace(`/workspaces/${workspaceId.value}/documents/edit?path=${encodeURIComponent(created.path)}`)
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

// ============================ 409 冲突处理 ============================
// 三种路径由 ConflictDialog 触发：
// - reload：拉取最新版本覆盖本地表单（本地未保存修改被丢弃，对话框文案已明示）；
// - force：以本地内容为基准，先重新 read 获取最新 revision/hash 后再 replace；
// - cancel：仅关闭对话框，本地草稿保留在表单。
async function handleConflictReload() {
  showConflict.value = false
  clearDraft()
  await loadDocument()
}

async function handleConflictForce() {
  if (!editPath.value) return
  showConflict.value = false
  saving.value = true
  error.value = null

  // 本地内容兜底：强制覆盖前先保存当前表单内容到草稿，
  // 即便后续 replace 失败用户也不会丢失编辑成果。
  saveDraft()

  try {
    const api = useDocumentApi()
    // 强制覆盖 = 读取最新版本号/hash，再提交本地内容（后端不支持裸 force 参数）。
    const latest = await api.read(workspaceId.value, editPath.value)
    const req: ReplaceDocumentRequest = {
      title: form.title,
      type: form.type,
      content_markdown: form.content_markdown,
      expected_revision: latest.revision_number,
      expected_hash: latest.content_hash
    }
    const updated = await api.replace(workspaceId.value, editPath.value, req)
    currentDoc.value = updated
    form.content_markdown = updated.content_markdown
    clearDraft()
    toast.add({ title: t('document.saveSuccess'), color: 'success' })
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 409) {
      conflictMsg.value = apiErr.error || t('errors.versionConflictDesc')
      showConflict.value = true
    } else {
      error.value = apiErr.error || t('document.saveFailed')
    }
  } finally {
    saving.value = false
  }
}

// ============================ Source 操作 ============================
function validateSourceForm(): boolean {
  sourceErrors.value = undefined
  sourceErrors.retrieved_at = undefined

  const value = sourceForm.value.trim()
  if (!value) {
    sourceErrors.value = t('document.sourceValueRequired')
  } else if (sourceForm.source_type === 'web') {
    try {
      const url = new URL(value)
      if (url.protocol !== 'http:' && url.protocol !== 'https:') {
        sourceErrors.value = t('document.sourceValueInvalidUrl')
      }
    } catch {
      sourceErrors.value = t('document.sourceValueInvalidUrl')
    }
  }

  if (sourceForm.retrieved_at) {
    const d = new Date(sourceForm.retrieved_at)
    if (Number.isNaN(d.getTime())) {
      sourceErrors.retrieved_at = t('document.retrievedAtInvalid')
    }
  }

  return !sourceErrors.value && !sourceErrors.retrieved_at
}

async function handleAddSource() {
  if (!editPath.value) return
  if (!validateSourceForm()) return

  try {
    const api = useDocumentApi()
    const req: AddSourceRequest = {
      source_type: sourceForm.source_type,
      value: sourceForm.value.trim(),
      title: sourceForm.title,
      // datetime-local 值无秒/时区，提交前转 ISO 字符串。
      retrieved_at: sourceForm.retrieved_at ? new Date(sourceForm.retrieved_at).toISOString() : '',
      content_hash: sourceForm.content_hash,
      refresh_interval_days: sourceForm.refresh_interval_days,
      source_document_id: sourceForm.source_document_id
    }
    const src = await api.addSource(workspaceId.value, editPath.value, req)
    sources.value.push(src)
    showSourceModal.value = false
    resetSourceForm()
    toast.add({ title: t('document.sourceAdded'), color: 'success' })
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.addSourceFailed')
  }
}

function resetSourceForm() {
  sourceForm.source_type = 'web'
  sourceForm.value = ''
  sourceForm.title = ''
  sourceForm.retrieved_at = ''
  sourceForm.content_hash = ''
  sourceForm.refresh_interval_days = 0
  sourceForm.source_document_id = ''
  sourceErrors.value = undefined
  sourceErrors.retrieved_at = undefined
}

async function handleDeleteSource(sourceId: string) {
  try {
    const api = useDocumentApi()
    await api.deleteSource(workspaceId.value, sourceId)
    sources.value = sources.value.filter(s => s.id !== sourceId)
    toast.add({ title: t('document.sourceDeleted'), color: 'success' })
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.deleteSourceFailed')
  }
}

// ============================ 归档 ============================
async function handleArchive() {
  if (!currentDoc.value) return
  try {
    const api = useDocumentApi()
    await api.archive(workspaceId.value, editPath.value!, {
      expected_revision: currentDoc.value.revision_number,
      expected_hash: currentDoc.value.content_hash
    })
    toast.add({ title: t('document.archived'), color: 'success' })
    clearDraft()
    // 归档后跳离编辑页——用户已明确归档意图，未保存守卫不应再拦截。
    suppressLeaveGuard = true
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

// ============================ 离开保护 ============================
const showLeaveGuard = ref(false)
let pendingLeave: (() => void) | null = null
// 程序化导航（创建成功后的 router.replace）不应被未保存守卫拦截。
let suppressLeaveGuard = false

onBeforeRouteLeave((_to, _from, next) => {
  if (suppressLeaveGuard || !isDirty.value) {
    next()
    return
  }
  showLeaveGuard.value = true
  pendingLeave = () => next()
  next(false)
})

function confirmLeave() {
  showLeaveGuard.value = false
  const go = pendingLeave
  pendingLeave = null
  go?.()
}

function cancelLeave() {
  showLeaveGuard.value = false
  pendingLeave = null
}

// 刷新/关闭标签页时若仍有未保存修改，阻止默认行为弹出浏览器原生提示。
function onBeforeUnload(e: BeforeUnloadEvent) {
  if (isDirty.value) {
    e.preventDefault()
    e.returnValue = ''
  }
}

onMounted(() => {
  window.addEventListener('beforeunload', onBeforeUnload)
})

onBeforeUnmount(() => {
  window.removeEventListener('beforeunload', onBeforeUnload)
  if (draftTimer) clearTimeout(draftTimer)
  if (pendingLeave) pendingLeave = null
})

// ============================ 编辑/预览切换 ============================
// 编辑区上方提供 tab：编辑（现有 UTextarea）/ 预览（MarkdownRenderer）。
// mod+e 或点击切换；预览态 textarea 隐藏。MarkdownRenderer 已做 DOMPurify 净化。
const viewTab = ref<'edit' | 'preview'>('edit')

function toggleViewTab() {
  viewTab.value = viewTab.value === 'edit' ? 'preview' : 'edit'
}

// ============================ 页面级快捷键 ============================
// mod+s 保存（preventDefault 已由 useHotkey 处理，这里调用 handleSave）；
// mod+enter 提交（与 mod+s 等价，统一"显式提交"心智）；
// mod+e 切换编辑/预览；escape 返回上一页（编辑态回 read，新建态回 workspace）。
// 模态（冲突/离开守卫/来源）打开时所有快捷键让位给 UModal 原生处理。
const modalOpen = computed(() => showConflict.value || showLeaveGuard.value || showSourceModal.value)
useHotkey('mod+s', () => { if (!modalOpen.value) handleSave() }, { allowInInput: true })
useHotkey('mod+enter', () => { if (!modalOpen.value) handleSave() }, { allowInInput: true })
useHotkey('mod+e', () => { if (!modalOpen.value) toggleViewTab() }, { allowInInput: true })
useHotkey('escape', () => {
  if (modalOpen.value) return
  const target = editPath.value
    ? `/workspaces/${workspaceId.value}/documents/read?path=${encodeURIComponent(editPath.value)}`
    : `/workspaces/${workspaceId.value}`
  // 走 router.push 而非 navigateTo：未保存守卫由 onBeforeRouteLeave 统一拦截，
  // 避免 escape 在 dirty 时静默失效。
  router.push(target)
})

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

        <!-- 本地草稿恢复提示 -->
        <div v-if="draftAvailable" class="mb-4 flex items-center justify-between rounded-lg border border-info/30 bg-info/10 p-3">
          <p class="text-sm text-info">{{ t('document.draftFound') }}</p>
          <div class="flex gap-2">
            <UButton size="xs" variant="ghost" color="neutral" @click="clearDraft">{{ t('common.cancel') }}</UButton>
            <UButton size="xs" color="info" @click="restoreDraft">{{ t('document.restoreDraft') }}</UButton>
          </div>
        </div>

        <UCard>
          <form @submit.prevent="handleSave" class="space-y-4">
            <!-- Path (only for new docs) -->
            <UFormField v-if="!isEditMode" :label="t('document.path')" name="path" :hint="t('document.pathHint')">
              <UInput v-model="form.path" placeholder="architecture/overview.md" class="w-full" />
            </UFormField>
            <div v-else class="text-sm">
              <span class="font-medium text-muted">{{ t('document.path') }}:</span>
              <DocBreadcrumb :workspace-id="workspaceId" :path="form.path" class="mt-1" />
            </div>

            <UFormField :label="t('document.title')" name="title">
              <UInput v-model="form.title" class="w-full" />
            </UFormField>

            <UFormField :label="t('document.type')" name="type">
              <USelect v-model="form.type" :items="documentTypes" value-key="value" label-key="label" class="w-full" />
            </UFormField>

            <!-- 编辑/预览切换：保留 UFormField 提供 label/error/id 关联；
                 tab 分段控件放 label 行右侧（label slot 渲染 labelWrapper 内）。 -->
            <UFormField name="content_markdown">
              <template #label>
                <span class="flex w-full items-center justify-between">
                  <span>{{ t('document.content') }}</span>
                  <span class="flex items-center gap-1" role="tablist" :aria-label="t('document.content')">
                    <UButton
                      size="xs"
                      :variant="viewTab === 'edit' ? 'solid' : 'ghost'"
                      role="tab"
                      id="doc-tab-edit"
                      aria-controls="doc-panel-edit"
                      :aria-selected="viewTab === 'edit'"
                      @click="viewTab = 'edit'"
                    >{{ t('document.editTab') }}</UButton>
                    <UButton
                      size="xs"
                      :variant="viewTab === 'preview' ? 'solid' : 'ghost'"
                      role="tab"
                      id="doc-tab-preview"
                      aria-controls="doc-panel-preview"
                      :aria-selected="viewTab === 'preview'"
                      @click="viewTab = 'preview'"
                    >{{ t('document.previewTab') }}</UButton>
                  </span>
                </span>
              </template>

              <div
                v-show="viewTab === 'edit'"
                id="doc-panel-edit"
                role="tabpanel"
                aria-labelledby="doc-tab-edit"
              >
                <UTextarea
                  v-model="form.content_markdown"
                  :rows="20"
                  class="w-full font-mono text-sm"
                  :placeholder="t('document.contentPlaceholder')"
                />
              </div>
              <div
                v-show="viewTab === 'preview'"
                id="doc-panel-preview"
                role="tabpanel"
                aria-labelledby="doc-tab-preview"
                class="min-h-80 rounded-lg border border-default bg-elevated/30 p-4"
              >
                <MarkdownRenderer :content="form.content_markdown" />
              </div>
            </UFormField>

            <div v-if="isEditMode && currentDoc" class="text-xs text-muted flex items-center gap-3">
              <span>{{ t('document.revision') }}: #{{ currentDoc.revision_number }}</span>
              <span :title="currentDoc.content_hash">{{ t('document.hash') }}: {{ currentDoc.content_hash.substring(0, 12) }}...</span>
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
                @click="resetSourceForm(); showSourceModal = true"
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
                  <UBadge variant="subtle" size="sm">{{ src.source_type }}</UBadge>
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
                :aria-label="t('common.delete')"
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
      @force="handleConflictForce"
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
            <UFormField :label="t('document.sourceValue')" name="value" :error="sourceErrors.value" required>
              <UInput v-model="sourceForm.value" placeholder="https://..." class="w-full" />
            </UFormField>
            <UFormField :label="t('document.sourceTitleOptional')" name="title">
              <UInput v-model="sourceForm.title" class="w-full" />
            </UFormField>
            <UFormField :label="t('document.retrievedAtOptional')" name="retrieved_at" :error="sourceErrors.retrieved_at">
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

    <!-- 未保存离开确认 -->
    <UModal v-model:open="showLeaveGuard" :dismissible="false">
      <template #content>
        <div class="p-6">
          <div class="flex items-start gap-3 mb-4">
            <UIcon name="i-lucide-triangle-alert" class="w-6 h-6 text-warning flex-shrink-0" />
            <div>
              <h3 class="text-lg font-semibold text-highlighted">{{ t('document.unsavedChangesTitle') }}</h3>
              <p class="text-sm text-muted mt-1">{{ t('document.unsavedChangesDesc') }}</p>
            </div>
          </div>
          <div class="flex justify-end gap-2 mt-6">
            <UButton color="neutral" variant="ghost" @click="cancelLeave">{{ t('document.keepEditing') }}</UButton>
            <UButton color="error" @click="confirmLeave">{{ t('document.discardAndLeave') }}</UButton>
          </div>
        </div>
      </template>
    </UModal>
    </template>
  </WorkspaceLayout>
</template>
