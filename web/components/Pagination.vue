<!-- Pagination.vue — 分页组件
//
// 引入动机：design/06-IMPLEMENTATION.md §工程要求 "所有列表和搜索分页"。
-->
<script setup lang="ts">
const props = defineProps<{
  total: number
  limit: number
  offset: number
}>()

const emit = defineEmits<{
  'update:offset': [offset: number]
}>()

const { t } = useI18n()

const currentPage = computed(() => Math.floor(props.offset / props.limit) + 1)
const totalPages = computed(() => Math.max(1, Math.ceil(props.total / props.limit)))

const canPrev = computed(() => props.offset > 0)
const canNext = computed(() => props.offset + props.limit < props.total)

function goToPage(page: number) {
  const newOffset = (page - 1) * props.limit
  emit('update:offset', newOffset)
}

function prev() {
  if (canPrev.value) {
    goToPage(currentPage.value - 1)
  }
}

function next() {
  if (canNext.value) {
    goToPage(currentPage.value + 1)
  }
}
</script>

<template>
  <div v-if="total > 0" class="flex items-center justify-between px-4 py-3 text-sm">
    <span class="text-muted">
      {{ t('pagination.range', { start: offset + 1, end: Math.min(offset + limit, total), total }) }}
    </span>
    <div class="flex items-center gap-1">
      <UButton
        size="xs"
        color="neutral"
        variant="ghost"
        icon="i-lucide-chevron-left"
        :disabled="!canPrev"
        @click="prev"
      />
      <span class="px-2 text-muted">
        {{ t('pagination.page', { current: currentPage, total: totalPages }) }}
      </span>
      <UButton
        size="xs"
        color="neutral"
        variant="ghost"
        icon="i-lucide-chevron-right"
        :disabled="!canNext"
        @click="next"
      />
    </div>
  </div>
</template>
