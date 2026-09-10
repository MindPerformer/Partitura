<!-- DocumentSidePanel.vue — 文档目录、元数据与来源面板
//
// 引入动机：桌面阅读页使用固定右栏，移动端使用抽屉展示同一组信息。
// 抽出面板内容后，两种布局共享目录跳转、元数据和来源管理行为。
-->
<script setup lang="ts">
import type { Document, Source } from '~/types/api'
import type { MarkdownHeading } from '~/utils/markdown'

const props = defineProps<{
  doc: Document
  headings: MarkdownHeading[]
  sources: Source[]
  workspaceId: string
  docPath: string
  canEdit: boolean
}>()

const emit = defineEmits<{
  tocClick: [id: string]
}>()

const { t } = useI18n()
const { formatDate } = useFormatDate()
const rightTab = ref<'toc' | 'meta' | 'sources'>('toc')
</script>

<template>
  <div class="flex h-full min-h-0 flex-col bg-default">
    <div class="shrink-0 border-b border-default px-3 py-2">
      <div class="grid grid-cols-3 gap-1">
        <UButton size="xs" :variant="rightTab === 'toc' ? 'solid' : 'ghost'" @click="rightTab = 'toc'">{{ t('document.tableOfContents') }}</UButton>
        <UButton size="xs" :variant="rightTab === 'meta' ? 'solid' : 'ghost'" @click="rightTab = 'meta'">{{ t('document.info') }}</UButton>
        <UButton size="xs" :variant="rightTab === 'sources' ? 'solid' : 'ghost'" @click="rightTab = 'sources'">{{ t('document.sources') }}</UButton>
      </div>
    </div>

    <div v-if="rightTab === 'toc'" class="min-h-0 flex-1 overflow-y-auto p-3">
      <ul v-if="props.headings.length > 0" class="space-y-1 text-sm">
        <li
          v-for="heading in props.headings"
          :key="heading.id"
          :style="{ paddingLeft: `${(heading.level - 1) * 12 + 4}px` }"
        >
          <button
            type="button"
            class="w-full truncate rounded px-1 py-1 text-left text-muted transition-colors hover:bg-elevated hover:text-primary"
            @click="emit('tocClick', heading.id)"
          >
            {{ heading.text }}
          </button>
        </li>
      </ul>
      <p v-else class="text-xs text-muted">{{ t('document.noHeadings') }}</p>
    </div>

    <div v-if="rightTab === 'meta'" class="min-h-0 flex-1 space-y-3 overflow-y-auto p-3 text-sm">
      <div>
        <p class="text-xs uppercase text-muted">{{ t('document.path') }}</p>
        <p class="break-all text-default">{{ props.doc.path }}</p>
      </div>
      <div>
        <p class="text-xs uppercase text-muted">{{ t('document.type') }}</p>
        <p class="text-default">{{ props.doc.type || 'N/A' }}</p>
      </div>
      <div>
        <p class="text-xs uppercase text-muted">{{ t('workspace.status') }}</p>
        <p class="text-default">{{ props.doc.status }}</p>
      </div>
      <div>
        <p class="text-xs uppercase text-muted">{{ t('document.revision') }}</p>
        <p class="text-default">#{{ props.doc.revision_number }}</p>
      </div>
      <div>
        <p class="text-xs uppercase text-muted">{{ t('document.hash') }}</p>
        <p class="break-all font-mono text-xs text-default">{{ props.doc.content_hash.substring(0, 16) }}...</p>
      </div>
      <div>
        <p class="text-xs uppercase text-muted">{{ t('document.created') }}</p>
        <p class="text-default">{{ formatDate(props.doc.created_at) }}</p>
      </div>
      <div>
        <p class="text-xs uppercase text-muted">{{ t('document.updated') }}</p>
        <p class="text-default">{{ formatDate(props.doc.updated_at) }}</p>
      </div>
    </div>

    <div v-if="rightTab === 'sources'" class="min-h-0 flex-1 overflow-y-auto p-3">
      <div v-if="props.sources.length > 0" class="space-y-2">
        <div
          v-for="src in props.sources"
          :key="src.id"
          class="rounded border border-default p-2 text-sm"
        >
          <div class="mb-1 flex items-center gap-1">
            <UBadge variant="subtle" size="sm">{{ src.source_type }}</UBadge>
          </div>
          <p class="break-all text-xs text-default">{{ src.value }}</p>
          <p v-if="src.title" class="mt-1 text-xs text-muted">{{ src.title }}</p>
        </div>
      </div>
      <p v-else class="text-xs text-muted">{{ t('document.noSources') }}</p>
      <UButton
        v-if="props.canEdit"
        size="xs"
        variant="ghost"
        block
        class="mt-3"
        :to="`/workspaces/${props.workspaceId}/documents/edit?path=${encodeURIComponent(props.docPath)}&tab=sources`"
      >{{ t('document.addSource') }}</UButton>
    </div>
  </div>
</template>
