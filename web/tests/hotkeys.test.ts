// tests/hotkeys.test.ts — useHotkey 全局快捷键系统测试
//
// 引入动机：useHotkeys 承担全局快捷键分发（mod 跨平台、输入框抑制、
// 双键序列、注销清理）。此测试通过向 window 派发真实 KeyboardEvent
// 验证核心行为契约，而非源码断言：
// 1. mod+k / ctrl+k 组合触发且 preventDefault
// 2. input/textarea 聚焦时无修饰单键被抑制，但 ctrl/meta 组合仍生效
// 3. 双键序列（g h）在 500ms 内依次触发
// 4. unregister 后不再触发
// 5. 非法 combo 注册即抛错（Fail Fast）

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'

function fire(key: string, opts: Partial<KeyboardEventInit> = {}, target: EventTarget = window) {
  const e = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...opts })
  target.dispatchEvent(e)
  return e
}

describe('useHotkey — 全局快捷键', () => {
  let unregister: Array<() => void>

  beforeEach(async () => {
    unregister = []
    const mod = await import('~/composables/useHotkeys')
    mod.clearAllHotkeys()
  })

  afterEach(() => {
    unregister.forEach(u => u())
    unregister = []
  })

  it('ctrl+k 组合键触发 handler 并 preventDefault', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    unregister.push(useHotkey('ctrl+k', spy))

    const e = fire('k', { ctrlKey: true })
    expect(spy).toHaveBeenCalledTimes(1)
    expect(e.defaultPrevented).toBe(true)
  })

  it('普通按键不触发修饰键组合（误触保护）', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    unregister.push(useHotkey('ctrl+k', spy))

    fire('k') // 无 ctrl
    expect(spy).not.toHaveBeenCalled()
  })

  it('input 聚焦时无修饰单键被抑制，但 ctrl 组合仍生效', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const singleSpy = vi.fn()
    const comboSpy = vi.fn()
    unregister.push(useHotkey('/', singleSpy))
    unregister.push(useHotkey('ctrl+s', comboSpy))

    const input = document.createElement('input')
    document.body.appendChild(input)

    // 无修饰单键在 input 中应被抑制（防止打字误触）
    fire('/', {}, input)
    expect(singleSpy).not.toHaveBeenCalled()

    // ctrl 组合在 input 中仍生效（显式命令）
    fire('s', { ctrlKey: true }, input)
    expect(comboSpy).toHaveBeenCalledTimes(1)

    document.body.removeChild(input)
  })

  it('非输入区域单键正常触发', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    unregister.push(useHotkey('/', spy))
    fire('/', {}, document.body)
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('双键序列 g h 在 500ms 内依次触发', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    unregister.push(useHotkey('g h', spy))

    fire('g', {}, document.body)
    fire('h', {}, document.body)
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('双键序列首键后按错键不触发', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    unregister.push(useHotkey('g h', spy))

    fire('g', {}, document.body)
    fire('x', {}, document.body) // 错误次键
    expect(spy).not.toHaveBeenCalled()
  })

  it('unregister 后不再触发', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    const off = useHotkey('ctrl+k', spy)
    off()

    fire('k', { ctrlKey: true })
    expect(spy).not.toHaveBeenCalled()
  })

  it('非法 combo 注册即抛错（Fail Fast）', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    expect(() => useHotkey('', vi.fn())).toThrow()
    expect(() => useHotkey('a b c', vi.fn())).toThrow() // 超过 2 键序列
  })

  it('?（shift+/ 产生的字符键）能正确触发帮助快捷键', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    unregister.push(useHotkey('?', spy))
    // 浏览器中按 ? 实际产生 key='?' + shiftKey=true
    fire('?', { shiftKey: true }, document.body)
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('shift+x 显式修饰组合正常匹配', async () => {
    const { useHotkey } = await import('~/composables/useHotkeys')
    const spy = vi.fn()
    unregister.push(useHotkey('shift+x', spy))
    fire('X', { shiftKey: true }, document.body)
    expect(spy).toHaveBeenCalledTimes(1)
  })

  it('useHotkeyWithHelp 记录帮助条目供 ? 面板', async () => {
    const { useHotkeyWithHelp, getHotkeysHelp } = await import('~/composables/useHotkeys')
    unregister.push(useHotkeyWithHelp('mod+k', vi.fn(), '打开命令面板'))
    const help = getHotkeysHelp()
    expect(help.some(h => h.combo === 'mod+k' && h.description === '打开命令面板')).toBe(true)
  })
})
