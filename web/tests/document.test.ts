// tests/document.test.ts — Document reader/editor/409 conflict 测试
//
// 引入动机：design/04-WEB-API.md §Concurrency 要求 409 显示明确冲突界面。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

function firstCall(): { url: string; opts: Record<string, unknown> } {
  const calls = ctrl.mockFn.mock.calls
  if (calls.length === 0) throw new Error('No fetch calls recorded')
  return { url: calls[0]![0] as string, opts: calls[0]![1] as Record<string, unknown> }
}

describe('Document API', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('read 请求使用 path 查询参数', async () => {
    ctrl.setResponse({
      id: 'doc-1', workspace_id: 'ws-1', path: 'architecture/overview.md', title: 'Overview',
      status: 'active', content_markdown: '# Overview\n\nContent', content_hash: 'abc123',
      revision_number: 1, is_special: false, created_by: 'user-1', updated_by: 'user-1',
      created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    const doc = await api.read('ws-1', 'architecture/overview.md')

    expect(doc.path).toBe('architecture/overview.md')
    expect(doc.title).toBe('Overview')
    const c = firstCall()
    expect(c.url).toContain('/workspaces/ws-1/documents/read')
    expect(c.url).toContain('path=architecture')
    expect(c.url).toContain('overview.md')
  })

  it('outline 请求返回 heading tree', async () => {
    ctrl.setResponse({
      path: 'architecture/overview.md',
      outline: [
        { level: 1, text: 'Overview', start_line: 1, end_line: 10, section_path: ['Overview'] },
        { level: 2, text: 'Components', start_line: 3, end_line: 8, section_path: ['Overview', 'Components'] }
      ]
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    const outline = await api.outline('ws-1', 'architecture/overview.md')

    expect(outline.outline).toHaveLength(2)
    expect(outline.outline[0]!.level).toBe(1)
    expect(outline.outline[0]!.text).toBe('Overview')
    expect(outline.outline[1]!.section_path).toEqual(['Overview', 'Components'])
  })

  it('replace 请求携带 expected_revision 和 expected_hash', async () => {
    ctrl.setResponse({
      id: 'doc-1', workspace_id: 'ws-1', path: 'test.md', title: 'Test',
      status: 'active', content_markdown: '# Updated', content_hash: 'new-hash',
      revision_number: 2, is_special: false, created_by: 'user-1', updated_by: 'user-1',
      created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    await api.replace('ws-1', 'test.md', {
      title: 'Test', type: '', content_markdown: '# Updated',
      expected_revision: 1, expected_hash: 'old-hash'
    })

    const c = firstCall()
    expect(c.opts.method).toBe('PUT')
    const body = JSON.parse(c.opts.body as string)
    expect(body.expected_revision).toBe(1)
    expect(body.expected_hash).toBe('old-hash')
  })

  it('409 冲突返回 ApiError with status 409', async () => {
    ctrl.setError({
      response: { status: 409, _data: { error: '版本冲突：expected_revision/expected_hash 不匹配' } },
      message: 'FetchError'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    try {
      await api.replace('ws-1', 'test.md', {
        title: 'Test', type: '', content_markdown: '# Updated',
        expected_revision: 1, expected_hash: 'old-hash'
      })
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(409)
      expect(apiErr.error).toContain('版本冲突')
    }
  })

  it('archive 请求携带 expected_revision 和 expected_hash', async () => {
    ctrl.setResponse({
      id: 'doc-1', workspace_id: 'ws-1', path: 'test.md', title: 'Test',
      status: 'archived', content_markdown: '# Test', content_hash: 'hash',
      revision_number: 1, is_special: false, created_by: 'user-1', updated_by: 'user-1',
      created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    await api.archive('ws-1', 'test.md', { expected_revision: 1, expected_hash: 'hash' })

    const c = firstCall()
    expect(c.opts.method).toBe('POST')
    expect(c.url).toContain('/documents/archive')
    const body = JSON.parse(c.opts.body as string)
    expect(body.expected_revision).toBe(1)
    expect(body.expected_hash).toBe('hash')
  })

  it('patch 请求携带 candidate_hash 和 expected 字段', async () => {
    ctrl.setResponse({
      id: 'doc-1', workspace_id: 'ws-1', path: 'test.md', title: 'Test',
      status: 'active', content_markdown: '# Patched', content_hash: 'new-hash',
      revision_number: 2, is_special: false, created_by: 'user-1', updated_by: 'user-1',
      created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    await api.patch('ws-1', 'test.md', {
      content_markdown: '# Patched', candidate_hash: 'new-hash',
      expected_revision: 1, expected_hash: 'old-hash'
    })

    const c = firstCall()
    expect(c.opts.method).toBe('PATCH')
    const body = JSON.parse(c.opts.body as string)
    expect(body.candidate_hash).toBe('new-hash')
    expect(body.expected_revision).toBe(1)
    expect(body.expected_hash).toBe('old-hash')
  })

  it('history 请求使用 path 查询参数和分页', async () => {
    ctrl.setResponse({
      revisions: [
        { id: 'rev-1', document_id: 'doc-1', revision_number: 2, path: 'test.md', title: 'Test', content_markdown: '# v2', content_hash: 'h2', status: 'active', created_by: 'user-1', created_at: '2025-01-02T00:00:00Z' },
        { id: 'rev-2', document_id: 'doc-1', revision_number: 1, path: 'test.md', title: 'Test', content_markdown: '# v1', content_hash: 'h1', status: 'active', created_by: 'user-1', created_at: '2025-01-01T00:00:00Z' }
      ],
      total: 2, limit: 20, offset: 0
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    const res = await api.history('ws-1', 'test.md', { limit: 10, offset: 0 })

    expect(res.revisions).toHaveLength(2)
    expect(res.revisions[0]!.revision_number).toBe(2)
    const c = firstCall()
    expect(c.url).toContain('/documents/history')
    expect(c.url).toContain('path=test.md')
  })

  it('revision 请求使用 path 和 revision 查询参数', async () => {
    ctrl.setResponse({
      id: 'rev-1', document_id: 'doc-1', revision_number: 1, path: 'test.md',
      title: 'Test', content_markdown: '# v1', content_hash: 'h1', status: 'active',
      created_by: 'user-1', created_at: '2025-01-01T00:00:00Z'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()
    const rev = await api.revision('ws-1', 'test.md', 1)

    expect(rev.revision_number).toBe(1)
    const c = firstCall()
    expect(c.url).toContain('revision=1')
  })
})
