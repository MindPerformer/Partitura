<!-- pages/admin/search-profiles.vue — Search Profiles
//
// 引入动机：design/04-WEB-API.md §页面 要求 Search Profiles 页面。
// 仅 system_admin 可访问。支持列表、创建、激活、回滚。
-->
<script setup lang="ts">
import type { SearchProfile, ApiError, CreateProfileRequest } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()

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

async function handleActivate(id: string) {
  try {
    const api = useSearchAdminApi()
    await api.activateProfile(id)
    await loadProfiles()
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.activateProfileFailed')
  }
}

async function handleRollback(id: string) {
  if (!confirm(t('admin.rollbackConfirm'))) return
  try {
    const api = useSearchAdminApi()
    await api.rollbackProfile(id)
    await loadProfiles()
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.rollbackFailed')
  }
}

function profileStatusColor(status: string): 'primary' | 'secondary' | 'success' | 'info' | 'warning' | 'error' | 'neutral' {
  if (status === 'active') return 'success'
  if (status === 'archived') return 'neutral'
  return 'info'
}

useHead({ title: () => t('admin.searchProfiles') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-4xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.searchProfiles') }}</h1>
        <UButton v-if="isSystemAdmin" icon="i-lucide-plus" @click="showCreateModal = true">{{ t('admin.newProfile') }}</UButton>
      </div>

      <div v-if="!isSystemAdmin" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('admin.systemAdminRequired') }}</p>
      </div>

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <div v-if="loading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <div v-else-if="profiles.length > 0" class="space-y-3">
          <div
            v-for="profile in profiles"
            :key="profile.id"
            class="border border-default rounded-lg p-4"
          >
            <div class="flex items-start justify-between mb-2">
              <div>
                <h3 class="font-medium text-highlighted">{{ profile.name }}</h3>
                <p class="text-xs text-muted">v{{ profile.version }} · {{ profile.embedding_model }} · {{ profile.embedding_dimensions }}d</p>
              </div>
              <UBadge :color="profileStatusColor(profile.status)" variant="subtle" size="xs">{{ profile.status }}</UBadge>
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
                @click="handleActivate(profile.id)"
              >{{ t('common.activate') }}</UButton>
              <UButton
                v-if="profile.status === 'archived'"
                size="xs"
                color="warning"
                variant="outline"
                @click="handleRollback(profile.id)"
              >{{ t('common.rollback') }}</UButton>
            </div>
          </div>
        </div>

        <p v-else class="text-center text-muted py-8">{{ t('admin.noProfiles') }}</p>

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
            <UFormField :label="t('workspace.name')" name="name">
              <UInput v-model="createForm.name" class="w-full" />
            </UFormField>
            <div class="grid grid-cols-2 gap-3">
              <UFormField :label="t('admin.embeddingModel')">
                <UInput v-model="createForm.embedding_model" class="w-full" />
              </UFormField>
              <UFormField :label="t('admin.dimensions')">
                <UInput v-model.number="createForm.embedding_dimensions" type="number" class="w-full" />
              </UFormField>
            </div>
            <div class="grid grid-cols-2 gap-3">
              <UFormField :label="t('admin.chunkTargetSize')">
                <UInput v-model.number="createForm.chunk_target_size" type="number" class="w-full" />
              </UFormField>
              <UFormField :label="t('admin.chunkOverlap')">
                <UInput v-model.number="createForm.chunk_overlap" type="number" class="w-full" />
              </UFormField>
            </div>
            <div class="grid grid-cols-3 gap-3">
              <UFormField :label="t('admin.titleBoost')">
                <UInput v-model.number="createForm.title_boost" type="number" step="0.1" class="w-full" />
              </UFormField>
              <UFormField :label="t('admin.headingBoost')">
                <UInput v-model.number="createForm.heading_boost" type="number" step="0.1" class="w-full" />
              </UFormField>
              <UFormField :label="t('admin.bodyBoost')">
                <UInput v-model.number="createForm.body_boost" type="number" step="0.1" class="w-full" />
              </UFormField>
            </div>
            <div class="grid grid-cols-3 gap-3">
              <UFormField :label="t('admin.lexicalTopK')">
                <UInput v-model.number="createForm.lexical_top_k" type="number" class="w-full" />
              </UFormField>
              <UFormField :label="t('admin.vectorTopK')">
                <UInput v-model.number="createForm.vector_top_k" type="number" class="w-full" />
              </UFormField>
              <UFormField :label="t('admin.rrfK')">
                <UInput v-model.number="createForm.rrf_k" type="number" class="w-full" />
              </UFormField>
            </div>
            <ErrorDisplay v-if="createError" :message="createError" />
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="showCreateModal = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="createLoading">{{ t('common.create') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>
  </div>
</template>
