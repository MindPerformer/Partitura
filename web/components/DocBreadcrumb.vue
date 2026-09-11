<!-- DocBreadcrumb.vue — 文档路径面包屑
//
// 引入动机：B3 要求把文档页顶部纯文本 doc.path（如 dir/subdir/file.md）
// 改为可点击面包屑。目录段点击导航到 workspace 首页（?path= 携带目录前缀，
// 供侧栏/后续页面定位）；文件段为当前页，不可点击并标记 aria-current="page"。
//
// a11y：<nav aria-label="breadcrumb"> + <ol>/<li> 语义化结构；
// 分隔符 aria-hidden 装饰，不被屏幕阅读器朗读。
-->
<script setup lang="ts">
const props = defineProps<{
  /** workspace UUID（用于构造目录段链接） */
  workspaceId: string
  /** 文档完整路径（如 "architecture/overview.md"）；为空则不渲染 */
  path?: string
}>()

const { t } = useI18n()

interface BreadcrumbSegment {
  /** 展示文本（单段名） */
  label: string
  /** 累积路径（如 "architecture" → "architecture/subdir"），仅目录段有链接 */
  to?: string
  /** 是否文件段（最后一段，不可点击，标记 aria-current="page"） */
  isFile: boolean
}

// 拆分 doc.path 为目录段（可点）+ 文件段（当前页）。
// 目录段导航到 workspace 首页并携带 ?path= 目录前缀，
// 由 WorkspaceLayout 侧栏自动展开对应目录并高亮。
const segments = computed<BreadcrumbSegment[]>(() => {
  const path = props.path?.trim()
  if (!path) return []

  const parts = path.split('/').filter(Boolean)
  return parts.map((part, i) => {
    const isFile = i === parts.length - 1
    const dirPath = parts.slice(0, i + 1).join('/')
    return {
      label: part,
      isFile,
      to: isFile
        ? undefined
        : `/workspaces/${props.workspaceId}?path=${encodeURIComponent(dirPath)}`
    }
  })
})
</script>

<template>
  <nav v-if="segments.length" :aria-label="t('common.breadcrumb')" class="text-sm">
    <ol class="flex flex-wrap items-center gap-1">
      <li
        v-for="(seg, i) in segments"
        :key="seg.label + i"
        class="flex items-center gap-1"
      >
        <NuxtLink
          v-if="seg.to"
          :to="seg.to"
          class="text-muted transition-colors hover:text-primary"
        >{{ seg.label }}</NuxtLink>
        <span
          v-else
          class="text-default break-all"
          aria-current="page"
        >{{ seg.label }}</span>
        <UIcon
          v-if="i < segments.length - 1"
          name="i-lucide-chevron-right"
          class="h-3.5 w-3.5 shrink-0 text-muted"
          aria-hidden="true"
        />
      </li>
    </ol>
  </nav>
</template>
