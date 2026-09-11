<!-- MarkdownRenderer.vue — 安全 Markdown 渲染组件
//
// 引入动机：design/04-WEB-API.md §Security 要求 safe Markdown rendering、XSS prevention。
// 使用 renderMarkdown 函数（marked + DOMPurify）将 Markdown 渲染为安全 HTML。
// 通过 v-html 渲染净化后的 HTML 是安全的。
-->
<script setup lang="ts">
import { renderMarkdownWithOutline, type MarkdownHeading } from '~/utils/markdown'

const props = defineProps<{
  content: string
}>()

const emit = defineEmits<{
  headings: [headings: MarkdownHeading[]]
}>()

const rendered = computed(() => renderMarkdownWithOutline(props.content))
const html = computed(() => rendered.value.html)

watch(rendered, (value) => {
  emit('headings', value.headings)
}, { immediate: true, flush: 'post' })
</script>

<template>
  <!-- article 语义化容器：渲染的 markdown 是独立文档内容 -->
  <article class="markdown-body" v-html="html" />
</template>

<style>
@reference "~/assets/css/main.css";

.markdown-body {
  @apply min-w-0 text-default leading-7;
  overflow-wrap: anywhere;
}

.markdown-body :where(h1, h2, h3, h4, h5, h6) {
  scroll-margin-top: 5rem;
}

.markdown-body h1 {
  @apply text-3xl font-bold mt-8 mb-4 pb-2 border-b border-default;
}

.markdown-body h2 {
  @apply text-2xl font-semibold mt-6 mb-3 pb-1 border-b border-default;
}

.markdown-body h3 {
  @apply text-xl font-semibold mt-5 mb-2;
}

.markdown-body h4 {
  @apply text-lg font-medium mt-4 mb-2;
}

.markdown-body h5 {
  @apply text-base font-medium mt-3 mb-1;
}

.markdown-body h6 {
  @apply text-sm font-medium mt-3 mb-1 text-muted;
}

.markdown-body p {
  @apply my-3;
}

.markdown-body ul {
  @apply list-disc pl-6 my-3;
}

.markdown-body ol {
  @apply list-decimal pl-6 my-3;
}

.markdown-body li {
  @apply my-1;
}

/* blockquote 正文用 text-default 保证暗色对比度；
   语义色 text-muted 仅保留在装饰性 h6/元信息，正文不降低可读性。 */
.markdown-body blockquote {
  @apply border-l-4 border-muted pl-4 my-3 italic text-default;
}

/* code 文本用 text-highlighted（而非 text-primary on bg-elevated）：
   主色在暗色 elevated 背景上对比度不足，正文/代码统一走前景色层级。 */
.markdown-body code {
  @apply bg-elevated rounded px-1.5 py-0.5 text-sm font-mono text-highlighted;
}

.markdown-body pre {
  @apply bg-elevated rounded-lg p-4 my-4 overflow-x-auto;
}

.markdown-body pre code {
  @apply bg-transparent p-0 text-default;
}

.markdown-body a {
  @apply text-primary underline hover:text-primary/80;
}

.markdown-table-scroll {
  @apply my-4 max-w-full overflow-x-auto;
}

.markdown-body table {
  @apply w-full border-collapse;
  min-width: 100%;
  width: max-content;
  table-layout: auto;
}

.markdown-body th,
.markdown-body td {
  @apply border border-default px-3 py-2 text-left;
  min-width: 0;
  overflow-wrap: anywhere;
  word-break: break-word;
  white-space: normal;
}

.markdown-body th {
  @apply bg-elevated font-medium;
}

.markdown-body img {
  @apply max-w-full rounded-lg my-3;
}

.markdown-body hr {
  @apply my-6 border-default;
}
</style>
