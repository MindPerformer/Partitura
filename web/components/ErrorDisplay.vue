<!-- ErrorDisplay.vue — 错误显示组件
//
// 引入动机：请求错误不能静默吞掉，需要安全且可理解地展示给用户。
-->
<script setup lang="ts">
defineProps<{
  message: string
  title?: string
}>()
</script>

<template>
  <div class="rounded-lg border border-error/20 bg-error/10 p-4">
    <div class="flex items-start gap-3">
      <UIcon name="i-lucide-triangle-alert" class="w-5 h-5 text-error flex-shrink-0 mt-0.5" />
      <div class="flex-1">
        <p v-if="title" class="font-medium text-error">{{ title }}</p>
        <p class="text-sm text-error">{{ message }}</p>
      </div>
    </div>
  </div>
</template>
