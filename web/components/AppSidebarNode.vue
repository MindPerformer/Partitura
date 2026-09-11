<!-- AppSidebarNode.vue — 递归树节点
//
// 引入动机：document tree 需要递归渲染目录和文件节点。
//
// a11y：文件节点用 <NuxtLink>（to=read?path=）支持 Ctrl+Click 新标签打开，
// role="treeitem" 且当前项 aria-current="page"；目录节点保留 button，
// 加 role="treeitem" + :aria-expanded。子列表用 <ul role="group">。
// 当前文档节点在 currentPath 变化后 scrollIntoView({block:'nearest'})。
-->
<script setup lang="ts">
import type { TreeNode } from '~/utils/tree'

const props = defineProps<{
  node: TreeNode
  currentPath?: string
  expandedDirs: Set<string>
  workspaceId: string
  level: number
}>()

const emit = defineEmits<{
  select: [node: TreeNode]
  toggle: [path: string]
}>()

const isActive = computed(() => !props.node.isDirectory && props.node.path === props.currentPath)
const isExpanded = computed(() => props.expandedDirs.has(props.node.path))

// 文件节点渲染为真实链接（read?path=），使 Ctrl/Cmd+Click 能在新标签打开。
const docLink = computed(() =>
  `/workspaces/${props.workspaceId}/documents/read?path=${encodeURIComponent(props.node.path)}`
)

function handleToggle() {
  emit('toggle', props.node.path)
}

// 当前文档节点滚动到可视区域，currentPath 变化后触发。
const rootEl = ref<HTMLElement | null>(null)
watch(() => props.currentPath, async (path) => {
  if (!isActive.value) return
  await nextTick()
  rootEl.value?.scrollIntoView({ block: 'nearest' })
}, { immediate: true })
</script>

<template>
  <li ref="rootEl" role="treeitem" :aria-expanded="node.isDirectory ? isExpanded : undefined" :aria-current="isActive ? 'page' : undefined">
    <!-- 目录节点：button + aria-expanded，点击仅展开/折叠 -->
    <button
      v-if="node.isDirectory"
      type="button"
      class="w-full flex items-center gap-1.5 px-2 py-1.5 rounded-md text-sm text-left transition-colors text-default hover:bg-elevated"
      :style="{ paddingLeft: `${level * 12 + 8}px` }"
      @click="handleToggle"
    >
      <UIcon
        :name="isExpanded ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
        class="w-3.5 h-3.5 flex-shrink-0 text-muted"
      />
      <span class="truncate">{{ node.label }}</span>
    </button>

    <!-- 文件节点：NuxtLink 支持 Ctrl+Click 新标签打开 -->
    <NuxtLink
      v-else
      :to="docLink"
      class="w-full flex items-center gap-1.5 px-2 py-1.5 rounded-md text-sm text-left transition-colors"
      :class="isActive
        ? 'bg-primary/10 text-primary font-medium'
        : 'text-default hover:bg-elevated'"
      :style="{ paddingLeft: `${level * 12 + 8}px` }"
    >
      <UIcon
        name="i-lucide-file"
        class="w-3.5 h-3.5 flex-shrink-0 text-muted"
      />
      <span class="truncate">{{ node.label }}</span>
    </NuxtLink>

    <ul v-if="node.isDirectory && isExpanded" class="space-y-0.5 mt-0.5" role="group">
      <AppSidebarNode
        v-for="child in node.children"
        :key="child.path"
        :node="child"
        :current-path="currentPath"
        :expanded-dirs="expandedDirs"
        :workspace-id="workspaceId"
        :level="level + 1"
        @select="emit('select', $event)"
        @toggle="emit('toggle', $event)"
      />
    </ul>
  </li>
</template>
