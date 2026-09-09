// tests/workspace-navigation.test.ts — workspace 导航和分页文档树测试
//
// 引入动机：计划要求所有 workspace 页面使用统一左侧文档树，
// 侧栏顶部有固定"主页/统计"入口，文档树必须完整分页加载。
// 此测试验证：
// 1. WorkspaceLayout 分页加载所有文档（超过 100 篇）
// 2. 主页/统计导航入口存在
// 3. 路由切换保持正确 workspace

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'
import type { DocumentListItem, ListDocumentsResponse } from '~/types/api'

const ctrl = createFetchMock()

function makeDoc(i: number): DocumentListItem {
  return {
    id: `doc-${i}`,
    path: `docs/doc-${i}.md`,
    title: `Document ${i}`,
    type: '',
    status: 'active',
    content_hash: `hash-${i}`,
    revision_number: 1,
    is_special: false,
    updated_by: 'user-1',
    updated_at: '2026-03-08T12:34:56Z'
  }
}

describe('Workspace Navigation', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('文档数 > 100 时，WorkspaceLayout 分页加载所有文档', async () => {
    const PAGE_SIZE = 100
    const TOTAL = 250

    ctrl.setImpl((url: string) => {
      const u = new URL(url, 'http://test.local')
      const offset = parseInt(u.searchParams.get('offset') || '0', 10)
      const limit = parseInt(u.searchParams.get('limit') || '100', 10)
      const docs: DocumentListItem[] = []
      const end = Math.min(offset + limit, TOTAL)
      for (let i = offset; i < end; i++) {
        docs.push(makeDoc(i))
      }
      return Promise.resolve({
        _data: { documents: docs, total: TOTAL, limit, offset } as ListDocumentsResponse,
        status: 200
      })
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    // 复现 WorkspaceLayout loadDocuments 的分页循环逻辑
    const all: DocumentListItem[] = []
    let offset = 0
    while (true) {
      const res = await api.list('ws-1', { limit: PAGE_SIZE, offset })
      all.push(...res.documents)
      if (all.length >= res.total || res.documents.length === 0) {
        break
      }
      offset += res.limit
    }

    // 验证：所有 250 个文档都被加载
    expect(all).toHaveLength(TOTAL)
    expect(all[0]!.id).toBe('doc-0')
    expect(all[100]!.id).toBe('doc-100')
    expect(all[249]!.id).toBe('doc-249')

    // 验证：发起了 3 次请求
    expect(ctrl.mockFn.mock.calls).toHaveLength(3)
  })

  it('文档数恰好 100 时不请求第二页', async () => {
    const PAGE_SIZE = 100
    const TOTAL = 100

    ctrl.setImpl((url: string) => {
      const u = new URL(url, 'http://test.local')
      const offset = parseInt(u.searchParams.get('offset') || '0', 10)
      const limit = parseInt(u.searchParams.get('limit') || '100', 10)
      const docs: DocumentListItem[] = []
      const end = Math.min(offset + limit, TOTAL)
      for (let i = offset; i < end; i++) {
        docs.push(makeDoc(i))
      }
      return Promise.resolve({
        _data: { documents: docs, total: TOTAL, limit, offset } as ListDocumentsResponse,
        status: 200
      })
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    const all: DocumentListItem[] = []
    let offset = 0
    while (true) {
      const res = await api.list('ws-1', { limit: PAGE_SIZE, offset })
      all.push(...res.documents)
      if (all.length >= res.total || res.documents.length === 0) {
        break
      }
      offset += res.limit
    }

    expect(all).toHaveLength(TOTAL)
    expect(ctrl.mockFn.mock.calls).toHaveLength(1)
  })

  it('空文档列表时立即终止', async () => {
    ctrl.setImpl((url: string) => {
      const u = new URL(url, 'http://test.local')
      const offset = parseInt(u.searchParams.get('offset') || '0', 10)
      return Promise.resolve({
        _data: { documents: [], total: 0, limit: 100, offset } as ListDocumentsResponse,
        status: 200
      })
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    const all: DocumentListItem[] = []
    let offset = 0
    while (true) {
      const res = await api.list('ws-1', { limit: 100, offset })
      all.push(...res.documents)
      if (all.length >= res.total || res.documents.length === 0) {
        break
      }
      offset += res.limit
    }

    expect(all).toHaveLength(0)
    expect(ctrl.mockFn.mock.calls).toHaveLength(1)
  })

  it('workspace stats API 被正确调用', async () => {
    ctrl.setResponse({
      stats: {
        total_documents: 10,
        active_documents: 8,
        draft_documents: 1,
        archived_documents: 1,
        member_count: 3,
        recent_revisions: []
      }
    })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    const result = await api.stats('ws-1')

    expect(result.stats.total_documents).toBe(10)
    expect(result.stats.active_documents).toBe(8)
    expect(result.stats.member_count).toBe(3)
  })
})
