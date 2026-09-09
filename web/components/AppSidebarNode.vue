<!-- AppSidebarNode.vue — 递归树节点
//
// 引入动机：document tree 需要递归渲染目录和文件节点。
-->
<script setup lang="ts">
import type { TreeNode } from '~/utils/tree'

const props = defineProps<{
  node: TreeNode
  currentPath?: string
  expandedDirs: Set<string>
  level: number
}>()

const emit = defineEmits<{
  select: [node: TreeNode]
  toggle: [path: string]
}>()

const isActive = computed(() => !props.node.isDirectory && props.node.path === props.currentPath)
const isExpanded = computed(() => props.expandedDirs.has(props.node.path))

function handleClick() {
  emit('select', props.node)
}
</script>

<template>
  <li>
    <button
      class="w-full flex items-center gap-1.5 px-2 py-1.5 rounded-md text-sm text-left transition-colors"
      :class="isActive
        ? 'bg-primary/10 text-primary font-medium'
        : 'text-default hover:bg-elevated'"
      :style="{ paddingLeft: `${level * 12 + 8}px` }"
      @click="handleClick"
    >
      <UIcon
        v-if="node.isDirectory"
        :name="isExpanded ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
        class="w-3.5 h-3.5 flex-shrink-0 text-muted"
      />
      <UIcon
        v-else
        name="i-lucide-file"
        class="w-3.5 h-3.5 flex-shrink-0 text-muted"
      />
      <span class="truncate">{{ node.label }}</span>
    </button>

    <ul v-if="node.isDirectory && isExpanded" class="space-y-0.5 mt-0.5">
      <AppSidebarNode
        v-for="child in node.children"
        :key="child.path"
        :node="child"
        :current-path="currentPath"
        :expanded-dirs="expandedDirs"
        :level="level + 1"
        @select="emit('select', $event)"
        @toggle="emit('toggle', $event)"
      />
    </ul>
  </li>
</template>
