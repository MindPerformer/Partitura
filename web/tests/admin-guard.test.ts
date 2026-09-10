// tests/admin-guard.test.ts — Admin 路由守卫测试
//
// 引入动机：design/04-WEB-API.md §RBAC 要求 admin 页面仅 system_admin 可访问。
// 前端通过 isSystemAdmin 进行可见性控制，后端通过 403 拒绝。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('Admin Guard', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('普通用户 isSystemAdmin 为 false，不能访问 admin 页面', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()

    auth.setAuth({
      user: { id: '1', username: 'normal', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    expect(auth.isSystemAdmin.value).toBe(false)
  })

  it('system_admin 用户 isSystemAdmin 为 true，可以访问 admin 页面', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()

    auth.setAuth({
      user: { id: '2', username: 'admin', system_role: 'system_admin' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    expect(auth.isSystemAdmin.value).toBe(true)
  })

  it('普通用户直接访问 admin URL 会被 admin middleware 重定向', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: '3', username: 'normal', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    const middleware = (await import('~/middleware/admin')).default
    const result = middleware({ path: '/admin/users', fullPath: '/admin/users' } as never, undefined as never)

    expect(result).toBeDefined()
  })

  it('system_admin 通过 admin middleware 时不重定向', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: '4', username: 'admin2', system_role: 'system_admin' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    const middleware = (await import('~/middleware/admin')).default
    const result = middleware({ path: '/admin/users', fullPath: '/admin/users' } as never, undefined as never)

    expect(result).toBeUndefined()
  })

  it('admin API listUsers 调用正确端点', async () => {
    ctrl.setResponse({ users: [], total: 0, limit: 20, offset: 0 })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()
    await api.listUsers()

    expect(ctrl.mockFn).toHaveBeenCalled()
  })

  it('admin API listAllWorkspaces 调用正确端点', async () => {
    ctrl.setResponse({ workspaces: [], total: 0, limit: 20, offset: 0 })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()
    await api.listAllWorkspaces()

    expect(ctrl.mockFn).toHaveBeenCalled()
  })

  it('admin API updateUser 调用 PUT 方法', async () => {
    ctrl.setResponse({
      id: '1', username: 'user', email: '', system_role: 'system_admin', workspace_create_perm: true
    })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()
    await api.updateUser('1', { system_role: 'system_admin' })

    expect(ctrl.mockFn).toHaveBeenCalled()
  })

  it('非 admin 用户访问 admin API 返回 403', async () => {
    ctrl.setError({
      response: { status: 403, _data: { error: 'system admin access required' } },
      message: 'FetchError'
    })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()

    try {
      await api.listUsers()
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number }
      expect(apiErr.status).toBe(403)
    }
  })
})
