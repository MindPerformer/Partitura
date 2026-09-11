<!-- pages/admin/search-profiles.vue — Search Profiles
//
// 引入动机：design/04-WEB-API.md §页面 要求 Search Profiles 页面。
// 仅 system_admin 可访问。支持列表、创建、激活、回滚。
//
// 修改/删除的落地方式（后端不支持原地修改与物理删除）：
// - 修改 = 基于现有 profile 新建版本（draft，需激活后生效）
// - 删除 = 归档（保留记录，可通过回滚重新激活）
-->
<script setup lang="ts">
import type { SearchProfile, ApiError, CreateProfileRequest, CreateProfileVersionRequest } from '~/types/api'

definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { profileStatusLabel } = useEnumLabels()

const {
  profiles,
  total,
  loading,
  error,
  load
} = useSearchProfiles()

const limit = ref(20)
const offset = ref(0)

const showCreateModal = ref(false)
const createLoading = ref(false)
const createError = ref<string | null>(null)

const defaultForm: CreateProfileRequest = {
  name: '',
  embedding_provider: 'openai-compatible',
  embedding_model: 'qwen3-embedding',
  embedding_dimensions: 1024,
  embedding_query_instruction: '',
  embedding_document_instruction: '',
  chunk_target_size: 512,
  chunk_overlap: 64,
  title_boost: 2.0,
  heading_boost: 1.5,
  path_boost: 1.0,
  tags_boost: 1.0,
  body_boost: 1.0,
  analyzer: 'standard',
  lexical_top_k: 50,
  vector_top_k: 50,
  rrf_k: 60,
  reranker_provider: 'openai-compatible',
  reranker_model: 'qwen3-reranker',
  reranker_candidate_count: 100,
  reranker_final_count: 20,
  max_chunks_per_document: 5,
  merge_adjacent_chunks: true,
  max_p95_latency_ms: 500,
  max_reranker_cost_per_query: 0.01
}
const createForm = ref<CreateProfileRequest>({ ...defaultForm })

/**
 * 新建版本弹窗的表单类型。
 *
 * 引入动机：后端 createProfileVersionRequest 接受除 version/status/es_index_name/created_by
 * 之外的全部可调参数覆盖，因此弹窗与创建弹窗一样暴露完整的 25 个字段，
 * 表单类型直接复用 CreateProfileRequest，保证两端字段集合不会漂移。
 */
type ProfileVersionForm = CreateProfileRequest

const showVersionModal = ref(false)
const versionLoading = ref(false)
const versionError = ref<string | null>(null)
const versionNotice = ref<string | null>(null)
/** 新建版本时被选中的源 profile：表单预填来自它的现有值，而非 defaultForm 的硬编码默认值。 */
const versionSource = ref<SearchProfile | null>(null)
const versionForm = ref<ProfileVersionForm | null>(null)
/** 正在归档的 profile id，用于按钮 loading 状态。 */
const archiveLoadingId = ref<string | null>(null)

/**
 * 以源 profile 的现有值构造新建版本表单。
 *
 * 必须显式逐字段取用：SearchProfile 还带有 version/status/es_index_name/created_by 等
 * 由服务端管理的元数据字段，直接展开会把它们带进表单并最终发给后端（后端严格解码会 400）。
 * 这里也正好是"哪些字段可被用户覆盖"的唯一定义处。
 */
function versionFormFromProfile(profile: SearchProfile): ProfileVersionForm {
  return {
    name: profile.name,
    embedding_provider: profile.embedding_provider,
    embedding_model: profile.embedding_model,
    embedding_dimensions: profile.embedding_dimensions,
    embedding_query_instruction: profile.embedding_query_instruction,
    embedding_document_instruction: profile.embedding_document_instruction,
    chunk_target_size: profile.chunk_target_size,
    chunk_overlap: profile.chunk_overlap,
    title_boost: profile.title_boost,
    heading_boost: profile.heading_boost,
    path_boost: profile.path_boost,
    tags_boost: profile.tags_boost,
    body_boost: profile.body_boost,
    analyzer: profile.analyzer,
    lexical_top_k: profile.lexical_top_k,
    vector_top_k: profile.vector_top_k,
    rrf_k: profile.rrf_k,
    reranker_provider: profile.reranker_provider,
    reranker_model: profile.reranker_model,
    reranker_candidate_count: profile.reranker_candidate_count,
    reranker_final_count: profile.reranker_final_count,
    max_chunks_per_document: profile.max_chunks_per_document,
    merge_adjacent_chunks: profile.merge_adjacent_chunks,
    max_p95_latency_ms: profile.max_p95_latency_ms,
    max_reranker_cost_per_query: profile.max_reranker_cost_per_query
  }
}

/**
 * 构造新建版本的请求体。
 *
 * 引入动机：弹窗暴露全部可调参数，等价于把源配置整份覆盖提交（name + 24 个参数）。
 * 这里直接展开表单，而不是再逐字段抄一遍：表单类型 CreateProfileRequest 的字段集合
 * 与后端 createProfileVersionRequest 接受的覆盖集合完全一致（version/status/
 * es_index_name/created_by 都不在类型里），且表单只可能由 versionFormFromProfile 构造，
 * 因此展开既不会遗漏字段，也不可能多塞后端拒绝的字段。
 *
 * 服务端仍以"未提供的字段继承源 profile"为语义，整份提交不会改变任何继承行为。
 */
function buildVersionPayload(form: ProfileVersionForm): CreateProfileVersionRequest {
  return { ...form }
}

/**
 * 打开"新建版本"弹窗并预填源 profile 的当前值。
 * 预填必须在打开时完成，保证用户看到的是被选中 profile 的真实配置。
 */
function openVersionModal(profile: SearchProfile) {
  versionSource.value = profile
  versionForm.value = versionFormFromProfile(profile)
  versionError.value = null
  showVersionModal.value = true
}

/**
 * 归档按钮可见性。
 * active 会被后端 409 拒绝（需先激活其它版本），archived 无需重复归档。
 */
function canArchive(status: string): boolean {
  return status !== 'active' && status !== 'archived'
}

async function loadProfiles() {
  if (!isSystemAdmin.value) return
  await load({ limit: limit.value, offset: offset.value })
}

onMounted(loadProfiles)

async function handleCreate() {
  if (!createForm.value.name) {
    createError.value = t('workspace.nameAndDisplayNameRequired')
    return
  }
  createLoading.value = true
  createError.value = null
  try {
    const api = useSearchAdminApi()
    await api.createProfile(createForm.value)
    showCreateModal.value = false
    createForm.value = { ...defaultForm }
    await loadProfiles()
  } catch (err) {
    const apiErr = err as ApiError
    createError.value = apiErr.error || t('admin.createProfileFailed')
  } finally {
    createLoading.value = false
  }
}

/**
 * 新建版本：把弹窗中暴露的字段作为覆盖值提交，其余字段由服务端继承源 profile。
 * 成功后关闭弹窗、刷新列表，并提示新版本为 draft（需激活后才生效）。
 */
async function handleCreateVersion() {
  const source = versionSource.value
  const form = versionForm.value
  if (!source || !form) {
    // 不应到达：弹窗只能通过 openVersionModal 打开并同时初始化源 profile 与表单。
    console.error('[search-profiles.vue] 新建版本缺少源 profile 或表单，已中止提交')
    return
  }
  if (!form.name) {
    versionError.value = t('admin.profileNameRequired')
    return
  }
  versionLoading.value = true
  versionError.value = null
  try {
    const api = useSearchAdminApi()
    await api.createProfileVersion(source.id, buildVersionPayload(form))
    showVersionModal.value = false
    versionSource.value = null
    versionForm.value = null
    versionNotice.value = t('admin.profileVersionCreated')
    await loadProfiles()
  } catch (err) {
    const apiErr = err as ApiError
    versionError.value = apiErr.error || t('admin.createProfileVersionFailed')
  } finally {
    versionLoading.value = false
  }
}

/**
 * 危险操作统一确认（UModal）。
 *
 * 引入动机：archive / rollback / activate 都会改变线上检索行为，
 * 原生 confirm() 不阻塞 JS 且样式不可控，统一换为 UModal 二次确认。
 * 通过 pendingAction 复用一个确认弹窗，action 字段决定确认后执行的操作。
 */
type PendingAction =
  | { kind: 'archive'; profile: SearchProfile }
  | { kind: 'rollback'; id: string }
  | { kind: 'activate'; id: string }

const pendingAction = ref<PendingAction | null>(null)
const confirmOpen = ref(false)
const confirmLoading = ref(false)
/** 操作级错误（归档/回滚/激活失败），与列表加载错误 error 分离，渲染在确认弹窗内。 */
const actionError = ref<string | null>(null)

type ConfirmColor = 'primary' | 'warning' | 'error' | 'success'
interface ConfirmCopy { title: string; body: string; confirmLabel: string; color: ConfirmColor }

const confirmCopy = computed<ConfirmCopy>(() => {
  const a = pendingAction.value
  if (!a) return { title: '', body: '', confirmLabel: '', color: 'primary' }
  if (a.kind === 'archive') {
    return { title: t('admin.archiveProfile'), body: t('admin.archiveProfileConfirm'), confirmLabel: t('common.archive'), color: 'error' }
  }
  if (a.kind === 'rollback') {
    return { title: t('admin.rollbackProfile'), body: t('admin.rollbackConfirm'), confirmLabel: t('common.rollback'), color: 'warning' }
  }
  // activate
  return { title: t('admin.activateProfile'), body: t('admin.activateProfileConfirm'), confirmLabel: t('common.activate'), color: 'success' }
})

function requestArchive(profile: SearchProfile) {
  pendingAction.value = { kind: 'archive', profile }
  actionError.value = null
  confirmOpen.value = true
}
function requestRollback(id: string) {
  pendingAction.value = { kind: 'rollback', id }
  actionError.value = null
  confirmOpen.value = true
}
function requestActivate(id: string) {
  pendingAction.value = { kind: 'activate', id }
  actionError.value = null
  confirmOpen.value = true
}

/**
 * 确认执行：按 pendingAction.kind 分发到对应 API，成功后刷新列表。
 * 失败时展示后端消息（归档的 409 冲突映射为本地化提示）。
 */
async function confirmPendingAction() {
  const action = pendingAction.value
  if (!action) return
  confirmLoading.value = true
  try {
    const api = useSearchAdminApi()
    if (action.kind === 'archive') {
      archiveLoadingId.value = action.profile.id
      await api.archiveProfile(action.profile.id)
    } else if (action.kind === 'rollback') {
      await api.rollbackProfile(action.id)
    } else {
      await api.activateProfile(action.id)
    }
    confirmOpen.value = false
    pendingAction.value = null
    await loadProfiles()
  } catch (err) {
    const apiErr = err as ApiError
    if (action.kind === 'archive') {
      actionError.value = apiErr.error
        || (apiErr.status === 409 ? t('admin.archiveActiveConflict') : t('admin.archiveProfileFailed'))
    } else if (action.kind === 'rollback') {
      actionError.value = apiErr.error || t('admin.rollbackFailed')
    } else {
      actionError.value = apiErr.error || t('admin.activateProfileFailed')
    }
    // 失败时保留弹窗，用户可重试或取消
  } finally {
    confirmLoading.value = false
    archiveLoadingId.value = null
  }
}

function profileStatusColor(status: string): 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error' | 'neutral' {
  if (status === 'active') return 'success'
  if (status === 'archived') return 'neutral'
  return 'info'
}

// ---- 键盘导航：j/k 上下移动选中行，Enter/o 打开"新建版本"弹窗（主操作），Escape 清除 ----
const selectedIndex = ref(-1)
const listEl = ref<HTMLElement | null>(null)

// 数据变化时清选中，避免指向已不存在的行。
watch(profiles, () => { selectedIndex.value = -1 })

function moveSelection(delta: number) {
  if (profiles.value.length === 0) return
  const next = selectedIndex.value < 0
    ? (delta > 0 ? 0 : profiles.value.length - 1)
    : Math.min(Math.max(selectedIndex.value + delta, 0), profiles.value.length - 1)
  selectedIndex.value = next
  listEl.value?.querySelectorAll('li')[next]?.scrollIntoView({ block: 'nearest' })
}

function openSelected() {
  const profile = profiles.value[selectedIndex.value]
  if (profile) openVersionModal(profile)
}

function clearSelection() {
  selectedIndex.value = -1
}

useHotkey('j', () => moveSelection(1))
useHotkey('k', () => moveSelection(-1))
useHotkey('enter', openSelected)
useHotkey('o', openSelected)
useHotkey('escape', clearSelection)

useHead({ title: () => t('admin.searchProfiles') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-6xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.searchProfiles') }}</h1>
        <UButton v-if="isSystemAdmin" icon="i-lucide-plus" @click="showCreateModal = true">{{ t('admin.newProfile') }}</UButton>
      </div>

      <EmptyState
        v-if="!isSystemAdmin"
        icon="i-lucide-lock"
        :title="t('admin.systemAdminRequired')"
      />

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />
        <div v-if="versionNotice" class="rounded-xl border border-success/20 bg-success/10 p-3 mb-4 text-sm text-success">{{ versionNotice }}</div>

        <div v-if="loading" class="flex justify-center py-8" role="status">
          <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-8 h-8 animate-spin text-muted" />
          <span class="sr-only">{{ t('common.loading') }}</span>
        </div>

        <ul ref="listEl" v-else-if="profiles.length > 0" class="space-y-3" role="list">
          <li
            v-for="(profile, idx) in profiles"
            :key="profile.id"
            class="border border-default rounded-xl p-4 transition-colors"
            :class="idx === selectedIndex ? 'ring-1 ring-primary/60 bg-elevated' : ''"
            :aria-selected="idx === selectedIndex"
          >
            <div class="flex items-start justify-between mb-2">
              <div>
                <h3 class="font-medium text-highlighted">{{ profile.name }}</h3>
                <p class="text-xs text-muted">v{{ profile.version }} · {{ profile.embedding_model }} · {{ profile.embedding_dimensions }}d</p>
              </div>
              <UBadge :color="profileStatusColor(profile.status)" variant="subtle" size="sm">{{ profileStatusLabel(profile.status) }}</UBadge>
            </div>
            <div class="grid grid-cols-3 sm:grid-cols-6 gap-2 text-xs text-muted mb-3">
              <span>Lex K: {{ profile.lexical_top_k }}</span>
              <span>Vec K: {{ profile.vector_top_k }}</span>
              <span>RRF K: {{ profile.rrf_k }}</span>
              <span>Chunk: {{ profile.chunk_target_size }}</span>
              <span>Rerank: {{ profile.reranker_candidate_count }}→{{ profile.reranker_final_count }}</span>
              <span>MaxDoc: {{ profile.max_chunks_per_document }}</span>
            </div>
            <div class="flex gap-2">
              <UButton
                v-if="profile.status !== 'active'"
                size="xs"
                color="success"
                variant="outline"
                @click="requestActivate(profile.id)"
              >{{ t('common.activate') }}</UButton>
              <UButton
                v-if="profile.status === 'archived'"
                size="xs"
                color="warning"
                variant="outline"
                @click="requestRollback(profile.id)"
              >{{ t('common.rollback') }}</UButton>
              <UButton
                size="xs"
                color="info"
                variant="outline"
                @click="openVersionModal(profile)"
              >{{ t('admin.newProfileVersion') }}</UButton>
              <UButton
                v-if="canArchive(profile.status)"
                size="xs"
                color="error"
                variant="outline"
                :loading="archiveLoadingId === profile.id"
                @click="requestArchive(profile)"
              >{{ t('admin.archiveProfile') }}</UButton>
            </div>
          </li>
        </ul>

        <EmptyState v-else icon="i-lucide-settings" :title="t('admin.noProfiles')" />

        <!-- 危险操作统一确认弹窗 -->
        <UModal v-model:open="confirmOpen">
          <template #content>
            <div class="p-6">
              <h3 class="text-lg font-semibold mb-2">{{ confirmCopy.title }}</h3>
              <p class="text-sm text-muted mb-4">{{ confirmCopy.body }}</p>
              <ErrorDisplay v-if="actionError" :message="actionError" class="mb-4" />
              <div class="flex justify-end gap-2">
                <UButton color="neutral" variant="ghost" @click="confirmOpen = false; pendingAction = null">{{ t('common.cancel') }}</UButton>
                <UButton :color="confirmCopy.color" :loading="confirmLoading" @click="confirmPendingAction">{{ confirmCopy.confirmLabel }}</UButton>
              </div>
            </div>
          </template>
        </UModal>

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="(o: number) => { offset = o; loadProfiles() }"
        />
      </template>
    </div>

    <!-- Create Profile Modal -->
    <UModal v-model:open="showCreateModal">
      <template #content>
        <div class="p-6 max-h-[80vh] overflow-y-auto">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.createProfile') }}</h3>
          <form @submit.prevent="handleCreate" class="space-y-3">
            <!-- 全部 25 个可调参数由共享表单组件渲染，与新建版本弹窗保持同一份字段定义 -->
            <ProfileParamsForm v-model="createForm" />
            <ErrorDisplay v-if="createError" :message="createError" />
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="showCreateModal = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="createLoading">{{ t('common.create') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>

    <!-- New Profile Version Modal -->
    <UModal v-model:open="showVersionModal">
      <template #content>
        <div v-if="versionForm && versionSource" class="p-6 max-h-[80vh] overflow-y-auto">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.newProfileVersion') }}</h3>
          <p class="text-xs text-muted mb-1">{{ versionSource.name }} · v{{ versionSource.version }}</p>
          <p class="text-xs text-muted mb-4">{{ t('admin.newProfileVersionHint') }}</p>
          <form @submit.prevent="handleCreateVersion" class="space-y-3">
            <!-- 与创建弹窗共用同一套字段；预填值全部来自被选中的源 profile -->
            <ProfileParamsForm v-model="versionForm" />
            <ErrorDisplay v-if="versionError" :message="versionError" />
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="showVersionModal = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="versionLoading">{{ t('admin.newProfileVersion') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>
  </div>
</template>
