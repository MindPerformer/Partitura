// tests/pagination.test.ts — 分页参数测试

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

function firstCall(): { url: string; opts: Record<string, unknown> } {
  const calls = ctrl.mockFn.mock.calls
  if (calls.length === 0) throw new Error('No fetch calls recorded')
  return { url: calls[0]![0] as string, opts: calls[0]![1] as Record<string, unknown> }
}

describe('Pagination', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('workspace list 传递 limit/offset', async () => {
    ctrl.setResponse({ workspaces: [], total: 100, limit: 10, offset: 20 })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.list({ limit: 10, offset: 20 })

    const c = firstCall()
    expect(c.url).toContain('limit=10')
    expect(c.url).toContain('offset=20')
  })

  it('workspace list 默认无分页参数', async () => {
    ctrl.setResponse({ workspaces: [], total: 0, limit: 20, offset: 0 })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.list()

    const c = firstCall()
    expect(c.url).not.toContain('limit=')
    expect(c.url).not.toContain('offset=')
  })

  it('document list 传递分页和过滤参数', async () => {
    ctrl.setResponse({ documents: [], total: 0, limit: 20, offset: 0 })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    await api.list('ws-1', { limit: 20, offset: 0, status: 'active', include_archived: false })

    const c = firstCall()
    expect(c.url).toContain('limit=20')
    expect(c.url).toContain('offset=0')
    expect(c.url).toContain('status=active')
    expect(c.url).toContain('include_archived=false')
  })

  it('admin users list 传递分页', async () => {
    ctrl.setResponse({ users: [], total: 50, limit: 20, offset: 0 })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()
    await api.listUsers({ limit: 50, offset: 0 })

    const c = firstCall()
    expect(c.url).toContain('limit=50')
  })

  it('admin audit list 传递分页', async () => {
    ctrl.setResponse({ entries: [], total: 200, limit: 50, offset: 100 })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()
    await api.listAudit({ limit: 50, offset: 100 })

    const c = firstCall()
    expect(c.url).toContain('limit=50')
    expect(c.url).toContain('offset=100')
  })

  it('document history 传递 path 和分页', async () => {
    ctrl.setResponse({ revisions: [], total: 5, limit: 10, offset: 0 })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    await api.history('ws-1', 'test.md', { limit: 10, offset: 0 })

    const c = firstCall()
    expect(c.url).toContain('path=test.md')
    expect(c.url).toContain('limit=10')
    expect(c.url).toContain('offset=0')
  })

  it('search 请求 body 包含 limit/offset', async () => {
    ctrl.setResponse({
      results: [], total: 0, limit: 10, offset: 20,
      degraded: false, search_id: 's1', reranker_used: false
    })

    const { useSearchApi } = await import('~/composables/useApi')
    const api = useSearchApi()
    await api.search('ws-1', { query: 'test', mode: 'hybrid', limit: 10, offset: 20 })

    const c = firstCall()
    const body = JSON.parse(c.opts.body as string)
    expect(body.limit).toBe(10)
    expect(body.offset).toBe(20)
  })
})
