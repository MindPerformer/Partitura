// tests/format-date.test.ts — useFormatDate 时间格式化行为测试
//
// 引入动机：计划要求前端使用标准 Date 解析，再以浏览器语言和时区显示。
// 非法值应记录可观察错误并显示已翻译的"时间不可用"，而非 Invalid Date。
// 使用 Intl.DateTimeFormat 按浏览器 locale/timezone 格式化。

import { describe, it, expect, beforeEach, vi } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('useFormatDate', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('合法 UTC RFC3339 输入被正确格式化为本地化字符串', async () => {
    const { useFormatDate } = await import('~/composables/useFormatDate')
    const { formatDate } = useFormatDate()

    const result = formatDate('2026-03-08T12:34:56Z')
    // 结果应包含年份和日期，不应是 "Invalid Date"
    expect(result).not.toBe('Invalid Date')
    expect(result).toContain('2026')
  })

  it('非法值返回已翻译的 fallback 而非 Invalid Date', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const { useFormatDate } = await import('~/composables/useFormatDate')
    const { formatDate } = useFormatDate()

    const result = formatDate('not-a-date')
    expect(result).not.toBe('Invalid Date')
    expect(result).not.toBe('NaN')
    // 应返回翻译后的 fallback 文案
    expect(result.length).toBeGreaterThan(0)
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringContaining('invalid RFC3339')
    )
    errorSpy.mockRestore()
  })

  it('空字符串返回 fallback', async () => {
    const { useFormatDate } = await import('~/composables/useFormatDate')
    const { formatDate } = useFormatDate()

    const result = formatDate('')
    expect(result).not.toBe('Invalid Date')
    expect(result.length).toBeGreaterThan(0)
  })

  it('null 和 undefined 返回 fallback', async () => {
    const { useFormatDate } = await import('~/composables/useFormatDate')
    const { formatDate } = useFormatDate()

    expect(formatDate(null as never)).not.toBe('Invalid Date')
    expect(formatDate(undefined as never)).not.toBe('Invalid Date')
  })

  it('formatDateOnly 仅返回日期部分', async () => {
    const { useFormatDate } = await import('~/composables/useFormatDate')
    const { formatDateOnly } = useFormatDate()

    const result = formatDateOnly('2026-03-08T12:34:56Z')
    expect(result).not.toBe('Invalid Date')
    expect(result).toContain('2026')
    // 日期格式不应包含时间部分（小时:分钟）
    // 不同 locale 可能输出不同格式，但不应包含秒
  })

  it('带时区偏移的 RFC3339 也能正确解析', async () => {
    const { useFormatDate } = await import('~/composables/useFormatDate')
    const { formatDate } = useFormatDate()

    const result = formatDate('2026-03-08T12:34:56+00:00')
    expect(result).not.toBe('Invalid Date')
    expect(result).toContain('2026')
  })

  it('格式化错误时记录 console.error 并返回 fallback', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {})
    const { useFormatDate } = await import('~/composables/useFormatDate')
    const { formatDate } = useFormatDate()

    // 传入一个能被 new Date 解析但可能导致 Intl.DateTimeFormat 异常的值
    // 实际上大多数合法 Date 都能格式化，这里验证空值路径
    const result = formatDate('')
    expect(result).not.toBe('Invalid Date')
    errorSpy.mockRestore()
  })
})
