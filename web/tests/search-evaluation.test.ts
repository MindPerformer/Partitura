import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('useSearchEvaluation', () => {
  beforeEach(() => ctrl.reset())

  it('null 响应不会把 null 传给选择项', async () => {
    ctrl.setResponse(null)
    const { useSearchEvaluation } = await import('~/composables/useSearchEvaluation')
    const state = useSearchEvaluation()

    await state.load()

    expect(state.datasets.value).toEqual([])
    expect(state.profiles.value).toEqual([])
    expect(state.error.value).toBe('服务器返回的评测列表格式不正确')
    expect(state.loading.value).toBe(false)
  })

  it('空数据集和空 profile 列表可正常加载', async () => {
    ctrl.setImpl(async (url) => url.includes('/evaluation/datasets')
      ? { _data: { datasets: [], total: 0, limit: 20, offset: 0 }, status: 200 }
      : { _data: { profiles: [], total: 0, limit: 100, offset: 0 }, status: 200 })
    const { useSearchEvaluation } = await import('~/composables/useSearchEvaluation')
    const state = useSearchEvaluation()

    await state.load()

    expect(state.datasets.value).toEqual([])
    expect(state.profiles.value).toEqual([])
    expect(state.error.value).toBeNull()
  })
})
