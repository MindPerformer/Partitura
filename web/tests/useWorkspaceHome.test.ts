// tests/useWorkspaceHome.test.ts — 工作区主页数据加载回归测试
//
// 引入动机：工作区主页曾在 immediate watch 执行时访问尚未初始化的 ref，
// 触发 Cannot access 'Q' before initialization。测试通过真实调用 composable
// 验证统计和 PROJECT.md 请求都能完成，避免退化为源码 contain 测试。

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { ref } from 'vue'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

const projectDocument = {
  id: 'doc-project',
  path: 'PROJECT.md',
  title: 'Project',
  type: 'markdown',
  status: 'active',
  content_hash: 'project-hash',
  revision_number: 1,
  is_special: true,
  updated_by: 'user-1',
  updated_at: '2026-03-08T12:00:00Z',
  content_markdown: '# Project\n'
}

const workspaceStats = {
  total_documents: 10,
  active_documents: 8,
  draft_documents: 1,
  archived_documents: 1,
  member_count: 3,
  recent_revisions: []
}

describe('useWorkspaceHome', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('immediate 加载会真实请求统计和 PROJECT.md，避免 TDZ 回归', async () => {
    ctrl.setImpl((url: string) => {
      if (url.includes('/workspaces/ws-1/stats')) {
        return Promise.resolve({ _data: { stats: workspaceStats }, status: 200 })
      }
      if (url.includes('/workspaces/ws-1/documents/')) {
        return Promise.resolve({ _data: projectDocument, status: 200 })
      }
      throw new Error(`Unexpected URL: ${url}`)
    })

    const { useWorkspaceHome } = await import('~/composables/useWorkspaceHome')
    const home = useWorkspaceHome(ref('ws-1'))

    // 若 TDZ 被重新引入，loadStats 会在 statsLoading.value 赋值时失败，
    // 请求不会发出，以下真实行为断言会失败。
    await vi.waitFor(() => {
      expect(ctrl.mockFn.mock.calls.some(call => String(call[0]).includes('/workspaces/ws-1/stats'))).toBe(true)
      expect(home.stats.value?.total_documents).toBe(10)
      expect(home.projectDoc.value?.path).toBe('PROJECT.md')
    })

    expect(home.statsLoading.value).toBe(false)
    expect(home.statsError.value).toBeNull()
    expect(ctrl.mockFn.mock.calls.some(call => {
      const url = String(call[0])
      return url.includes('/workspaces/ws-1/documents/read') && url.includes('path=PROJECT.md')
    })).toBe(true)
  })

  it('统计 API 失败时写入 API error 信息', async () => {
    ctrl.setImpl((url: string) => {
      if (url.includes('/workspaces/ws-1/stats')) {
        return Promise.reject({
          response: { status: 503, _data: { error: 'stats service unavailable' } },
          message: 'FetchError'
        })
      }
      if (url.includes('/workspaces/ws-1/documents/')) {
        return Promise.resolve({ _data: projectDocument, status: 200 })
      }
      throw new Error(`Unexpected URL: ${url}`)
    })

    const { useWorkspaceHome } = await import('~/composables/useWorkspaceHome')
    const home = useWorkspaceHome(ref('ws-1'))

    await vi.waitFor(() => {
      expect(home.statsLoading.value).toBe(false)
      expect(home.statsError.value).toBe('stats service unavailable')
    })
    expect(home.stats.value).toBeNull()
  })

  it('PROJECT.md 404 时保留空文档状态且不影响统计', async () => {
    ctrl.setImpl((url: string) => {
      if (url.includes('/workspaces/ws-1/stats')) {
        return Promise.resolve({ _data: { stats: workspaceStats }, status: 200 })
      }
      if (url.includes('/workspaces/ws-1/documents/')) {
        return Promise.reject({
          response: { status: 404, _data: { error: 'not found' } },
          message: 'FetchError'
        })
      }
      throw new Error(`Unexpected URL: ${url}`)
    })

    const { useWorkspaceHome } = await import('~/composables/useWorkspaceHome')
    const home = useWorkspaceHome(ref('ws-1'))

    await vi.waitFor(() => {
      expect(home.stats.value?.total_documents).toBe(10)
      expect(home.projectLoading.value).toBe(false)
    })

    expect(home.projectDoc.value).toBeNull()
    expect(home.statsError.value).toBeNull()
  })
})
