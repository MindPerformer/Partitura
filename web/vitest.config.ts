// vitest.config.ts — Vitest 配置
//
// 引入动机：design/06-IMPLEMENTATION.md Phase5 要求测试覆盖核心交互。
// 使用 @nuxt/test-utils 的 nuxt 环境提供 Nuxt app 上下文，
// 使 useState、useCookie、useRuntimeConfig 等 auto-import 正常工作。
//
// hookTimeout 和 teardownTimeout 提高：
//   @nuxt/test-utils 的 setupNuxt 在 CI / 离线环境需要更长时间，
//   特别是 @nuxt/fonts 和 @nuxt/icon 模块的初始化。
import { defineVitestConfig } from '@nuxt/test-utils/config'

export default defineVitestConfig({
  test: {
    environment: 'nuxt',
    globals: true,
    include: ['tests/**/*.test.ts'],
    setupFiles: ['tests/setup.ts'],
    hookTimeout: 60000,
    teardownTimeout: 30000
  }
})
