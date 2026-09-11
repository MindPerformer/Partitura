<!-- layouts/auth.vue — 认证页布局（login/bootstrap）
//
// 引入动机：与 default 布局一致的可访问性——提供 skip-link 与主内容地标。
// 认证页无顶栏，仅居中卡片；不挂载命令面板（未登录无可用命令）。
-->
<script setup lang="ts">
const { t } = useI18n()
</script>

<template>
  <div class="min-h-screen flex items-center justify-center bg-muted/50">
    <!-- skip-link：仅键盘聚焦时可见 -->
    <a
      href="#main-content"
      class="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-primary focus:px-4 focus:py-2 focus:text-white focus:outline-none"
    >{{ t('common.skipToContent') }}</a>

    <main id="main-content" tabindex="-1" class="flex items-center justify-center w-full">
      <slot />
    </main>
  </div>
</template>
