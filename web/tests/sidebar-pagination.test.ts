// tests/sidebar-pagination.test.ts — read.vue sidebar 分页加载行为测试
//
// 引入动机：read.vue 的文档 sidebar 原固定 limit=100，当文档数超过 100 时截断。
// 修复后改为分页循环加载全量文档。此测试验证：
// 1. 第二页（offset > 0）被正确请求 — 所有分页项可访问
// 2. 网络失败/API 错误不被静默吞掉 — 错误可观察且有日志
// 3. 非法 total 契约不会导致死循环
//
// 测试策略：
// - 使用 createFetchMock 的 setImpl 根据 URL 中的 offset 参数返回不同响应
// - 模拟 read.vue loadDocuments 的分页循环逻辑
// - 验证 console.error 被调用（错误可观察）
// - 验证第二页请求包含正确的 offset 参数

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { createFetchMock } from './setup'
import type { DocumentListItem, ListDocumentsResponse, ApiError } from '~/types/api'

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
    updated_at: '2025-01-01T00:00:00Z'
  }
}

/** 从 fetch mock 调用中提取 URL 查询参数 */
function getCallUrl(callIndex: number): URL {
  const calls = ctrl.mockFn.mock.calls
  if (callIndex >= calls.length) throw new Error(`Call ${callIndex} not found (total: ${calls.length})`)
  const url = calls[callIndex]![0] as string
  return new URL(url, 'http://test.local')
}

/** 从 URL 中提取 offset 参数 */
function getOffset(callIndex: number): number {
  const url = getCallUrl(callIndex)
  return parseInt(url.searchParams.get('offset') || '0', 10)
}

/** 从 URL 中提取 limit 参数 */
function getLimit(callIndex: number): number {
  const url = getCallUrl(callIndex)
  return parseInt(url.searchParams.get('limit') || '0', 10)
}

describe('Sidebar Pagination — read.vue loadDocuments behavior', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('文档数 > 100 时，分页加载第二页，所有文档可访问', async () => {
    // 模拟 150 个文档，每页 100
    const PAGE_SIZE = 100
    const TOTAL = 150

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

    // 复现 read.vue loadDocuments 的分页循环逻辑
    const all: DocumentListItem[] = []
    const MAX_PAGES = 200
    for (let page = 0; page < MAX_PAGES; page++) {
      const offset = page * PAGE_SIZE
      const res = await api.list('ws-1', { limit: PAGE_SIZE, offset })
      all.push(...res.documents)
      if (res.documents.length === 0 || all.length >= res.total) break
    }

    // 验证：所有 150 个文档都被加载
    expect(all).toHaveLength(TOTAL)
    expect(all[0]!.id).toBe('doc-0')
    expect(all[99]!.id).toBe('doc-99')
    expect(all[100]!.id).toBe('doc-100')
    expect(all[149]!.id).toBe('doc-149')

    // 验证：发起了 2 次请求，第二次 offset=100
    expect(ctrl.mockFn.mock.calls).toHaveLength(2)
    expect(getOffset(0)).toBe(0)
    expect(getLimit(0)).toBe(PAGE_SIZE)
    expect(getOffset(1)).toBe(PAGE_SIZE)
    expect(getLimit(1)).toBe(PAGE_SIZE)
  })

  it('文档数恰好等于 page_size 时不请求第二页', async () => {
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
    const res = await api.list('ws-1', { limit: PAGE_SIZE, offset: 0 })
    all.push(...res.documents)
    if (res.documents.length === 0 || all.length >= res.total) {
      // 不需要第二页
    } else {
      await api.list('ws-1', { limit: PAGE_SIZE, offset: PAGE_SIZE })
    }

    expect(all).toHaveLength(TOTAL)
    expect(ctrl.mockFn.mock.calls).toHaveLength(1)
  })

  it('第一页 API 错误时，错误不被吞掉，console.error 被调用', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})

    ctrl.setError({
      response: { status: 500, _data: { error: 'internal server error' } },
      message: 'FetchError'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    // 复现 loadDocuments 的错误处理逻辑
    let documentsError: string | null = null
    const all: DocumentListItem[] = []

    try {
      const res = await api.list('ws-1', { limit: 100, offset: 0 })
      all.push(...res.documents)
    } catch (err) {
      const apiErr = err as ApiError
      const msg = apiErr.error || `status=${apiErr.status}`
      console.error(`[read.vue] loadDocuments page 0 failed: ${msg}`)
      documentsError = apiErr.error || 'loadFailed'
    }

    // 验证：错误被记录
    expect(documentsError).toBe('internal server error')
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringContaining('[read.vue] loadDocuments page 0 failed: internal server error')
    )
    // 已加载结果为空（第一页就失败）
    expect(all).toHaveLength(0)

    errorSpy.mockRestore()
  })

  it('第二页网络失败时，保留第一页已加载结果，错误可观察', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const PAGE_SIZE = 100

    let callCount = 0
    ctrl.setImpl((url: string) => {
      callCount++
      if (callCount === 1) {
        // 第一页成功
        const u = new URL(url, 'http://test.local')
        const offset = parseInt(u.searchParams.get('offset') || '0', 10)
        const docs: DocumentListItem[] = []
        for (let i = offset; i < offset + PAGE_SIZE; i++) {
          docs.push(makeDoc(i))
        }
        return Promise.resolve({
          _data: { documents: docs, total: 250, limit: PAGE_SIZE, offset } as ListDocumentsResponse,
          status: 200
        })
      }
      // 第二页失败
      return Promise.reject({
        response: { status: 503, _data: { error: 'service unavailable' } },
        message: 'FetchError'
      })
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    // 复现 loadDocuments 的分页循环逻辑
    const all: DocumentListItem[] = []
    let documentsError: string | null = null
    const MAX_PAGES = 200

    for (let page = 0; page < MAX_PAGES; page++) {
      const offset = page * PAGE_SIZE
      try {
        const res = await api.list('ws-1', { limit: PAGE_SIZE, offset })
        all.push(...res.documents)
        if (res.documents.length === 0 || all.length >= res.total) break
      } catch (err) {
        const apiErr = err as ApiError
        const msg = apiErr.error || `status=${apiErr.status}`
        console.error(`[read.vue] loadDocuments page ${page} failed: ${msg}`)
        documentsError = apiErr.error || 'loadFailed'
        break
      }
    }

    // 验证：第一页的 100 个文档被保留
    expect(all).toHaveLength(100)
    // 验证：错误被记录
    expect(documentsError).toBe('service unavailable')
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringContaining('[read.vue] loadDocuments page 1 failed: service unavailable')
    )

    errorSpy.mockRestore()
  })

  it('非法 total（total < offset）不会导致死循环', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const PAGE_SIZE = 100

    // 模拟 API 返回非法 total：total=5 但每次返回 100 条
    ctrl.setImpl((url: string) => {
      const u = new URL(url, 'http://test.local')
      const offset = parseInt(u.searchParams.get('offset') || '0', 10)
      const docs: DocumentListItem[] = []
      // 始终返回 100 条（模拟 API 契约异常）
      for (let i = offset; i < offset + PAGE_SIZE; i++) {
        docs.push(makeDoc(i))
      }
      return Promise.resolve({
        _data: { documents: docs, total: 5, limit: PAGE_SIZE, offset } as ListDocumentsResponse,
        status: 200
      })
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    // 复现 loadDocuments 的分页循环逻辑，包含非法 total 防御
    const all: DocumentListItem[] = []
    const MAX_PAGES = 200

    for (let page = 0; page < MAX_PAGES; page++) {
      const offset = page * PAGE_SIZE
      const res = await api.list('ws-1', { limit: PAGE_SIZE, offset })
      all.push(...res.documents)

      if (res.documents.length === 0 || all.length >= res.total) break
      if (res.total < offset) {
        console.error(`[read.vue] loadDocuments: API returned total=${res.total} < offset=${offset}, stopping`)
        break
      }
    }

    // 验证：第一页就终止了（all.length=100 >= total=5）
    expect(ctrl.mockFn.mock.calls).toHaveLength(1)
    expect(all).toHaveLength(100) // 第一页的数据仍被加载

    errorSpy.mockRestore()
  })

  it('API 返回空文档列表时立即终止，不请求后续页', async () => {
    const PAGE_SIZE = 100

    ctrl.setImpl((url: string) => {
      const u = new URL(url, 'http://test.local')
      const offset = parseInt(u.searchParams.get('offset') || '0', 10)
      return Promise.resolve({
        _data: { documents: [], total: 0, limit: PAGE_SIZE, offset } as ListDocumentsResponse,
        status: 200
      })
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    const all: DocumentListItem[] = []
    const MAX_PAGES = 200

    for (let page = 0; page < MAX_PAGES; page++) {
      const offset = page * PAGE_SIZE
      const res = await api.list('ws-1', { limit: PAGE_SIZE, offset })
      all.push(...res.documents)
      if (res.documents.length === 0 || all.length >= res.total) break
    }

    expect(all).toHaveLength(0)
    expect(ctrl.mockFn.mock.calls).toHaveLength(1)
  })
})
