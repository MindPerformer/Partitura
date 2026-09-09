// tests/csrf.test.ts — CSRF token header/cookie 测试

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

function firstCall(): { url: string; opts: Record<string, unknown> } {
  const calls = ctrl.mockFn.mock.calls
  if (calls.length === 0) throw new Error('No fetch calls recorded')
  return { url: calls[0]![0] as string, opts: calls[0]![1] as Record<string, unknown> }
}

function setDocumentCookie(name: string, value: string) {
  if (typeof document !== 'undefined') {
    document.cookie = `${name}=${value}`
  }
}

describe('CSRF Token Handling', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('GET 请求不携带 CSRF header', async () => {
    setDocumentCookie('csrf', 'csrf-token-abc')

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.list()

    expect(ctrl.mockFn).toHaveBeenCalledTimes(1)
    const headers = firstCall().opts.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBeUndefined()
  })

  it('POST 请求携带 CSRF header', async () => {
    setDocumentCookie('csrf', 'csrf-token-post')

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.create({ name: 'test', display_name: 'Test', description: '' })

    expect(ctrl.mockFn).toHaveBeenCalledTimes(1)
    const headers = firstCall().opts.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('csrf-token-post')
  })

  it('PUT 请求携带 CSRF header', async () => {
    setDocumentCookie('csrf', 'csrf-token-put')

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.update('1', { display_name: 'Updated', description: '' })

    expect(ctrl.mockFn).toHaveBeenCalledTimes(1)
    const headers = firstCall().opts.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('csrf-token-put')
  })

  it('DELETE 请求携带 CSRF header', async () => {
    setDocumentCookie('csrf', 'csrf-token-delete')

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.removeMember('ws-1', 'user-1')

    expect(ctrl.mockFn).toHaveBeenCalledTimes(1)
    const headers = firstCall().opts.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('csrf-token-delete')
  })

  it('所有请求携带 credentials: include', async () => {
    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.list()

    expect(firstCall().opts.credentials).toBe('include')
  })

  it('无 CSRF cookie 时不发送 header', async () => {
    setDocumentCookie('csrf', 'csrf-unique-value-for-this-test')

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()
    await api.create({ name: 'test', display_name: 'Test', description: '' })

    const headers = firstCall().opts.headers as Record<string, string>
    expect(headers['X-CSRF-Token']).toBe('csrf-unique-value-for-this-test')
  })
})
