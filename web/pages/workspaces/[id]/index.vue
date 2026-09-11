<!-- pages/workspaces/[id]/index.vue — Workspace Home
//
// 引入动机：design/04-WEB-API.md §页面 要求 Workspace Home 页面。
// 显示 workspace 信息、统计卡片、PROJECT.md 内容、快捷入口。
// 使用统一 WorkspaceLayout 提供左侧文档树和主页/统计导航。
-->
<script setup lang="ts">
definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)

const { workspace, loading, error, canEdit, currentMemberRole } = useWorkspaceContext(workspaceId)
const { workspaceRoleLabel, workspaceStatusLabel } = useEnumLabels()
const { formatDate: formatDateUtil } = useFormatDate()
const {
  projectDoc,
  projectLoading,
  stats,
  statsLoading,
  statsError
} = useWorkspaceHome(workspaceId)

// ============================ 页面级快捷键 ============================
// c / n：新建文档（仅 canEdit）。输入框聚焦时自动抑制（useHotkey 内置规则）。
useHotkey('c', () => {
  if (!canEdit.value) return
  navigateTo(`/workspaces/${workspaceId.value}/documents/edit`)
})
useHotkey('n', () => {
  if (!canEdit.value) return
  navigateTo(`/workspaces/${workspaceId.value}/documents/edit`)
})

useHead({ title: () => (workspace.value?.display_name || t('workspace.title')) + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <template #default="{ canEdit: layoutCanEdit }">
      <div v-if="loading" class="flex justify-center py-12">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <ErrorDisplay v-else-if="error" :message="error" :title="t('workspace.workspaceInaccessible')" />

      <div v-else-if="workspace" class="mx-auto w-full max-w-5xl px-4 py-6 sm:px-6 sm:py-8">
        <!-- Workspace header -->
        <section class="mb-6 rounded-xl border border-default bg-elevated/30 p-5 sm:p-6">
          <div class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
            <div class="min-w-0">
              <p class="mb-2 text-xs font-semibold uppercase tracking-[0.18em] text-primary">{{ t('workspace.home') }}</p>
              <h1 class="break-words text-xl font-bold text-highlighted sm:text-2xl">{{ workspace.display_name }}</h1>
              <p v-if="workspace.description" class="mt-2 max-w-2xl text-sm leading-6 text-muted">{{ workspace.description }}</p>
            </div>
            <div class="flex shrink-0 flex-wrap items-center gap-2">
              <UBadge variant="subtle" size="sm">{{ workspaceStatusLabel(workspace.status) }}</UBadge>
              <UBadge variant="subtle" size="sm" color="neutral">{{ t('workspace.role') }}: {{ workspaceRoleLabel(currentMemberRole) }}</UBadge>
            </div>
          </div>
        </section>

        <!-- Workspace stats -->
        <section aria-labelledby="workspace-stats-title" class="mb-6">
          <div class="mb-3 flex items-center justify-between">
            <h2 id="workspace-stats-title" class="text-base font-semibold text-highlighted sm:text-lg">{{ t('workspace.workspaceStats') }}</h2>
          </div>

          <div v-if="statsLoading" class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
            <div v-for="item in 5" :key="item" class="h-24 animate-pulse rounded-xl border border-default bg-elevated/40" />
          </div>
          <ErrorDisplay v-else-if="statsError" :message="statsError" />
          <div v-else-if="stats" class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
            <div class="rounded-xl border border-default bg-default p-4 shadow-sm">
              <UIcon name="i-lucide-files" class="mb-4 h-5 w-5 text-primary" />
              <p class="text-2xl font-bold text-default sm:text-3xl">{{ stats.total_documents }}</p>
              <p class="mt-1 text-xs text-muted">{{ t('workspace.totalDocuments') }}</p>
            </div>
            <div class="rounded-xl border border-default bg-default p-4 shadow-sm">
              <UIcon name="i-lucide-file-check-2" class="mb-4 h-5 w-5 text-success" />
              <p class="text-2xl font-bold text-default sm:text-3xl">{{ stats.active_documents }}</p>
              <p class="mt-1 text-xs text-muted">{{ t('workspace.activeDocuments') }}</p>
            </div>
            <div class="rounded-xl border border-default bg-default p-4 shadow-sm">
              <UIcon name="i-lucide-file-pen-line" class="mb-4 h-5 w-5 text-warning" />
              <p class="text-2xl font-bold text-default sm:text-3xl">{{ stats.draft_documents }}</p>
              <p class="mt-1 text-xs text-muted">{{ t('workspace.draftDocuments') }}</p>
            </div>
            <div class="rounded-xl border border-default bg-default p-4 shadow-sm">
              <UIcon name="i-lucide-archive" class="mb-4 h-5 w-5 text-error" />
              <p class="text-2xl font-bold text-default sm:text-3xl">{{ stats.archived_documents }}</p>
              <p class="mt-1 text-xs text-muted">{{ t('workspace.archivedDocuments') }}</p>
            </div>
            <div class="rounded-xl border border-default bg-default p-4 shadow-sm">
              <UIcon name="i-lucide-users" class="mb-4 h-5 w-5 text-info" />
              <p class="text-2xl font-bold text-default sm:text-3xl">{{ stats.member_count }}</p>
              <p class="mt-1 text-xs text-muted">{{ t('workspace.memberCount') }}</p>
            </div>
          </div>
          <p v-else class="rounded-xl border border-dashed border-default py-8 text-center text-sm text-muted">{{ t('common.noData') }}</p>
        </section>

        <!-- Quick actions -->
        <section class="mb-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
          <UButton
            :to="`/workspaces/${workspaceId}/documents/edit`"
            variant="outline"
            block
            class="h-auto justify-start p-4"
            :disabled="!layoutCanEdit"
          >
            <UIcon name="i-lucide-plus" class="mr-2 h-5 w-5 shrink-0" />
            <span class="text-left"><span class="block font-medium">{{ t('workspace.newDocument') }}</span><span class="mt-0.5 block text-xs text-muted">{{ t('workspace.newDocumentHint') }}</span></span>
          </UButton>
          <UButton
            :to="`/workspaces/${workspaceId}/search`"
            variant="outline"
            block
            class="h-auto justify-start p-4"
          >
            <UIcon name="i-lucide-search" class="mr-2 h-5 w-5 shrink-0" />
            <span class="text-left"><span class="block font-medium">{{ t('common.search') }}</span><span class="mt-0.5 block text-xs text-muted">{{ t('workspace.searchHint') }}</span></span>
          </UButton>
          <UButton
            :to="`/workspaces/${workspaceId}/members`"
            variant="outline"
            block
            class="h-auto justify-start p-4"
          >
            <UIcon name="i-lucide-users" class="mr-2 h-5 w-5 shrink-0" />
            <span class="text-left"><span class="block font-medium">{{ t('workspace.members') }}</span><span class="mt-0.5 block text-xs text-muted">{{ t('workspace.membersHint') }}</span></span>
          </UButton>
          <UButton
            :to="`/workspaces/${workspaceId}/settings`"
            variant="outline"
            block
            class="h-auto justify-start p-4"
          >
            <UIcon name="i-lucide-settings" class="mr-2 h-5 w-5 shrink-0" />
            <span class="text-left"><span class="block font-medium">{{ t('workspace.settings') }}</span><span class="mt-0.5 block text-xs text-muted">{{ t('workspace.settingsHint') }}</span></span>
          </UButton>
        </section>

        <!-- Recent revisions and PROJECT.md -->
        <div class="grid min-w-0 gap-6 lg:grid-cols-3">
          <UCard class="lg:col-span-1">
            <template #header>
              <div class="flex items-center gap-2">
                <UIcon name="i-lucide-history" class="h-4 w-4 text-primary" />
                <h2 class="font-semibold">{{ t('workspace.recentRevisions') }}</h2>
              </div>
            </template>
            <div v-if="stats?.recent_revisions.length" class="space-y-2">
              <NuxtLink
                v-for="rev in stats.recent_revisions"
                :key="`${rev.path}-${rev.revision_number}`"
                :to="`/workspaces/${workspaceId}/documents/read?path=${encodeURIComponent(rev.path)}`"
                class="block rounded-xl border border-default p-3 transition-colors hover:border-primary/50 hover:bg-elevated/50"
              >
                <div class="flex min-w-0 items-center justify-between gap-2">
                  <span class="truncate text-sm font-medium text-highlighted">{{ rev.title || rev.path }}</span>
                  <UBadge size="sm" variant="subtle">#{{ rev.revision_number }}</UBadge>
                </div>
                <p class="mt-1 truncate text-xs text-muted">{{ rev.path }}</p>
              </NuxtLink>
            </div>
            <p v-else class="py-6 text-center text-sm text-muted">{{ t('workspace.noRecentRevisions') }}</p>
          </UCard>

          <div class="min-w-0 lg:col-span-2">
            <!-- 与 stats 骨架屏统一：项目文档加载用占位骨架而非 spinner -->
            <div v-if="projectLoading" class="min-h-48 animate-pulse rounded-xl border border-default bg-elevated/40 p-5">
              <div class="mb-3 h-5 w-32 rounded bg-default" />
              <div class="space-y-2">
                <div class="h-3 w-full rounded bg-default" />
                <div class="h-3 w-5/6 rounded bg-default" />
                <div class="h-3 w-2/3 rounded bg-default" />
              </div>
            </div>
            <UCard v-else-if="projectDoc" class="min-w-0 overflow-hidden">
              <template #header>
                <div class="flex items-center justify-between gap-3">
                  <div class="flex min-w-0 items-center gap-2">
                    <UIcon name="i-lucide-file-text" class="h-4 w-4 shrink-0 text-primary" />
                    <h2 class="truncate font-semibold">{{ t('workspace.projectMd') }}</h2>
                  </div>
                  <UButton
                    v-if="canEdit"
                    size="xs"
                    variant="ghost"
                    icon="i-lucide-pencil"
                    :aria-label="t('common.edit')"
                    :to="`/workspaces/${workspaceId}/documents/edit?path=PROJECT.md`"
                  />
                </div>
              </template>
              <MarkdownRenderer :content="projectDoc.content_markdown" />
            </UCard>
            <UCard v-else>
              <EmptyState
                icon="i-lucide-file-text"
                :title="t('workspace.projectMdNotAvailable')"
              />
            </UCard>
          </div>
        </div>
      </div>
    </template>
  </WorkspaceLayout>
</template>
