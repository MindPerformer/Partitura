// composables/useHotkeys.ts — 轻量全局键盘快捷键注册系统
//
// 引入动机：UI/UX 优化阶段需要全局快捷键（Ctrl+K 命令面板、/ 聚焦搜索、
// ? 帮助、g h 双键序列等），且必须在不侵入各页面的前提下统一管理。
//
// 设计决策：
// - 单一 window keydown 监听器 + 注册表（Map）：避免每个 useHotkey 调用都
//   挂一个监听器，便于统一处理 input 聚焦抑制与双键序列状态。
// - 修饰键跨平台：combo 中的 "mod" 在 Mac 上映射 meta(⌘)，其它平台映射 ctrl；
//   "ctrl"/"meta" 字面修饰键按字面匹配。对外文档统一推荐用 "mod"。
// - 输入聚焦抑制（关键安全约束）：当焦点在 input/textarea/select/
//   contenteditable 上时，忽略一切"无修饰单键/序列"（如 g、h、/、?），
//   防止打字误触快捷键；但带 mod/ctrl/alt 的组合键（Ctrl+S、Ctrl+K）在
//   输入框中仍然生效，因为这些是显式的用户命令而非可打印字符。
// - 双键序列：combo "g h" 表示先按 g 再在 SEQUENCE_TIMEOUT 内按 h；
//   首键命中后进入 pending 状态，超时或按错键则清空并重置。
// - 生命周期：监听器挂在 window 上并在注册表为空时移除；调用方通过
//   onScopeDispose 自动 unregister，组件卸载即清理，无泄漏。
// - Fail Fast：非法 combo 在注册时抛错（不静默忽略）；handler 抛出的异常
//   向上传播给 window.onerror / 控制台，不吞掉。
//
// 平台说明：本项目 ssr:false 纯 CSR，但仍在模块加载期对 window 做了
// typeof 守卫，保证 vitest / SSR 工具链下 import 不崩溃。

import { getCurrentScope, onScopeDispose } from 'vue'

/** 双键序列允许的最大间隔（毫秒）。500ms 是常见 GitHub/Vim 风格序列阈值。 */
const SEQUENCE_TIMEOUT = 500

/** 已注册的单个快捷键动作 */
export interface HotkeyAction {
  /** 触发回调；返回 false 可阻止默认后续处理（一般不需要） */
  handler: (e: KeyboardEvent) => void
  /** 可选的可读描述，供 `?` 帮助面板展示（通常是已翻译文案） */
  description?: string
  /**
   * 是否在 input/textarea 聚焦时也触发。默认遵循"无修饰键则抑制"规则；
   * 显式设为 true 可强制单键在输入框中生效（极少用，谨慎）。
   */
  allowInInput?: boolean
}

interface ParsedCombo {
  /** 修饰键集合（已规范化：'mod' 已按平台展开为 ctrl/meta） */
  mods: Set<'ctrl' | 'meta' | 'shift' | 'alt'>
  /** 序列中的按键（1 个为单键/组合，2 个为双键序列），已规范化 */
  keys: string[]
}

interface RegistryEntry extends ParsedCombo {
  handler: (e: KeyboardEvent) => void
  description?: string
  allowInInput: boolean
}

// 模块级注册表：combo 字符串 -> 条目。key 为规范化后的 combo（空格分隔）。
const registry = new Map<string, RegistryEntry>()

// 模块级监听器是否已挂载。
let listening = false

// 双键序列的 pending 状态：记录首键 combo 与时间戳。
let pendingSequence: { firstKey: string; mods: string; time: number } | null = null
let sequenceTimer: ReturnType<typeof setTimeout> | null = null

/** 是否为 Mac 平台（决定 mod → meta 还是 ctrl）。 */
function isMac(): boolean {
  return typeof navigator !== 'undefined' && /mac/i.test(navigator.platform)
}

/** 规范化按键名：统一小写，个别别名映射到 KeyboardEvent.key 的值。 */
function normalizeKey(raw: string): string {
  const k = raw.toLowerCase()
  // 常见别名统一
  if (k === 'esc' || k === 'escape') return 'escape'
  if (k === 'space' || k === ' ') return ' '
  if (k === 'return' || k === 'enter') return 'enter'
  if (k === 'del' || k === 'delete') return 'delete'
  if (k === 'arrowup' || k === 'up') return 'arrowup'
  if (k === 'arrowdown' || k === 'down') return 'arrowdown'
  if (k === 'arrowleft' || k === 'left') return 'arrowleft'
  if (k === 'arrowright' || k === 'right') return 'arrowright'
  return k
}

/**
 * 解析 combo 字符串为结构化形式。
 *
 * 语法：
 * - "mod+k" / "ctrl+s"：修饰键 + 单键（用 + 连接）
 * - "shift+?"：shift + 字符
 * - "/" / "?" / "g" / "escape"：单键
 * - "g h"：双键序列（空格分隔，先 g 后 h）
 * - 修饰键集合：mod(跨平台)/ctrl/meta/shift/alt
 *
 * @throws 非法 combo（空串、序列超 2 键、序列中带修饰键）时抛 Error（Fail Fast）
 */
function parseCombo(combo: string): ParsedCombo {
  const trimmed = combo.trim()
  if (!trimmed) {
    throw new Error('[useHotkeys] 快捷键 combo 不能为空')
  }
  // 双键序列以空格分隔（"g h"）。
  const sequenceParts = trimmed.split(/\s+/)
  if (sequenceParts.length > 2) {
    throw new Error(`[useHotkeys] 不支持超过 2 键的序列："${combo}"`)
  }

  const mods = new Set<'ctrl' | 'meta' | 'shift' | 'alt'>()
  const keys: string[] = []

  for (const part of sequenceParts) {
    // 每段内部用 + 分隔修饰键与主键
    const tokens = part.split('+').map(s => s.trim().toLowerCase()).filter(Boolean)
    if (tokens.length === 0) {
      throw new Error(`[useHotkeys] 无效的快捷键段："${part}"`)
    }
    // 双键序列的每一键不允许再带修饰键（避免歧义）。
    if (sequenceParts.length === 2 && tokens.length > 1) {
      throw new Error(`[useHotkeys] 序列键 "${part}" 不能带修饰键`)
    }
    // 最后一个 token 是主键，其余为修饰键
    const mainKey = tokens[tokens.length - 1]!
    for (let i = 0; i < tokens.length - 1; i++) {
      const m = tokens[i]!
      if (m === 'mod') {
        mods.add(isMac() ? 'meta' : 'ctrl')
      } else if (m === 'ctrl' || m === 'control') {
        mods.add('ctrl')
      } else if (m === 'meta' || m === 'cmd' || m === 'command') {
        mods.add('meta')
      } else if (m === 'shift') {
        mods.add('shift')
      } else if (m === 'alt' || m === 'option') {
        mods.add('alt')
      } else {
        throw new Error(`[useHotkeys] 未知修饰键 "${m}"（combo="${combo}"）`)
      }
    }
    keys.push(normalizeKey(mainKey))
  }

  return { mods, keys }
}

/** 把解析后的 combo 重新序列化为规范化 key（用作注册表 Map 的键）。 */
function comboKey(p: ParsedCombo): string {
  const modStr = ['ctrl', 'meta', 'shift', 'alt'].filter(m => p.mods.has(m as never)).join('+')
  return `${modStr}::${p.keys.join(' ')}`
}

/** 判断事件目标是否为可输入元素（需要抑制单键快捷键）。 */
function isEditableTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false
  if (target.isContentEditable) return true
  const tag = target.tagName
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT'
}

/** 事件修饰键状态与条目修饰键是否完全一致（不多不少）。 */
function modsMatch(e: KeyboardEvent, mods: Set<string>): boolean {
  return e.ctrlKey === mods.has('ctrl')
    && e.metaKey === mods.has('meta')
    && e.shiftKey === mods.has('shift')
    && e.altKey === mods.has('alt')
}

/** 清空双键序列 pending 状态与计时器。 */
function clearSequence(): void {
  pendingSequence = null
  if (sequenceTimer !== null) {
    clearTimeout(sequenceTimer)
    sequenceTimer = null
  }
}

/** 核心 keydown 处理器：先匹配双键序列的次键，再匹配完整 combo。 */
function onKeydown(e: KeyboardEvent): void {
  if (registry.size === 0) return

  const key = normalizeKey(e.key)
  const editable = isEditableTarget(e.target)

  // --- 双键序列：检查是否存在 pending 首键，且当前键与之组成已注册序列 ---
  if (pendingSequence && Date.now() - pendingSequence.time <= SEQUENCE_TIMEOUT) {
    // 序列键不带修饰键；若当前按了修饰键则丢弃序列。
    if (!e.ctrlKey && !e.metaKey && !e.altKey) {
      const seqCombo = `${pendingSequence.mods}::${pendingSequence.firstKey} ${key}`
      const entry = registry.get(seqCombo)
      clearSequence()
      if (entry) {
        if (!editable || entry.allowInInput) {
          e.preventDefault()
          entry.handler(e)
        }
        return
      }
      // 未命中序列则继续按普通单键处理当前键（不回退首键，避免误触发）。
    } else {
      clearSequence()
    }
  } else if (pendingSequence) {
    clearSequence()
  }

  // --- 单键 / 修饰键组合匹配 ---
  const hasCommandMod = e.ctrlKey || e.metaKey || e.altKey
  // 输入框抑制规则：带 ctrl/meta/alt 的显式命令组合在输入框中仍生效；
  // 无命令修饰的可打印键（字母、/、?、shift+x 大写等）在输入框中被抑制，
  // 防止打字误触快捷键。shift 单独不算命令修饰键（shift+字母 = 大写输入）。
  const suppressedInInput = editable && !hasCommandMod

  // 候选 lookup key：先按"含 shift"匹配（命中显式注册的 shift+x 组合），
  // 未命中再"去 shift"匹配——处理需要 shift 才能输入的字符键（如 '?'/'!'/':'，
  // 注册为裸键 "?" 但 e.shiftKey=true）。
  const modWithShift = [
    e.ctrlKey ? 'ctrl' : '',
    e.metaKey ? 'meta' : '',
    e.shiftKey ? 'shift' : '',
    e.altKey ? 'alt' : ''
  ].filter(Boolean).join('+')
  const modNoShift = [
    e.ctrlKey ? 'ctrl' : '',
    e.metaKey ? 'meta' : '',
    e.altKey ? 'alt' : ''
  ].filter(Boolean).join('+')

  const candidateKeys = e.shiftKey
    ? [`${modWithShift}::${key}`, `${modNoShift}::${key}`]
    : [`${modWithShift}::${key}`]

  for (const directKey of candidateKeys) {
    const entry = registry.get(directKey)
    if (!entry) continue
    if (suppressedInInput && !entry.allowInInput) {
      return
    }
    e.preventDefault()
    entry.handler(e)
    return
  }

  // --- 双键序列首键：当前键是某个序列的第一段则进入 pending ---
  if (!editable && !e.ctrlKey && !e.metaKey && !e.altKey) {
    const prefix = `::${key} `
    for (const k of registry.keys()) {
      // 序列条目 mods 为空时 comboKey 形如 "::g h"
      if (k.startsWith(prefix) && k.split('::')[0] === '') {
        pendingSequence = { firstKey: key, mods: '', time: Date.now() }
        sequenceTimer = setTimeout(clearSequence, SEQUENCE_TIMEOUT)
        break
      }
    }
  }
}

/** 确保全局监听器已挂载。 */
function ensureListening(): void {
  if (listening || typeof window === 'undefined') return
  window.addEventListener('keydown', onKeydown)
  listening = true
}

/** 当注册表为空时移除全局监听器（避免空转）。 */
function maybeStopListening(): void {
  if (!listening || registry.size > 0 || typeof window === 'undefined') return
  window.removeEventListener('keydown', onKeydown)
  listening = false
  clearSequence()
}

/**
 * useHotkey — 注册单个快捷键。
 *
 * 在组件 setup 中调用即注册，组件卸载（onScopeDispose）自动注销。
 * 在模块/全局作用域调用则需手动 unregister（返回的函数）。
 *
 * @param combo 快捷键组合（"mod+k"、"ctrl+s"、"/"、"?"、"g h"、"escape"）
 * @param handler 触发回调
 * @param opts 可选：description（帮助文案）、allowInInput（输入框中强制生效）
 * @returns unregister 注销函数
 */
export function useHotkey(
  combo: string,
  handler: (e: KeyboardEvent) => void,
  opts: { description?: string; allowInInput?: boolean } = {}
): () => void {
  const parsed = parseCombo(combo)
  const key = comboKey(parsed)
  registry.set(key, {
    ...parsed,
    handler,
    description: opts.description,
    allowInInput: opts.allowInInput ?? false
  })
  ensureListening()

  const unregister = () => {
    registry.delete(key)
    maybeStopListening()
  }

  // 在组件/effect 作用域内自动注销；不在作用域内（如测试直接调用）时
  // onScopeDispose 会 warn，这里用 getCurrentScope 守卫。
  if (getCurrentScope()) {
    onScopeDispose(unregister)
  }

  return unregister
}

/** 单个已注册快捷键的展示信息（供 `?` 帮助面板）。 */
export interface HotkeyHelpItem {
  combo: string
  description: string
}

// 帮助面板的展示清单：combo 原始串 + 描述（在注册时写入）。
const helpRegistry = new Map<string, string>()

/**
 * 注册快捷键并加入帮助清单。等价于 useHotkey + 记录展示条目。
 * 推荐全局快捷键使用此函数，使 `?` 帮助面板自动收录。
 */
export function useHotkeyWithHelp(
  combo: string,
  handler: (e: KeyboardEvent) => void,
  description: string,
  opts: { allowInInput?: boolean } = {}
): () => void {
  helpRegistry.set(combo, description)
  const unregister = useHotkey(combo, handler, { description, ...opts })
  const combined = () => {
    helpRegistry.delete(combo)
    unregister()
  }
  if (getCurrentScope()) {
    onScopeDispose(() => helpRegistry.delete(combo))
  }
  return combined
}

/**
 * 获取当前已注册且登记到帮助清单的快捷键列表（响应式快照）。
 * 供 `?` 帮助面板渲染；每次调用返回最新数组。
 */
export function getHotkeysHelp(): HotkeyHelpItem[] {
  // 同一动作可能注册多个等价 combo（如 mod+k 与 ctrl+k 都打开命令面板），
  // 按描述去重：保留首个出现的 combo 作为代表键位，避免帮助面板出现重复行。
  const seen = new Map<string, HotkeyHelpItem>()
  for (const [combo, description] of helpRegistry.entries()) {
    if (!seen.has(description)) {
      seen.set(description, { combo, description })
    }
  }
  return Array.from(seen.values())
}

/** 清空所有快捷键（测试/重置用）。 */
export function clearAllHotkeys(): void {
  registry.clear()
  helpRegistry.clear()
  maybeStopListening()
}
