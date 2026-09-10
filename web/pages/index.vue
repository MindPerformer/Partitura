<!-- pages/index.vue — Workspace List
//
// 引入动机：design/04-WEB-API.md §页面 要求 Workspace List 页面。
// 调用 GET /api/workspaces 列出当前用户成员的 workspace。
-->
<script setup lang="ts">
import type { Workspace, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isSystemAdmin, currentUser } = useAuth()

const workspaces = ref<Workspace[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

// 创建 workspace
const showCreateModal = ref(false)
const createForm = ref({ name: '', display_name: '', description: '' })
const createLoading = ref(false)
const createError = ref<string | null>(null)

async function loadWorkspaces() {
  loading.value = true
  error.value = null
  try {
    const api = useWorkspaceApi()
    const res = await api.list({ limit: limit.value, offset: offset.value })
    workspaces.value = res.workspaces
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('workspace.loadFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadWorkspaces)

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadWorkspaces()
}

async function handleCreate() {
  if (!createForm.value.name || !createForm.value.display_name) {
    createError.value = t('workspace.nameAndDisplayNameRequired')
    return
  }

  createLoading.value = true
  createError.value = null

  try {
    const api = useWorkspaceApi()
    const ws = await api.create(createForm.value)
    showCreateModal.value = false
    createForm.value = { name: '', display_name: '', description: '' }
    // 导航到新 workspace
    navigateTo(`/workspaces/${ws.id}`)
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 403) {
      createError.value = t('workspace.noPermission')
    } else if (apiErr.status === 409) {
      createError.value = t('workspace.nameExists')
    } else {
      createError.value = apiErr.error || t('workspace.createFailed')
    }
  } finally {
    createLoading.value = false
  }
}

useHead({ title: () => t('workspace.workspaces') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-5xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('workspace.workspaces') }}</h1>
        <UButton
          icon="i-lucide-plus"
          @click="showCreateModal = true"
        >
          {{ t('workspace.newWorkspace') }}
        </UButton>
      </div>

      <ErrorDisplay v-if="error" :message="error" />

      <div v-if="loading" class="flex justify-center py-12">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <div v-else-if="workspaces.length === 0 && !error" class="text-center py-12">
        <UIcon name="i-lucide-folder-open" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('workspace.noWorkspaces') }}</p>
        <p class="text-sm text-dimmed mt-1">{{ t('workspace.createToGetStarted') }}</p>
      </div>

      <div v-else class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <NuxtLink
          v-for="ws in workspaces"
          :key="ws.id"
          :to="`/workspaces/${ws.id}`"
          class="block rounded-lg border border-default p-4 hover:border-primary/50 transition-colors bg-default"
        >
          <div class="flex items-start justify-between mb-2">
            <UIcon name="i-lucide-folder" class="w-6 h-6 text-primary" />
            <UBadge
              v-if="ws.status === 'archived'"
              color="neutral"
              variant="subtle"
              size="sm"
            >{{ t('workspace.archived') }}</UBadge>
          </div>
          <h3 class="font-medium text-highlighted truncate">{{ ws.display_name }}</h3>
          <p class="text-sm text-muted truncate">{{ ws.description || ws.name }}</p>
        </NuxtLink>
      </div>

      <Pagination
        v-if="total > limit"
        :total="total"
        :limit="limit"
        :offset="offset"
        @update:offset="handleOffsetChange"
      />
    </div>

    <!-- Create Workspace Modal -->
    <UModal v-model:open="showCreateModal">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold text-highlighted mb-4">{{ t('workspace.createWorkspace') }}</h3>
          <form @submit.prevent="handleCreate" class="space-y-4">
            <UFormField :label="t('workspace.name')" :hint="t('workspace.nameHint')" name="name">
              <UInput v-model="createForm.name" :placeholder="t('workspace.namePlaceholder')" class="w-full" />
            </UFormField>
            <UFormField :label="t('workspace.displayName')" name="display_name">
              <UInput v-model="createForm.display_name" :placeholder="t('workspace.displayNamePlaceholder')" class="w-full" />
            </UFormField>
            <UFormField :label="t('workspace.description')" name="description">
              <UTextarea v-model="createForm.description" :placeholder="t('workspace.descriptionPlaceholder')" class="w-full" />
            </UFormField>
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
