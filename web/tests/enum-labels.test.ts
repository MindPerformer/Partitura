// tests/enum-labels.test.ts — useEnumLabels 枚举→i18n 标签契约测试
//
// 引入动机：admin/config.vue 用 settingTypeLabel 把后端 SystemSetting.type
// （string/int/bool/float/duration/json）映射到本地化文案。此契约此前未覆盖，
// 键错位会导致显示原始枚举值或 undefined。
//
// 测试策略：mountSuspended 挂载一个真实使用 useEnumLabels() 的探针组件，
// 在 setup 上下文拿到 t()，把各 setting 类型的标签渲染出来，断言为本地化文案
// （非枚举值本身、非 i18n key 原文）。

import { describe, it, expect } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineComponent, h } from 'vue'

const zhLocale = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'zh.json'), 'utf-8')) as Record<string, unknown>
const enLocale = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'en.json'), 'utf-8')) as Record<string, unknown>

function labelsOf(path: string): string[] {
  const read = (locale: Record<string, unknown>) =>
    path.split('.').reduce<unknown>((acc, k) => (acc as Record<string, unknown> | undefined)?.[k], locale)
  const zh = read(zhLocale)
  const en = read(enLocale)
  if (typeof zh !== 'string' || typeof en !== 'string') throw new Error(`locale 缺少 ${path}`)
  return [zh, en]
}

/** 探针组件：在真实 setup 上下文中调用 useEnumLabels，渲染各 setting 类型标签。 */
const Probe = defineComponent({
  name: 'EnumLabelProbe',
  setup() {
    const { settingTypeLabel } = useEnumLabels()
    const types = ['string', 'int', 'bool', 'float', 'duration', 'json']
    return () => h('div',
      types.map(ty => h('span', { 'data-type': ty }, settingTypeLabel(ty)))
    )
  }
})

describe('useEnumLabels — settingTypeLabel 契约', () => {
  it('int/bool/float/duration/string/json 各映射到正确的本地化标签', async () => {
    const wrapper = await mountSuspended(Probe)
    await wrapper.vm.$nextTick()

    // 期望映射：每个类型对应的 i18n key 的中英文案都可接受
    const cases: Array<[string, string]> = [
      ['string', 'admin.settingTypeString'],
      ['int', 'admin.settingTypeInteger'],
      ['bool', 'admin.settingTypeBoolean'],
      ['float', 'admin.settingTypeNumber'],
      ['duration', 'admin.settingTypeDuration'],
      ['json', 'admin.settingTypeJson']
    ]

    for (const [type, key] of cases) {
      const el = wrapper.find(`[data-type="${type}"]`)
      expect(el.exists(), `应渲染 ${type} 标签`).toBe(true)
      const text = el.text()
      const candidates = labelsOf(key)
      // 标签必须等于该 key 的中/英文案之一（即真实翻译，而非枚举值或 key 原文）
      expect(candidates.includes(text), `settingTypeLabel(${type}) 应为 ${key} 的译文，实际 "${text}"`).toBe(true)
      // 兜底：绝不把枚举值本身当作标签渲染
      expect(text).not.toBe(type)
    }
  })

  it('未知类型返回枚举值本身（不回退到空串或崩溃）', async () => {
    const UnknownProbe = defineComponent({
      setup() {
        const { settingTypeLabel } = useEnumLabels()
        return () => h('span', { 'data-type': 'unknown' }, settingTypeLabel('weird_type'))
      }
    })
    const wrapper = await mountSuspended(UnknownProbe)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-type="unknown"]').text()).toBe('weird_type')
  })

  it('空类型返回占位符 "-"', async () => {
    const EmptyProbe = defineComponent({
      setup() {
        const { settingTypeLabel } = useEnumLabels()
        return () => h('span', { 'data-type': 'empty' }, settingTypeLabel(''))
      }
    })
    const wrapper = await mountSuspended(EmptyProbe)
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-type="empty"]').text()).toBe('-')
  })
})
