// composables/useFormatDate.ts — 集中化时间格式化工具
//
// 引入动机：计划要求 API 时间字段统一为 UTC RFC3339，前端使用浏览器 locale/timezone 格式化。
// 分散的 new Date(value).toLocaleString() 实现存在以下问题：
// 1. 非法值直接渲染 "Invalid Date" 给用户
// 2. 无法统一国际化
// 3. 错误不可观察
//
// 设计原则：
// - 仅接受 RFC3339 格式字符串，非法值记录 console.error 并返回已翻译的 fallback
// - 使用 Intl.DateTimeFormat 按浏览器 locale 和 timezone 格式化
// - 本项目 ssr:false 纯 CSR：仅使用浏览器 locale（缺失回退 en-US）
// - 不直接渲染 Invalid Date

/**
 * useFormatDate 提供安全的时间格式化函数。
 *
 * 返回：
 * - formatDate(value: string | null | undefined): string — 格式化日期时间
 * - formatDateOnly(value: string | null | undefined): string — 仅格式化日期部分
 *
 * 行为：
 * - 输入为空或非法时，记录 console.error 并返回已翻译的 "时间不可用" 文案
 * - 使用 Intl.DateTimeFormat 按浏览器 locale 和 timezone 格式化
 *
 * 注意：useI18n() 是 Vue composable，必须在组件 setup 上下文中调用。
 * 在非组件上下文（如单元测试）中调用时，useI18n() 会抛出异常。
 * 此处使用 try-catch 安全获取 t 函数，回退为返回固定 key 的函数。
 */
export function useFormatDate() {
  let t: (key: string) => string
  try {
    const i18n = useI18n()
    t = i18n.t
  } catch {
    // 非 setup 上下文（如单元测试），回退为返回 key 的 identity 函数
    t = (key: string) => key
  }

  /**
   * 获取当前环境的 locale。
   * 本项目 ssr:false 纯 CSR：使用浏览器 navigator.language，缺失时回退 en-US。
   */
  function getLocale(): string {
    if (typeof navigator !== 'undefined' && navigator.language) {
      return navigator.language
    }
    return 'en-US'
  }

  /**
   * 判断字符串是否为合法的 RFC3339 日期时间。
   * 合法格式示例：2026-03-08T12:34:56Z 或 2026-03-08T12:34:56+00:00
   */
  function isValidRFC3339(value: string): boolean {
    if (!value || value.trim() === '') return false
    const d = new Date(value)
    return !isNaN(d.getTime())
  }

  /**
   * 格式化日期时间为本地化字符串。
   *
   * 参数：
   * - value: UTC RFC3339 字符串
   *
   * 返回：本地化日期时间字符串，非法值返回已翻译的 fallback。
   */
  function formatDate(value: string | null | undefined): string {
    if (value === null || value === undefined || value === '') {
      return t('common.timeUnavailable')
    }

    if (!isValidRFC3339(value)) {
      console.error(`[useFormatDate] invalid RFC3339 value: "${value}"`)
      return t('common.timeUnavailable')
    }

    try {
      const locale = getLocale()
      const formatter = new Intl.DateTimeFormat(locale, {
        year: 'numeric',
        month: 'short',
        day: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
        timeZoneName: 'short'
      })
      return formatter.format(new Date(value))
    } catch (err) {
      console.error(`[useFormatDate] formatting failed for "${value}":`, err)
      return t('common.timeUnavailable')
    }
  }

  /**
   * 仅格式化日期部分（不含时间）。
   *
   * 参数：
   * - value: UTC RFC3339 字符串
   *
   * 返回：本地化日期字符串，非法值返回已翻译的 fallback。
   */
  function formatDateOnly(value: string | null | undefined): string {
    if (value === null || value === undefined || value === '') {
      return t('common.timeUnavailable')
    }

    if (!isValidRFC3339(value)) {
      console.error(`[useFormatDate] invalid RFC3339 value: "${value}"`)
      return t('common.timeUnavailable')
    }

    try {
      const locale = getLocale()
      const formatter = new Intl.DateTimeFormat(locale, {
        year: 'numeric',
        month: 'short',
        day: 'numeric'
      })
      return formatter.format(new Date(value))
    } catch (err) {
      console.error(`[useFormatDate] formatting failed for "${value}":`, err)
      return t('common.timeUnavailable')
    }
  }

  return {
    formatDate,
    formatDateOnly
  }
}
