// tests/conflict-dialog.test.ts — 409 冲突对话框三选项 emit 测试
//
// 引入动机：edit.vue 保存返回 409 时通过 ConflictDialog 提供三种处理路径
// （reload / force / cancel），对应不同的并发冲突解决策略。
// 此测试挂载真实 ConflictDialog 组件，验证三个按钮各自触发正确的 emit。
//
// 测试策略：mountSuspended 挂载真实组件，按 i18n 文案定位按钮并点击，
// 通过 wrapper.emitted() 断言对应事件被触发。

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

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

interface BtnFinder {
  findAll: (s: string) => Array<{ text: () => string; trigger: (e: string) => Promise<void> }>
}

/** ConflictDialog 的按钮渲染在 UModal teleport 的 content 中，从 document 全局查找。 */
function findDialogButton(path: string): HTMLButtonElement | undefined {
  const candidates = labelsOf(path)
  return Array.from(document.querySelectorAll<HTMLButtonElement>('[data-slot="content"] button'))
    .find(b => candidates.includes(b.textContent?.trim() ?? ''))
}

describe('ConflictDialog — 409 冲突三选项', () => {
  let mounted: { unmount: () => void; emitted: (e?: string) => Record<string, unknown[]> | unknown[] } | null = null

  beforeEach(() => {
    ctrl.reset()
  })

  afterEach(() => {
    mounted?.unmount()
    mounted = null
    vi.restoreAllMocks()
  })

  async function mountDialog() {
    const Dialog = await import('~/components/ConflictDialog.vue')
    const wrapper = await mountSuspended(Dialog.default, {
      attachTo: document.body,
      props: { open: true, message: '版本冲突：他人已修改' }
    })
    await flushPromises()
    mounted = wrapper
    return wrapper
  }

  it('点击「重新加载」触发 reload emit', async () => {
    const wrapper = await mountDialog()
    const btn = findDialogButton('errors.conflictReload')
    expect(btn, '应渲染「重新加载」按钮').toBeTruthy()
    btn!.click()
    await flushPromises()
    expect(wrapper.emitted('reload')).toBeTruthy()
    expect(wrapper.emitted('reload')).toHaveLength(1)
  })

  it('点击「强制覆盖」触发 force emit', async () => {
    const wrapper = await mountDialog()
    const btn = findDialogButton('errors.conflictForce')
    expect(btn, '应渲染「强制覆盖」按钮').toBeTruthy()
    btn!.click()
    await flushPromises()
    expect(wrapper.emitted('force')).toBeTruthy()
    expect(wrapper.emitted('force')).toHaveLength(1)
  })

  it('点击「取消」触发 cancel emit', async () => {
    const wrapper = await mountDialog()
    const btn = findDialogButton('errors.conflictCancel')
    expect(btn, '应渲染「取消」按钮').toBeTruthy()
    btn!.click()
    await flushPromises()
    expect(wrapper.emitted('cancel')).toBeTruthy()
    expect(wrapper.emitted('cancel')).toHaveLength(1)
  })

  it('三个按钮同时渲染，覆盖 reload/force/cancel 三种冲突路径', async () => {
    await mountDialog()
    expect(findDialogButton('errors.conflictReload')).toBeTruthy()
    expect(findDialogButton('errors.conflictForce')).toBeTruthy()
    expect(findDialogButton('errors.conflictCancel')).toBeTruthy()
  })
})
