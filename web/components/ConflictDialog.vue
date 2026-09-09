<!-- ConflictDialog.vue — 409 冲突对话框
//
// 引入动机：design/04-WEB-API.md §Concurrency 要求 409 显示明确冲突界面/操作，
// 不得 silent overwrite。
// 当编辑保存返回 409 时，显示冲突对话框，提供"重新加载"和"强制覆盖"选项。
-->
<script setup lang="ts">
const props = defineProps<{
  open: boolean
  message?: string
}>()

const emit = defineEmits<{
  reload: []
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
        <div class="flex justify-end gap-2 mt-6">
          <UButton color="neutral" variant="ghost" @click="handleCancel">{{ t('common.cancel') }}</UButton>
          <UButton color="primary" @click="handleReload">{{ t('errors.reloadLatest') }}</UButton>
        </div>
      </div>
    </template>
  </UModal>
</template>
