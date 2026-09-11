// tests/rbac.test.ts — RBAC 可见性和 admin 路由拒绝测试
//
// 引入动机：design/04-WEB-API.md §RBAC 要求前端按后端 RBAC 进行可见性控制。
// admin route 拒绝普通用户。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('RBAC', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('system_admin 用户 isSystemAdmin 为 true', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()

    auth.setAuth({
      user: { id: '1', username: 'admin', system_role: 'system_admin' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    })

    expect(auth.isSystemAdmin.value).toBe(true)
  })

  it('普通用户 isSystemAdmin 为 false', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()

    auth.setAuth({
      user: { id: '2', username: 'user', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    })

    expect(auth.isSystemAdmin.value).toBe(false)
  })

  it('hasMinRole 正确判断角色层级', async () => {
    const { hasMinRole, WORKSPACE_ROLES } = await import('~/types/api')

    expect(hasMinRole(WORKSPACE_ROLES.OWNER, WORKSPACE_ROLES.VIEWER)).toBe(true)
    expect(hasMinRole(WORKSPACE_ROLES.OWNER, WORKSPACE_ROLES.OWNER)).toBe(true)
    expect(hasMinRole(WORKSPACE_ROLES.ADMIN, WORKSPACE_ROLES.OWNER)).toBe(false)
    expect(hasMinRole(WORKSPACE_ROLES.EDITOR, WORKSPACE_ROLES.ADMIN)).toBe(false)
    expect(hasMinRole(WORKSPACE_ROLES.EDITOR, WORKSPACE_ROLES.EDITOR)).toBe(true)
    expect(hasMinRole(WORKSPACE_ROLES.VIEWER, WORKSPACE_ROLES.EDITOR)).toBe(false)
    expect(hasMinRole(WORKSPACE_ROLES.VIEWER, WORKSPACE_ROLES.VIEWER)).toBe(true)
  })

  it('admin API 403 错误被正确处理', async () => {
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
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(403)
      expect(apiErr.error).toBe('system admin access required')
    }
  })

  it('空角色字符串 hasMinRole 返回 false', async () => {
    const { hasMinRole, WORKSPACE_ROLES } = await import('~/types/api')

    expect(hasMinRole('', WORKSPACE_ROLES.VIEWER)).toBe(false)
    expect(hasMinRole(null as never, WORKSPACE_ROLES.VIEWER)).toBe(false)
    expect(hasMinRole('unknown', WORKSPACE_ROLES.VIEWER)).toBe(false)
  })

  it('ROLE_RANK 层级正确', async () => {
    const { ROLE_RANK } = await import('~/types/api')

    expect(ROLE_RANK.owner!).toBeGreaterThan(ROLE_RANK.admin!)
    expect(ROLE_RANK.admin!).toBeGreaterThan(ROLE_RANK.editor!)
    expect(ROLE_RANK.editor!).toBeGreaterThan(ROLE_RANK.viewer!)
  })
})
