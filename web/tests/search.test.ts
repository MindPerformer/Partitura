// tests/search.test.ts — Search API 测试（仅当前 workspace + degraded 状态）

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

function firstCall(): { url: string; opts: Record<string, unknown> } {
  const calls = ctrl.mockFn.mock.calls
  if (calls.length === 0) throw new Error('No fetch calls recorded')
  return { url: calls[0]![0] as string, opts: calls[0]![1] as Record<string, unknown> }
}

describe('Search API', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('search 请求 URL 包含 workspace ID', async () => {
    ctrl.setResponse({
      results: [], total: 0, limit: 10, offset: 0,
      degraded: false, search_id: 'search-1', reranker_used: false
    })

    const { useSearchApi } = await import('~/composables/useApi')
    const api = useSearchApi()
    await api.search('ws-123', { query: 'test', mode: 'hybrid', limit: 10, offset: 0 })

    const c = firstCall()
    expect(c.url).toContain('/workspaces/ws-123/search')
    const body = JSON.parse(c.opts.body as string)
    expect(body.query).toBe('test')
    expect(body.mode).toBe('hybrid')
  })

  it('degraded 状态正确返回', async () => {
    ctrl.setResponse({
      results: [], total: 0, limit: 10, offset: 0,
      degraded: true, degradation_reason: 'elasticsearch_unavailable',
      search_id: 'search-2', reranker_used: false
    })

    const { useSearchApi } = await import('~/composables/useApi')
    const api = useSearchApi()
    const res = await api.search('ws-1', { query: 'test', mode: 'hybrid', limit: 10, offset: 0 })

    expect(res.degraded).toBe(true)
    expect(res.degradation_reason).toBe('elasticsearch_unavailable')
  })

  it('search 结果包含 path/title/snippet/score', async () => {
    ctrl.setResponse({
      results: [{
        document_id: 'doc-1', path: 'architecture/overview.md', title: 'Overview',
        section_path: ['Architecture'], start_line: 1, end_line: 20,
        snippet: 'Architecture overview...', score: 0.95, revision: 3, rank: 1
      }],
      total: 1, limit: 10, offset: 0,
      degraded: false, search_id: 'search-3', reranker_used: true
    })

    const { useSearchApi } = await import('~/composables/useApi')
    const api = useSearchApi()
    const res = await api.search('ws-1', { query: 'architecture', mode: 'hybrid', limit: 10, offset: 0 })

    expect(res.results).toHaveLength(1)
    expect(res.results[0]!.path).toBe('architecture/overview.md')
    expect(res.results[0]!.score).toBe(0.95)
    expect(res.reranker_used).toBe(true)
  })

  it('search 是 POST 请求携带 CSRF header', async () => {
    if (typeof document !== 'undefined') {
      document.cookie = 'csrf=token-search'
    }

    ctrl.setResponse({
      results: [], total: 0, limit: 10, offset: 0,
      degraded: false, search_id: 's', reranker_used: false
    })

    const { useSearchApi } = await import('~/composables/useApi')
    const api = useSearchApi()
    await api.search('ws-1', { query: 'test', mode: 'hybrid', limit: 10, offset: 0 })

    const c = firstCall()
    expect(c.opts.method).toBe('POST')
    const headers = c.opts.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('token-search')

    if (typeof document !== 'undefined') {
      document.cookie = 'csrf=; expires=Thu, 01 Jan 1970 00:00:00 GMT; path=/'
    }
  })

  it('feedback 请求携带必要字段', async () => {
    ctrl.setResponse({ status: 'recorded' })

    const { useSearchApi } = await import('~/composables/useApi')
    const api = useSearchApi()
    await api.feedback('ws-1', {
      feedback_type: 'verified', query: 'test',
      document_id: 'doc-1', search_id: 'search-1'
    })

    const c = firstCall()
    expect(c.url).toContain('/workspaces/ws-1/search/feedback')
    const body = JSON.parse(c.opts.body as string)
    expect(body.feedback_type).toBe('verified')
    expect(body.document_id).toBe('doc-1')
  })
})
