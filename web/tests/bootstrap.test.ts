// tests/bootstrap.test.ts — Bootstrap 页面和 API 契约测试
//
// 引入动机：计划要求真实行为测试——Bootstrap 单次性、输入校验、i18n。
// 禁止源码字符串包含测试，所有测试调用真实 API mock 验证行为。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('Bootstrap API Contract', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('GET /api/bootstrap 返回 bootstrap_available 布尔值', async () => {
    ctrl.setResponse({ bootstrap_available: true })

    const { useBootstrapApi } = await import('~/composables/useApi')
    const api = useBootstrapApi()
    const result = await api.getStatus()

    expect(result.bootstrap_available).toBe(true)
    expect(ctrl.mockFn).toHaveBeenCalledTimes(1)
    // 验证请求路径
    const callUrl = ctrl.mockFn.mock.calls[0]![0] as string
    expect(callUrl).toContain('/bootstrap')
  })

  it('POST /api/bootstrap 创建首个管理员', async () => {
    ctrl.setResponse({ status: 'created', user_id: 'user-001' })

    const { useBootstrapApi } = await import('~/composables/useApi')
    const api = useBootstrapApi()
    const result = await api.createAdmin({
      username: 'admin',
      email: 'admin@example.com',
      password: 'secure-password-123'
    })

    expect(result.status).toBe('created')
    expect(result.user_id).toBe('user-001')

    // 验证请求方法和 body
    const callOpts = ctrl.mockFn.mock.calls[0]![1] as { method: string; body: string }
    expect(callOpts.method).toBe('POST')
    const body = JSON.parse(callOpts.body)
    expect(body.username).toBe('admin')
    expect(body.email).toBe('admin@example.com')
    expect(body.password).toBe('secure-password-123')
  })

  it('Bootstrap 不可用时返回 bootstrap_available=false', async () => {
    ctrl.setResponse({ bootstrap_available: false })

    const { useBootstrapApi } = await import('~/composables/useApi')
    const api = useBootstrapApi()
    const result = await api.getStatus()

    expect(result.bootstrap_available).toBe(false)
  })

  it('创建管理员失败时抛出 ApiError', async () => {
    ctrl.setError({
      response: { status: 403, _data: { error: 'Bootstrap 已关闭：系统已有用户' } },
      message: 'FetchError'
    })

    const { useBootstrapApi } = await import('~/composables/useApi')
    const api = useBootstrapApi()

    try {
      await api.createAdmin({
        username: 'admin',
        email: 'admin@example.com',
        password: 'secure-password-123'
      })
      expect.fail('Should have thrown')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(403)
      expect(apiErr.error).toContain('Bootstrap')
    }
  })
})

// 引入动机：useI18n() 是 Vue composable，必须在 setup 上下文顶层调用。
// 在测试函数体中直接调用会抛出 "Must be called at the top of a setup function"。
// 改为直接读取 locale JSON 文件验证翻译键存在性和非空值，
// 与 i18n.test.ts 采用相同的文件直读方式，不依赖 Vue 组件上下文。
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const zhLocale = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'zh.json'), 'utf-8'))
const enLocale = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'en.json'), 'utf-8'))

describe('Bootstrap i18n', () => {
  it('中文翻译包含 bootstrap 键', () => {
    // 验证 bootstrap 相关翻译键存在且返回非空字符串
    expect(zhLocale.bootstrap).toBeDefined()
    expect(zhLocale.bootstrap.title).toBeTruthy()
    expect(zhLocale.bootstrap.subtitle).toBeTruthy()
    expect(zhLocale.bootstrap.createAdmin).toBeTruthy()
    expect(zhLocale.bootstrap.usernameRequired).toBeTruthy()
    expect(zhLocale.bootstrap.passwordLength).toBeTruthy()
  })

  it('英文翻译包含 bootstrap 键', () => {
    expect(enLocale.bootstrap).toBeDefined()
    expect(enLocale.bootstrap.title).toBeTruthy()
    expect(enLocale.bootstrap.subtitle).toBeTruthy()
    expect(enLocale.bootstrap.createAdmin).toBeTruthy()
  })
})

describe('Bootstrap API Type Contract', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('BootstrapStatusResponse 必须包含 bootstrap_available 布尔值字段', async () => {
    ctrl.setResponse({ bootstrap_available: true })

    const { useBootstrapApi } = await import('~/composables/useApi')
    const api = useBootstrapApi()
    const result = await api.getStatus()

    // 严格契约验证：返回值必须有 bootstrap_available 布尔值
    expect(typeof result.bootstrap_available).toBe('boolean')
  })

  it('BootstrapResponse 必须包含 status 和 user_id 字段', async () => {
    ctrl.setResponse({ status: 'created', user_id: 'user-001' })

    const { useBootstrapApi } = await import('~/composables/useApi')
    const api = useBootstrapApi()
    const result = await api.createAdmin({
      username: 'admin',
      email: 'admin@example.com',
      password: 'secure-password-123'
    })

    expect(typeof result.status).toBe('string')
    expect(typeof result.user_id).toBe('string')
  })
})
