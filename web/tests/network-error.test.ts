// tests/network-error.test.ts — 网络错误和 HTTP 错误状态码处理测试
//
// 引入动机：design/04-WEB-API.md §Error Handling 要求正确处理 401/403/404/409 和网络错误。
// 网络错误不应静默吞掉，应返回有意义的错误信息。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('Network Error Handling', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('404 错误返回 status 404 和 error message', async () => {
    ctrl.setError({
      response: { status: 404, _data: { error: 'document not found' } },
      message: 'FetchError'
    })

    const { useDocumentApi } = await import('~/composables/useApi')
    const api = useDocumentApi()

    try {
      await api.read('ws-1', 'nonexistent.md')
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(404)
      expect(apiErr.error).toBe('document not found')
    }
  })

  it('403 错误返回 status 403 和 error message', async () => {
    ctrl.setError({
      response: { status: 403, _data: { error: 'access denied' } },
      message: 'FetchError'
    })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    try {
      await api.get('ws-forbidden')
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(403)
      expect(apiErr.error).toBe('access denied')
    }
  })

  it('409 错误返回 status 409 和 conflict message', async () => {
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

  it('网络错误（无 response）返回 status 0', async () => {
    ctrl.setError({ message: 'Failed to fetch: network error' })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    try {
      await api.list()
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(0)
      expect(apiErr.error).toBe('Failed to fetch: network error')
    }
  })

  it('500 服务器错误返回 status 500', async () => {
    ctrl.setError({
      response: { status: 500, _data: { error: 'internal server error' } },
      message: 'FetchError'
    })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    try {
      await api.list()
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(500)
      expect(apiErr.error).toBe('internal server error')
    }
  })

  it('错误响应无 _data.error 时使用 message', async () => {
    ctrl.setError({ response: { status: 502 }, message: 'Bad Gateway' })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    try {
      await api.list()
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(502)
      expect(apiErr.error).toBe('Bad Gateway')
    }
  })

  it('错误响应无 response 和 message 时返回默认错误', async () => {
    ctrl.setError({})

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    try {
      await api.list()
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(0)
      expect(apiErr.error).toBe('网络请求失败')
    }
  })
})
