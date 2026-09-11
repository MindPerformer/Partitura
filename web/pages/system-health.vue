<!-- pages/system-health.vue — System Health
//
// 引入动机：design/04-WEB-API.md §页面 要求 System Health 页面。
//
// 阻塞报告：server 没有 /health、/api/health 或任何系统健康检查端点。
// 此页面明确告知用户该功能需要后端支持，不显示伪数据。
-->
<script setup lang="ts">
definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()

useHead({ title: () => t('health.title') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-4xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('health.title') }}</h1>

      <div class="rounded-xl border border-warning/20 bg-warning/10 p-6">
        <div class="flex items-start gap-3">
          <UIcon name="i-lucide-triangle-alert" class="w-6 h-6 text-warning flex-shrink-0" />
          <div>
            <h3 class="font-medium text-warning">{{ t('health.endpointNotAvailable') }}</h3>
            <p class="text-sm text-warning mt-1">
              {{ t('health.endpointNotAvailableDesc') }}
            </p>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
