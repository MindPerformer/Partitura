// tests/setup.ts — 测试环境初始化
//
// 引入动机：Vitest 测试需要 mock $fetch 以避免真实 HTTP 请求。
// 使用 @nuxt/test-utils 的 nuxt 环境提供 Nuxt app 上下文。
//
// Nuxt 4 + @nuxt/test-utils v4 变更：
// - Nuxt 环境注入了真实的 $fetch（ofetch 实现），
//   vi.stubGlobal('$fetch', ...) 会被 Nuxt 环境覆盖。
// - 正确做法：直接替换 $fetch.raw 属性为 vi.fn 实例。

import { vi } from 'vitest'

export interface FetchMockController {
  mockFn: ReturnType<typeof vi.fn>
  setResponse: (data: unknown) => void
  setError: (error: unknown) => void
  setImpl: (impl: (url: string, opts: unknown) => Promise<unknown>) => void
  reset: () => void
}

export function createFetchMock(): FetchMockController {
  const state = {
    response: null as unknown,
    error: null as unknown,
    impl: null as ((url: string, opts: unknown) => Promise<unknown>) | null
  }

  const mockFn = vi.fn(async (url: string, opts: unknown) => {
    if (state.impl) return state.impl(url, opts)
    if (state.error) throw state.error
    return { _data: state.response, status: 200 }
  })

  const fetchObj = $fetch as unknown as { raw: (url: string, opts: unknown) => Promise<unknown> }
  fetchObj.raw = mockFn

  return {
    mockFn,
    setResponse: (data: unknown) => {
      state.response = data
      state.error = null
    },
    setError: (error: unknown) => {
      state.error = error
      state.response = null
    },
    setImpl: (impl: (url: string, opts: unknown) => Promise<unknown>) => {
      state.impl = impl
    },
    reset: () => {
      state.response = null
      state.error = null
      state.impl = null
      mockFn.mockClear()
    }
  }
}
