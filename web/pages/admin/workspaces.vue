<!-- pages/admin/workspaces.vue — Admin Workspaces
//
// 引入动机：design/04-WEB-API.md §页面 要求 Admin Workspaces 页面。
// 仅 system_admin 可访问，列出全部 workspace。
-->
<script setup lang="ts">
import type { Workspace, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()

const workspaces = ref<Workspace[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

async function loadWorkspaces() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useAdminApi()
    const res = await api.listAllWorkspaces({ limit: limit.value, offset: offset.value })
    workspaces.value = res.workspaces
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadWorkspacesFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadWorkspaces)

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadWorkspaces()
}

useHead({ title: () => t('admin.adminWorkspaces') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-4xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('admin.adminWorkspaces') }}</h1>

      <div v-if="!isSystemAdmin" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('admin.systemAdminRequired') }}</p>
      </div>

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <div v-if="loading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <UCard v-else-if="workspaces.length > 0">
          <div class="space-y-2">
            <div
              v-for="ws in workspaces"
              :key="ws.id"
              class="flex items-center justify-between border border-default rounded-lg p-3"
            >
              <div>
                <p class="text-sm font-medium text-highlighted">{{ ws.display_name }}</p>
                <p class="text-xs text-muted">{{ ws.name }} · {{ t('admin.createdBy') }} {{ ws.created_by.substring(0, 8) }}...</p>
              </div>
              <div class="flex items-center gap-2">
                <UBadge :color="ws.status === 'active' ? 'success' : 'neutral'" variant="subtle" size="sm">{{ ws.status }}</UBadge>
                <UButton size="xs" variant="ghost" icon="i-lucide-eye" :to="`/workspaces/${ws.id}`" />
              </div>
            </div>
          </div>
        </UCard>

        <p v-else class="text-center text-muted py-8">{{ t('admin.noWorkspaces') }}</p>

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="handleOffsetChange"
        />
      </template>
    </div>
  </div>
</template>
