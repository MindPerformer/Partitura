<!-- AppSidebar.vue — 文档树侧边栏
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 Sidebar 显示 document tree。
// 从 document list 构建树形结构，支持目录展开/折叠。
//
// a11y：外层 <ul role="tree">，目录子列表 <ul role="group">；
// 文件节点为 NuxtLink（支持 Ctrl+Click 新标签打开），目录节点保留 button + aria-expanded。
-->
<script setup lang="ts">
import type { DocumentListItem } from '~/types/api'
import { buildDocumentTree, type TreeNode } from '~/utils/tree'

const props = defineProps<{
  documents: DocumentListItem[]
  currentPath?: string
  workspaceId: string
}>()

const emit = defineEmits<{
  select: [path: string]
}>()

const { t } = useI18n()

const tree = computed(() => buildDocumentTree(props.documents))
const expandedDirs = ref<Set<string>>(new Set())

function toggleDir(path: string) {
  if (expandedDirs.value.has(path)) {
    expandedDirs.value.delete(path)
  } else {
    expandedDirs.value.add(path)
  }
}

function isExpanded(path: string): boolean {
  return expandedDirs.value.has(path)
}

// 默认展开当前文档所在目录
watch(() => props.currentPath, (path) => {
  if (path) {
    const parts = path.split('/')
    for (let i = 1; i < parts.length; i++) {
      expandedDirs.value.add(parts.slice(0, i).join('/'))
    }
  }
}, { immediate: true })

function selectNode(node: TreeNode) {
  if (node.isDirectory) {
    toggleDir(node.path)
  } else {
    emit('select', node.path)
  }
}
</script>

<template>
  <nav class="h-full overflow-y-auto py-4 px-2" :aria-label="t('document.documents')">
    <ul v-if="tree.length > 0" class="space-y-0.5" role="tree">
      <AppSidebarNode
        v-for="node in tree"
        :key="node.path"
        :node="node"
        :current-path="currentPath"
        :expanded-dirs="expandedDirs"
        :workspace-id="workspaceId"
        :level="0"
        @select="selectNode"
        @toggle="toggleDir"
      />
    </ul>
    <p v-else class="text-sm text-muted px-2 py-4">
      {{ t('document.noDocuments') }}
    </p>
  </nav>
</template>
