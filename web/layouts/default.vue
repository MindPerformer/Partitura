<!-- layouts/default.vue — 默认布局（所有非 auth 页）
//
// 引入动机：UI/UX 优化阶段让所有非 auth 页自动获得顶栏、主内容地标与
// 跳转链接（skip-link），并挂载全局命令面板与快捷键。
//
// 双顶栏去重（关键设计）：
// 当前多数页面（index/account/admin/*）以及 WorkspaceLayout 仍在自身模板中
// 渲染 <AppHeader/>，且 workspace 页顶栏随 workspace 异步加载才挂载。若布局
// 无条件再渲染一份会出现双顶栏。因此布局顶栏通过 MutationObserver 监听
// #main-content 内的 DOM：一旦页面在其 slot 中渲染出 <header>（来自页面自带
// AppHeader 或 WorkspaceLayout），布局顶栏即自我隐藏；页面没有自带顶栏时
// （后续批次移除冗余 AppHeader 后）布局顶栏持续提供。此机制无需改动任何页面。
//
// 结构：
// - skip-link：键盘用户首个 Tab 即跳到 #main-content。
// - <AppHeader/>：布局提供的顶栏（页面自带顶栏时自动隐藏）。
// - <main id="main-content">：主内容地标，供 skip-link 与屏幕阅读器定位。
// - <CommandPalette/>：全局命令面板 + 快捷键（mod+k、ctrl+k、/、?、g h）。
-->
<script setup lang="ts">
const { t } = useI18n()

// 页面是否已自带 <header>（页面内 AppHeader/WorkspaceLayout 渲染）。
const pageHasOwnHeader = ref(false)
const mainEl = ref<HTMLElement | null>(null)
let observer: MutationObserver | null = null

// 检测主内容区是否出现页面自带的 <header>（布局顶栏在 main 之外，不会被误判）。
function checkForPageHeader(): void {
  const main = mainEl.value
  if (!main) return
  pageHasOwnHeader.value = main.querySelector('header') !== null
}

onMounted(() => {
  checkForPageHeader()
  // workspace 顶栏等异步渲染的 header 通过 MutationObserver 捕获。
  observer = new MutationObserver(checkForPageHeader)
  if (mainEl.value) {
    observer.observe(mainEl.value, { childList: true, subtree: true })
  }
})

onBeforeUnmount(() => {
  observer?.disconnect()
  observer = null
})
</script>

<template>
  <div class="min-h-screen bg-muted/50">
    <!-- skip-link：仅键盘聚焦时可见 -->
    <a
      href="#main-content"
      class="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-primary focus:px-4 focus:py-2 focus:text-white focus:outline-none"
    >{{ t('common.skipToContent') }}</a>

    <AppHeader v-if="!pageHasOwnHeader" />

    <main id="main-content" ref="mainEl" tabindex="-1">
      <slot />
    </main>

    <CommandPalette />
  </div>
</template>
