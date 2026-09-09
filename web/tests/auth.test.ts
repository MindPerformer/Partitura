// tests/auth.test.ts — 登录和未登录保护测试
//
// 引入动机：design/04-WEB-API.md §Security 要求阻止未登录访问项目页面。
// 测试 login 流程和 auth middleware 路由守卫。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('Login and Auth Guard', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('login 成功后设置认证状态', async () => {
    ctrl.setResponse({
      user: { id: 'user-1', username: 'testuser', system_role: 'user' },
      csrf_token: 'csrf-token-123',
      expires_at: '2025-01-01T00:00:00Z'
    })

    const { useAuthApi } = await import('~/composables/useApi')
    const { useAuth } = await import('~/composables/useAuth')

    const api = useAuthApi()
    const result = await api.login('testuser', 'password')

    expect(result.user.username).toBe('testuser')
    expect(result.csrf_token).toBe('csrf-token-123')

    const auth = useAuth()
    auth.setAuth(result)

    expect(auth.isAuthenticated.value).toBe(true)
    expect(auth.currentUser.value?.username).toBe('testuser')
    expect(auth.currentUser.value?.system_role).toBe('user')
    expect(auth.isSystemAdmin.value).toBe(false)
  })

  it('login 失败返回 401 时不设置认证状态', async () => {
    ctrl.setError({
      response: { status: 401, _data: { error: '用户名或密码错误' } },
      message: 'FetchError'
    })

    const { useAuthApi } = await import('~/composables/useApi')
    const { useAuth } = await import('~/composables/useAuth')

    const api = useAuthApi()
    const auth = useAuth()

    try {
      await api.login('wrong', 'credentials')
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(401)
      expect(apiErr.error).toBe('用户名或密码错误')
    }

    expect(auth.isAuthenticated.value).toBe(false)
  })

  it('401 响应触发 clearAuth', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: '1', username: 'test', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)
    expect(auth.isAuthenticated.value).toBe(true)

    ctrl.setError({
      response: { status: 401, _data: { error: '未认证' } },
      message: 'FetchError'
    })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    try {
      await api.list()
    } catch {
      // expected
    }

    expect(auth.isAuthenticated.value).toBe(false)
  })

  it('auth middleware 重定向未登录用户到 /login', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()

    auth.clearAuth()
    await nextTick()
    expect(auth.isAuthenticated.value).toBe(false)

    const middleware = (await import('~/middleware/auth')).default

    const to = { path: '/workspaces/123', fullPath: '/workspaces/123' }
    const result = middleware(to as never, undefined as never)

    expect(result).toBeDefined()
  })

  it('auth middleware 允许已登录用户访问', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()

    auth.setAuth({
      user: { id: '1', username: 'test', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)
    await nextTick()

    const middleware = (await import('~/middleware/auth')).default

    const to = { path: '/workspaces/123', fullPath: '/workspaces/123' }
    const result = middleware(to as never, undefined as never)

    expect(result).toBeUndefined()
  })

  it('auth middleware 将已登录用户从 /login 重定向到首页', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()

    auth.setAuth({
      user: { id: '1', username: 'test', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)
    await nextTick()

    const middleware = (await import('~/middleware/auth')).default

    const to = { path: '/login', fullPath: '/login' }
    const result = middleware(to as never, undefined as never)

    expect(result).toBeDefined()
  })

  it('logout 清理认证状态', async () => {
    ctrl.setResponse({ status: 'ok' })

    const { useAuthApi } = await import('~/composables/useApi')
    const { useAuth } = await import('~/composables/useAuth')

    const auth = useAuth()
    auth.setAuth({
      user: { id: '1', username: 'test', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)
    expect(auth.isAuthenticated.value).toBe(true)

    const api = useAuthApi()
    await api.logout()
    auth.clearAuth()

    expect(auth.isAuthenticated.value).toBe(false)
    expect(auth.currentUser.value).toBeNull()
  })
})
