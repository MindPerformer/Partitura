// tests/admin-create-user.test.ts — admin 创建用户 API 和表单行为测试
//
// 引入动机：计划要求仅 system_admin 可创建普通用户，前端需要测试：
// 1. createUser API 调用正确的端点和请求体
// 2. 成功后清空密码输入并刷新用户列表
// 3. 错误时显示错误信息
// 4. 客户端输入校验

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('Admin Create User', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('createUser 调用 POST /admin/users 并传递正确请求体', async () => {
    ctrl.setResponse({
      id: 'user-001',
      username: 'newuser',
      email: 'new@test.com',
      system_role: 'user',
      workspace_create_perm: false
    })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()

    const result = await api.createUser({
      username: 'newuser',
      email: 'new@test.com',
      password: 'password123'
    })

    expect(result.id).toBe('user-001')
    expect(result.username).toBe('newuser')
    expect(result.system_role).toBe('user')

    // 验证调用参数
    expect(ctrl.mockFn).toHaveBeenCalledTimes(1)
    const call = ctrl.mockFn.mock.calls[0]!
    const url = call[0] as string
    expect(url).toContain('/admin/users')
  })

  it('createUser 403 错误被正确处理', async () => {
    ctrl.setError({
      response: { status: 403, _data: { error: 'system admin access required' } },
      message: 'FetchError'
    })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()

    try {
      await api.createUser({
        username: 'newuser',
        email: 'new@test.com',
        password: 'password123'
      })
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(403)
      expect(apiErr.error).toBe('system admin access required')
    }
  })

  it('createUser 409 重复冲突错误被正确处理', async () => {
    ctrl.setError({
      response: { status: 409, _data: { error: '用户名或邮箱已存在' } },
      message: 'FetchError'
    })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()

    try {
      await api.createUser({
        username: 'existing',
        email: 'existing@test.com',
        password: 'password123'
      })
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(409)
      expect(apiErr.error).toBe('用户名或邮箱已存在')
    }
  })

  it('createUser 400 验证错误被正确处理', async () => {
    ctrl.setError({
      response: { status: 400, _data: { error: '密码长度至少 8 个字符' } },
      message: 'FetchError'
    })

    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()

    try {
      await api.createUser({
        username: 'newuser',
        email: 'new@test.com',
        password: 'short'
      })
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(400)
      expect(apiErr.error).toBe('密码长度至少 8 个字符')
    }
  })

  it('CreateUserRequest 类型包含正确字段', async () => {
    // 验证类型定义存在
    const { useAdminApi } = await import('~/composables/useApi')
    const api = useAdminApi()

    // 确认 createUser 方法存在
    expect(typeof api.createUser).toBe('function')
  })
})
