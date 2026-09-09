<!-- pages/workspaces/[id]/index.vue — Workspace Home
//
// 引入动机：design/04-WEB-API.md §页面 要求 Workspace Home 页面。
// 显示 workspace 信息、统计卡片、PROJECT.md 内容、快捷入口。
// 使用统一 WorkspaceLayout 提供左侧文档树和主页/统计导航。
-->
<script setup lang="ts">
import type { Document, WorkspaceStats, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)

const { workspace, loading, error, canEdit, currentMemberRole } = useWorkspaceContext(workspaceId)
const { formatDate: formatDateUtil } = useFormatDate()

// 加载 PROJECT.md
const projectDoc = ref<Document | null>(null)
const projectLoading = ref(false)

async function loadProjectDoc() {
  if (!workspaceId.value) return
  projectLoading.value = true
  try {
    const api = useDocumentApi()
    projectDoc.value = await api.read(workspaceId.value, 'PROJECT.md')
  } catch {
    // PROJECT.md 可能不存在或无权限，静默处理
    projectDoc.value = null
  } finally {
    projectLoading.value = false
  }
}

watch(workspace, () => {
  loadProjectDoc()
  loadStats()
}, { immediate: true })

// 加载工作台统计信息
const stats = ref<WorkspaceStats | null>(null)
const statsLoading = ref(false)
const statsError = ref<string | null>(null)

async function loadStats() {
  if (!workspaceId.value) return
  statsLoading.value = true
  statsError.value = null
  try {
    const api = useWorkspaceApi()
    const res = await api.stats(workspaceId.value)
    stats.value = res.stats
  } catch (err) {
    const apiErr = err as ApiError
    statsError.value = apiErr.error || t('workspace.loadStatsFailed')
    stats.value = null
  } finally {
    statsLoading.value = false
  }
}

useHead({ title: () => (workspace.value?.display_name || t('workspace.title')) + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <template #default="{ canEdit: layoutCanEdit }">
      <div v-if="loading" class="flex justify-center py-12">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <ErrorDisplay v-else-if="error" :message="error" :title="t('workspace.workspaceInaccessible')" />

      <div v-else-if="workspace" class="max-w-4xl mx-auto px-4 py-8">
        <div class="mb-6">
          <h1 class="text-2xl font-bold text-highlighted">{{ workspace.display_name }}</h1>
          <p class="text-muted mt-1">{{ workspace.description }}</p>
          <div class="flex items-center gap-3 mt-3 text-sm">
            <UBadge variant="subtle" size="xs">{{ workspace.status }}</UBadge>
            <UBadge variant="subtle" size="xs" color="neutral">{{ t('workspace.role') }}: {{ currentMemberRole || 'N/A' }}</UBadge>
          </div>
        </div>

        <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mb-8">
          <UButton
            :to="`/workspaces/${workspaceId}/documents/edit`"
            variant="outline"
            block
            :disabled="!layoutCanEdit"
          >
            <UIcon name="i-lucide-plus" class="mr-1" /> {{ t('workspace.newDocument') }}
          </UButton>
          <UButton
            :to="`/workspaces/${workspaceId}/search`"
            variant="outline"
            block
          >
            <UIcon name="i-lucide-search" class="mr-1" /> {{ t('common.search') }}
          </UButton>
          <UButton
            :to="`/workspaces/${workspaceId}/members`"
            variant="outline"
            block
          >
            <UIcon name="i-lucide-users" class="mr-1" /> {{ t('workspace.members') }}
          </UButton>
          <UButton
            :to="`/workspaces/${workspaceId}/settings`"
            variant="outline"
            block
          >
            <UIcon name="i-lucide-settings" class="mr-1" /> {{ t('workspace.settings') }}
          </UButton>
        </div>

        <!-- Workspace Stats -->
        <UCard class="mb-6">
          <template #header>
            <h2 class="font-semibold">{{ t('workspace.workspaceStats') }}</h2>
          </template>

          <div v-if="statsLoading" class="flex justify-center py-8">
            <UIcon name="i-lucide-loader-circle" class="w-6 h-6 animate-spin text-muted" />
          </div>

          <ErrorDisplay v-else-if="statsError" :message="statsError" />

          <div v-else-if="stats" class="space-y-4">
            <div class="grid grid-cols-2 sm:grid-cols-4 gap-4">
              <div class="border border-default rounded-lg p-3 text-center">
                <p class="text-2xl font-bold text-highlighted">{{ stats.total_documents }}</p>
                <p class="text-xs text-muted">{{ t('workspace.totalDocuments') }}</p>
              </div>
              <div class="border border-default rounded-lg p-3 text-center">
                <p class="text-2xl font-bold text-green-500">{{ stats.active_documents }}</p>
                <p class="text-xs text-muted">{{ t('workspace.activeDocuments') }}</p>
              </div>
              <div class="border border-default rounded-lg p-3 text-center">
                <p class="text-2xl font-bold text-amber-500">{{ stats.draft_documents }}</p>
                <p class="text-xs text-muted">{{ t('workspace.draftDocuments') }}</p>
              </div>
              <div class="border border-default rounded-lg p-3 text-center">
                <p class="text-2xl font-bold text-red-500">{{ stats.archived_documents }}</p>
                <p class="text-xs text-muted">{{ t('workspace.archivedDocuments') }}</p>
              </div>
            </div>

            <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div class="border border-default rounded-lg p-3 text-center">
                <p class="text-2xl font-bold text-blue-500">{{ stats.member_count }}</p>
                <p class="text-xs text-muted">{{ t('workspace.memberCount') }}</p>
              </div>
            </div>

            <div>
              <h3 class="text-sm font-medium text-highlighted mb-2">{{ t('workspace.recentRevisions') }}</h3>
              <div v-if="stats.recent_revisions.length === 0" class="text-muted text-sm">
                {{ t('workspace.noRecentRevisions') }}
              </div>
              <div v-else class="space-y-2">
                <NuxtLink
                  v-for="rev in stats.recent_revisions"
                  :key="`${rev.path}-${rev.revision_number}`"
                  :to="`/workspaces/${workspaceId}/documents/read?path=${encodeURIComponent(rev.path)}`"
                  class="block border border-default rounded-lg p-2 hover:border-primary/50 transition-colors"
                >
                  <div class="flex items-center justify-between">
                    <span class="text-sm font-medium text-highlighted truncate">{{ rev.title || rev.path }}</span>
                    <UBadge size="xs" variant="subtle">#{{ rev.revision_number }}</UBadge>
                  </div>
                  <p class="text-xs text-muted">{{ rev.path }}</p>
                </NuxtLink>
              </div>
            </div>
          </div>

          <p v-else class="text-center text-muted py-4">{{ t('common.noData') }}</p>
        </UCard>

        <!-- PROJECT.md -->
        <div v-if="projectLoading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-6 h-6 animate-spin text-muted" />
        </div>
        <UCard v-else-if="projectDoc" class="mt-6">
          <template #header>
            <div class="flex items-center justify-between">
              <h2 class="font-semibold">{{ t('workspace.projectMd') }}</h2>
              <UButton
                v-if="canEdit"
                size="xs"
                variant="ghost"
                icon="i-lucide-pencil"
                :to="`/workspaces/${workspaceId}/documents/edit?path=PROJECT.md`"
              />
            </div>
          </template>
          <MarkdownRenderer :content="projectDoc.content_markdown" />
        </UCard>
        <UCard v-else class="mt-6">
          <p class="text-muted text-center py-4">{{ t('workspace.projectMdNotAvailable') }}</p>
        </UCard>
      </div>
    </template>
  </WorkspaceLayout>
</template>
