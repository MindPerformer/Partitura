<!-- EmptyState.vue — 统一空态组件
//
// 引入动机：各页面（文档列表、成员、搜索结果、管理列表等）存在大量零散、
// 样式不一致的空态占位（裸段落 + 图标 + 描述）。本组件提供单一、可复用、
// 可访问的空态结构，供后续页面统一替换。
//
// 用法（示例见各调用页）：
//   EmptyState icon="i-lucide-folder-open" :title="..." :description="..."
//   可选 CTA 放默认 slot（如新建/重试按钮）。
//
// 注意：注释中不书写自闭合/闭合组件标签字面量，避免某些 HTML 解析路径
// （测试环境的 innerHTML/parse5）把注释内的 "</EmptyState>" 误判为非法闭合。
-->
<script setup lang="ts">
// 属性均为展示型；icon 是 Iconify 名称（@nuxt/icon），可选——无 icon 时仅渲染文案。
defineProps<{
  /** Iconify 图标名（如 i-lucide-inbox）。省略则不显示图标。 */
  icon?: string
  /** 主标题（突出显示）。 */
  title: string
  /** 次级描述（弱化、辅助说明）。可选。 */
  description?: string
}>()
</script>

<template>
  <!-- role="status" + aria-live 使空态被屏幕阅读器感知为状态变化而非纯装饰 -->
  <div class="text-center py-12" role="status">
    <UIcon
      v-if="icon"
      :name="icon"
      class="w-12 h-12 text-muted mx-auto mb-3"
      aria-hidden="true"
    />
    <p class="text-highlighted font-medium">{{ title }}</p>
    <p v-if="description" class="text-muted text-sm mt-1">{{ description }}</p>
    <!-- 默认 slot：CTA 按钮区（如“新建”“重试”），有内容时加间距 -->
    <div v-if="$slots.default" class="mt-4 flex items-center justify-center gap-2">
      <slot />
    </div>
  </div>
</template>
