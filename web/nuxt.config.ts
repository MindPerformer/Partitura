// nuxt.config.ts — Nuxt 前端配置
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 Nuxt + Nuxt UI + TypeScript。
// 配置 CSR、API 代理、Nuxt UI v4 模块、TypeScript strict 模式、CSS 入口。
//
// 生产架构：Nuxt CSR 静态构建 + Nginx runtime。
// 浏览器仅使用相对路径 /api，Nginx 反向代理 /api 到 Go 后端。
// 开发环境通过 nitro devProxy 代理到本地 Go 后端，不进入生产路径。
//
// Nuxt UI v4 官方安装要求：
// - modules: ['@nuxt/ui']（自动注册 @nuxt/icon、@nuxt/fonts、@nuxtjs/color-mode）
// - CSS 入口按官方顺序导入 tailwindcss 和 @nuxt/ui
// - app.vue 使用 <UApp> 包裹
//
// Phase 6 修复：关闭 @nuxt/fonts 的 Google Fonts provider，避免测试时
// 发起外网请求导致超时。@nuxt/fonts 的 configKey 为 "fonts"，
// 将 providers.google 和 providers.googleicons 设为 false 即可从 provider
// 列表中移除（fontless resolveProviders 会删除值为 false 的 provider）。
// 保留 local provider 用于本地字体解析，不破坏 Nuxt UI v4 的字体能力。
//
// Phase 6 i18n：注册 @nuxtjs/i18n 模块，默认简体中文，支持 English 切换。
// 使用 no_prefix 策略不改变现有 URL，cookie 持久化语言选择。
export default defineNuxtConfig({
  modules: ['@nuxt/ui', '@nuxtjs/i18n'],
  devtools: { enabled: true },
  // Phase 6 E：明确使用 CSR（客户端渲染）模式，减少服务端敏感状态。
  // 生产环境由 Nginx 提供静态文件和 SPA fallback，不需要 Nitro 服务端运行时。
  ssr: false,
  css: ['~/assets/css/main.css'],
  // 关闭 Google Fonts 网络依赖。
  // 引入动机：@nuxt/fonts 默认启用 google 和 googleicons provider，
  // 在测试初始化时向 fonts.googleapis.com 发起请求导致离线环境超时。
  // 设为 false 后 fontless 的 resolveProviders 会从 provider 列表中删除它们，
  // 仅保留 local provider 解析本地字体文件，不影响 Nuxt UI v4 功能。
  fonts: {
    providers: {
      google: false,
      googleicons: false
    }
  },
  // i18n 配置：简体中文默认，English 可切换，no_prefix 不改变 URL。
  // 引入动机：Phase 6 要求标准完整中英 i18n，使用 @nuxtjs/i18n 官方方案。
  // no_prefix 策略使 URL 不包含 locale 前缀，保持现有路由不变。
  // cookie 持久化使用户刷新后语言选择保持不变。
  // 新增语言只需在此添加 locale 条目并创建对应 JSON 词典文件。
  i18n: {
    locales: [
      { code: 'zh', language: 'zh-CN', name: '简体中文', file: 'zh.json' },
      { code: 'en', language: 'en-US', name: 'English', file: 'en.json' }
    ],
    defaultLocale: 'zh',
    strategy: 'no_prefix',
    langDir: '../locales',
    detectBrowserLanguage: {
      useCookie: true,
      cookieKey: 'i18n_locale',
      redirectOn: 'root'
    }
  },
  typescript: {
    strict: true,
    typeCheck: true
  },
  // 运行时配置：
  // - public.apiBase: 浏览器端 API 基础路径，始终为相对路径 /api，不暴露内部 hostname
  //
  // 生产环境由 Nginx 反向代理 /api 到 Go 后端，浏览器仅使用相对路径 /api。
  // 开发环境通过 nitro devProxy 代理到本地 Go 后端。
  // 不再需要 serverInternalUrl——Nginx 在运行时从环境变量获取 upstream 地址。
  runtimeConfig: {
    public: {
      apiBase: '/api'
    }
  },
  // 开发环境代理 API 请求到 Go 后端
  // 生产环境由 Nginx 反向代理 /api，此配置仅用于 dev server，不进入生产构建产物。
  // 引入动机：design/05-OPERATIONS.md 要求 web 代理 /api 到内部 server，
  // 而非让浏览器直接访问容器 hostname。生产环境由 Nginx 处理，
  // 开发环境通过 devProxy 实现相同效果。
  nitro: {
    devProxy: {
      '/api': {
        target: process.env.API_PROXY_TARGET || 'http://localhost:8080',
        changeOrigin: true
      }
    }
  },
  app: {
    head: {
      title: 'Partitura',
      meta: [
        { name: 'viewport', content: 'width=device-width, initial-scale=1' },
        { name: 'description', content: 'Partitura — 内部知识库' },
        // 内部知识库：禁止搜索引擎收录。
        { name: 'robots', content: 'noindex,nofollow' }
      ]
    }
  }
})
