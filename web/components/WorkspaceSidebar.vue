<!-- WorkspaceSidebar.vue — 可复用的工作区导航侧栏
//
// 引入动机：桌面侧栏和移动端抽屉需要显示完全一致的主页入口与文档树，
// 抽成独立组件避免两套模板逐渐产生行为差异。
-->
<script setup lang="ts">
import type { DocumentListItem } from '~/types/api'

const props = defineProps<{
  workspaceId: string
  documents: DocumentListItem[]
  currentPath?: string
  canEdit: boolean
}>()

const emit = defineEmits<{
  select: [path: string]
  collapse: []
}>()

const { t } = useI18n()
const route = useRoute()

// 文档名过滤：大小写不敏感地匹配 path/title 片段，用于重建文档树。
const filterQuery = ref('')
const filteredDocuments = computed(() => {
  const q = filterQuery.value.trim().toLowerCase()
  if (!q) return props.documents
  return props.documents.filter(doc =>
    doc.path.toLowerCase().includes(q) || doc.title.toLowerCase().includes(q)
  )
})

function handleSelect(path: string) {
  emit('select', path)
}
</script>

<template>
  <div class="flex h-full min-h-0 flex-col bg-default">
    <nav class="shrink-0 space-y-1 border-b border-default px-3 py-3">
      <div class="flex items-center gap-1">
        <NuxtLink
          :to="`/workspaces/${props.workspaceId}`"
          class="flex min-w-0 flex-1 items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-colors"
          :class="route.path === `/workspaces/${props.workspaceId}` ? 'bg-primary/10 text-primary' : 'text-default hover:bg-elevated'"
        >
          <UIcon name="i-lucide-home" class="h-4 w-4 shrink-0" />
          <span class="truncate">{{ t('workspace.home') }}</span>
        </NuxtLink>
        <!-- 收起按钮：内嵌在 sidebar 头部右侧，点击折叠为图标 rail -->
        <UButton
          size="xs"
          color="neutral"
          variant="ghost"
          icon="i-lucide-panel-left-close"
          :aria-label="t('workspace.collapseSidebar')"
          :title="t('workspace.collapseSidebar')"
          @click="emit('collapse')"
        />
      </div>
    </nav>

    <div class="flex shrink-0 items-center justify-between gap-2 border-b border-default px-4 py-3">
      <span class="text-xs font-semibold uppercase tracking-wide text-muted">{{ t('document.documents') }}</span>
      <UButton
        v-if="props.canEdit"
        size="xs"
        variant="ghost"
        icon="i-lucide-plus"
        :to="`/workspaces/${props.workspaceId}/documents/edit`"
        :aria-label="t('document.newDocument')"
      />
    </div>

    <!-- 文档名过滤 -->
    <div class="shrink-0 border-b border-default px-3 py-2">
      <UInput
        v-model="filterQuery"
        icon="i-lucide-filter"
        size="sm"
        :placeholder="t('document.filterDocuments')"
        :aria-label="t('document.filterDocuments')"
        class="w-full"
      />
    </div>

    <AppSidebar
      class="min-h-0 flex-1"
      :documents="filteredDocuments"
      :workspace-id="props.workspaceId"
      :current-path="props.currentPath"
      @select="handleSelect"
    />
  </div>
</template>
