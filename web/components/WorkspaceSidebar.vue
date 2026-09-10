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
}>()

const { t } = useI18n()
const route = useRoute()

function handleSelect(path: string) {
  emit('select', path)
}
</script>

<template>
  <div class="flex h-full min-h-0 flex-col bg-default">
    <nav class="shrink-0 space-y-1 border-b border-default px-3 py-3">
      <NuxtLink
        :to="`/workspaces/${props.workspaceId}`"
        class="flex items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-colors"
        :class="route.path === `/workspaces/${props.workspaceId}` ? 'bg-primary/10 text-primary' : 'text-default hover:bg-elevated'"
      >
        <UIcon name="i-lucide-home" class="h-4 w-4 shrink-0" />
        {{ t('workspace.home') }}
      </NuxtLink>
    </nav>

    <div class="flex shrink-0 items-center justify-between border-b border-default px-4 py-3">
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

    <AppSidebar
      class="min-h-0 flex-1"
      :documents="props.documents"
      :workspace-id="props.workspaceId"
      :current-path="props.currentPath"
      @select="handleSelect"
    />
  </div>
</template>
