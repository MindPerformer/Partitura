<!-- Pagination.vue — 分页组件
//
// 引入动机：design/06-IMPLEMENTATION.md §工程要求 "所有列表和搜索分页"。
//
// 可访问性（Phase 2）：
//   - 外层包 <nav> 提供 landmark 导航语义
//   - 翻页按钮带 aria-label，当前页文本带 aria-current="page"
//   - 提供跳页输入框（仅当 totalPages > 1 时有意义）
//   - 可选 page-size 选择器：仅当父组件传入 pageSizes 时渲染，配合 update:limit 使用
-->
<script setup lang="ts">
const props = defineProps<{
  total: number
  limit: number
  offset: number
  /** 可选的每页条数选项；传入后渲染 page-size 选择器并通过 update:limit 通知父组件 */
  pageSizes?: number[]
}>()

const emit = defineEmits<{
  'update:offset': [offset: number]
  'update:limit': [limit: number]
}>()

const { t } = useI18n()
// 跳页输入框的唯一 id：避免同一页面渲染多个分页组件时 label[for] 冲突
const jumpInputId = `pagination-jump-${useId()}`

const currentPage = computed(() => Math.floor(props.offset / props.limit) + 1)
const totalPages = computed(() => Math.max(1, Math.ceil(props.total / props.limit)))

const canPrev = computed(() => props.offset > 0)
const canNext = computed(() => props.offset + props.limit < props.total)

// 跳页输入：本地缓冲，避免每次击键都触发翻页请求
const jumpValue = ref<string>('')
watch(currentPage, () => {
  // 翻页后清空跳页输入，避免残留旧页码
  jumpValue.value = ''
})

const pageSizeItems = computed(() =>
  (props.pageSizes ?? []).map(size => ({ label: String(size), value: size }))
)

function goToPage(page: number) {
  const clamped = Math.min(Math.max(1, page), totalPages.value)
  const newOffset = (clamped - 1) * props.limit
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

function jump() {
  const page = Number.parseInt(jumpValue.value, 10)
  if (!Number.isFinite(page) || page < 1 || page > totalPages.value) {
    // 非法输入：重新清空，不发起请求
    jumpValue.value = ''
    return
  }
  goToPage(page)
}

function onPageSizeChange(size: number | string) {
  const n = typeof size === 'string' ? Number.parseInt(size, 10) : size
  if (!Number.isFinite(n) || n <= 0 || n === props.limit) return
  emit('update:limit', n)
}
</script>

<template>
  <nav v-if="total > 0" :aria-label="t('pagination.navigation')" class="flex items-center justify-between px-4 py-3 text-sm">
    <span class="text-muted">
      {{ t('pagination.range', { start: offset + 1, end: Math.min(offset + limit, total), total }) }}
    </span>
    <div class="flex items-center gap-2">
      <UButton
        size="xs"
        color="neutral"
        variant="ghost"
        icon="i-lucide-chevron-left"
        :aria-label="t('pagination.prevPage')"
        :disabled="!canPrev"
        @click="prev"
      />
      <span class="px-2 text-muted" aria-current="page">
        {{ t('pagination.page', { current: currentPage, total: totalPages }) }}
      </span>
      <UButton
        size="xs"
        color="neutral"
        variant="ghost"
        icon="i-lucide-chevron-right"
        :aria-label="t('pagination.nextPage')"
        :disabled="!canNext"
        @click="next"
      />

      <!-- 跳页：仅当存在多页时提供 -->
      <div v-if="totalPages > 1" class="ml-2 flex items-center gap-1">
        <label :for="jumpInputId" class="sr-only">{{ t('pagination.goToPage') }}</label>
        <UInput
          :id="jumpInputId"
          v-model="jumpValue"
          type="number"
          :min="1"
          :max="totalPages"
          :placeholder="t('pagination.pageLabel')"
          :aria-label="t('pagination.goToPage')"
          class="w-20"
          size="xs"
          @keyup.enter="jump"
          @blur="jump"
        />
      </div>

      <!-- 可选 page-size 选择器 -->
      <USelect
        v-if="pageSizeItems.length > 0"
        :model-value="limit"
        :items="pageSizeItems"
        value-key="value"
        label-key="label"
        size="xs"
        class="ml-2 w-24"
        :aria-label="t('pagination.pageLabel')"
        @update:model-value="onPageSizeChange"
      />
    </div>
  </nav>
</template>
