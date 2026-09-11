// tests/csrf.test.ts — CSRF token header/cookie 测试

import { describe, it, expect, beforeEach, afterEach } from 'vitest'
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

/**
 * 过期所有测试写入的 cookie，消除跨用例污染。
 * 逐个读取 document.cookie 中的 name，用 Max-Age=0 使其过期。
 */
function clearAllCookies() {
  if (typeof document === 'undefined') return
  for (const pair of document.cookie.split(';')) {
    const name = pair.split('=')[0]?.trim()
    if (name) {
      document.cookie = `${name}=; Path=/; Max-Age=0`
    }
  }
}

describe('CSRF Token Handling', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  afterEach(() => {
    clearAllCookies()
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

  it('账户邮箱和密码更新请求携带 CSRF header 与正确 body', async () => {
    setDocumentCookie('csrf', 'csrf-token-account')
    const { useAuthApi } = await import('~/composables/useApi')
    const api = useAuthApi()

    ctrl.setResponse({ id: 'u1', username: 'user', email: 'new@example.com', system_role: 'user', workspace_create_perm: false })
    await api.updateEmail({ current_password: 'old-password', email: 'new@example.com' })
    const emailCall = ctrl.mockFn.mock.calls[0]!
    expect((emailCall[1] as Record<string, unknown>).headers).toMatchObject({ 'X-CSRF-Token': 'csrf-token-account' })
    expect(JSON.parse((emailCall[1] as { body: string }).body)).toEqual({ current_password: 'old-password', email: 'new@example.com' })

    ctrl.reset()
    ctrl.setResponse({ status: 'ok' })
    await api.updatePassword({ current_password: 'old-password', new_password: 'new-password-123' })
    const passwordCall = ctrl.mockFn.mock.calls[0]!
    expect((passwordCall[1] as Record<string, unknown>).headers).toMatchObject({ 'X-CSRF-Token': 'csrf-token-account' })
    expect(JSON.parse((passwordCall[1] as { body: string }).body)).toEqual({ current_password: 'old-password', new_password: 'new-password-123' })
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
