// tests/useIndexJobs.test.ts — Index Jobs 响应契约与错误状态测试
//
// 引入动机：修复管理端索引任务页（pages/admin/jobs.vue）白屏。
// 根因是 /api/admin/jobs 曾返回 Go 字段名，页面拿到 job.id === undefined
// 后在渲染期抛异常，整个列表渲染中断。
//
// 本测试覆盖合法响应、jobs 不是数组、Go 字段名形态响应、total 非数值、请求抛错，
// 确保 API boundary 验证不通过时显示本地化错误、jobs 为 []、不抛异常。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('useIndexJobs', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('合法响应被正确解析并填充数据', async () => {
    ctrl.setResponse({
      jobs: [
        {
          id: '0123456789abcdef',
          type: 'index_document',
          status: 'pending',
          payload: { document_id: 'doc-1' },
          attempts: 2,
          max_attempts: 3,
          error: '',
          created_at: '2026-03-08T10:00:00Z',
          updated_at: '2026-03-08T10:05:00Z'
        }
      ],
      total: 1,
      limit: 20,
      offset: 0
    })

    const { useIndexJobs } = await import('~/composables/useIndexJobs')
    const { jobs, total, error, load } = useIndexJobs()

    await load({ limit: 20, offset: 0 })

    expect(error.value).toBeNull()
    expect(total.value).toBe(1)
    expect(jobs.value.length).toBe(1)

    const job = jobs.value[0]
    if (!job) throw new Error('expected job at index 0')
    expect(job.id).toBe('0123456789abcdef')
    expect(job.attempts).toBe(2)
    expect(job.max_attempts).toBe(3)
    expect(job.updated_at).toBe('2026-03-08T10:05:00Z')
  })

  it('空数组响应正常处理，jobs 为 []，无错误', async () => {
    ctrl.setResponse({ jobs: [], total: 0, limit: 20, offset: 0 })

    const { useIndexJobs } = await import('~/composables/useIndexJobs')
    const { jobs, total, error, load } = useIndexJobs()

    await load()

    expect(error.value).toBeNull()
    expect(jobs.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('jobs 不是数组被视为非法契约，报错且不渲染', async () => {
    ctrl.setResponse({ jobs: null, total: 0, limit: 20, offset: 0 })

    const { useIndexJobs } = await import('~/composables/useIndexJobs')
    const { jobs, total, error, load } = useIndexJobs()

    await load()

    expect(error.value).toContain('服务器返回的任务列表格式不正确')
    expect(jobs.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('Go 字段名形态的响应（jobs 缺失）被识别为非法契约', async () => {
    // 这正是线上白屏时后端返回的形态：字段是 Go 名而非 snake_case。
    ctrl.setResponse({
      Jobs: [{ ID: '0123456789abcdef', AttemptCount: 2, LastError: '', CreatedAt: '2026-03-08T10:00:00Z' }],
      Total: 1
    })

    const { useIndexJobs } = await import('~/composables/useIndexJobs')
    const { jobs, total, error, load } = useIndexJobs()

    await load()

    expect(error.value).toContain('服务器返回的任务列表格式不正确')
    expect(jobs.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('total 不是 number 被视为非法响应', async () => {
    ctrl.setResponse({ jobs: [], total: 'zero' })

    const { useIndexJobs } = await import('~/composables/useIndexJobs')
    const { jobs, total, error, load } = useIndexJobs()

    await load()

    expect(error.value).not.toBeNull()
    expect(jobs.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('API 异常被正确捕获为错误消息', async () => {
    ctrl.setError({
      response: { status: 500, _data: { error: 'internal error' } },
      message: 'FetchError'
    })

    const { useIndexJobs } = await import('~/composables/useIndexJobs')
    const { error, jobs, load } = useIndexJobs()

    await load()

    expect(error.value).toBe('internal error')
    expect(jobs.value).toEqual([])
  })
})
