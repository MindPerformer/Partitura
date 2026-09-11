<!-- ConflictDialog.vue — 409 冲突对话框
//
// 引入动机：design/04-WEB-API.md §Concurrency 要求 409 显示明确冲突界面/操作，
// 不得 silent overwrite。
// 当编辑保存返回 409 时，显示冲突对话框，提供三种处理路径：
//   1. 重新加载 —— 拉取最新版本覆盖本地，明确警告将丢弃未保存修改；
//   2. 强制覆盖 —— 以本地内容为基准，先重新 read 获取最新 revision/hash 后再 replace；
//   3. 取消 —— 保留本地草稿，关闭对话框由用户自行决定后续。
-->
<script setup lang="ts">
const props = defineProps<{
  open: boolean
  message?: string
}>()

const emit = defineEmits<{
  reload: []
  force: []
  cancel: []
  'update:open': [value: boolean]
}>()

const { t } = useI18n()

// 使用 computed get/set 桥接 prop 到 v-model
const isOpen = computed({
  get: () => props.open,
  set: (val: boolean) => emit('update:open', val)
})

function handleReload() {
  emit('reload')
}

function handleForce() {
  emit('force')
}

function handleCancel() {
  emit('cancel')
}
</script>

<template>
  <UModal v-model:open="isOpen" :dismissible="false">
    <template #content>
      <div class="p-6">
        <div class="flex items-start gap-3 mb-4">
          <UIcon name="i-lucide-triangle-alert" class="w-6 h-6 text-warning flex-shrink-0" />
          <div>
            <h3 class="text-lg font-semibold text-highlighted">{{ t('errors.versionConflict') }}</h3>
            <p class="text-sm text-muted mt-1">
              {{ message || t('errors.versionConflictDesc') }}
            </p>
          </div>
        </div>
        <div class="flex flex-col sm:flex-row justify-end gap-2 mt-6">
          <UButton color="neutral" variant="ghost" @click="handleCancel">{{ t('errors.conflictCancel') }}</UButton>
          <UButton color="error" variant="outline" @click="handleForce">{{ t('errors.conflictForce') }}</UButton>
          <UButton color="primary" @click="handleReload">{{ t('errors.conflictReload') }}</UButton>
        </div>
        <p class="mt-3 text-xs text-muted">
          {{ t('errors.conflictHint') }}
        </p>
      </div>
    </template>
  </UModal>
</template>
