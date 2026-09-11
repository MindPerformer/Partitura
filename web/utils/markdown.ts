// utils/markdown.ts — 安全 Markdown 渲染
//
// 引入动机：design/04-WEB-API.md §Security 要求 safe Markdown rendering、XSS prevention。
// 使用 marked 解析 Markdown 为 HTML，DOMPurify 净化 HTML。
// 默认不信任 HTML，链接协议需安全。
//
// 安全策略：
// - marked 解析 Markdown 为 HTML（marked 默认不解析内联 HTML 标签，但用户可能通过各种
//   Markdown 语法构造出包含原始 HTML 的输出，因此后续净化不可省略）
// - DOMPurify 净化 HTML，移除 script/iframe/object/embed 等危险标签
// - 链接协议仅允许 http/https/mailto，禁止 javascript:、data:、vbscript: 等
// - 不允许内联 style 属性
// - 不允许任意 HTML 标签（DOMPurify 白名单兜底）
// - data: URI 在所有标签上均被禁止（DOMPurify 默认允许 img/audio/video 的 data: URI，
//   但 data:text/html 可执行 XSS，因此通过 uponSanitizeAttribute hook 额外拦截）
//
// SSR 安全策略：
// - 在 SSR 环境下使用 jsdom 创建 DOM 窗口，让 DOMPurify 真正运行（而非正则兜底）
// - jsdom 窗口全局缓存，避免每次渲染创建新实例
// - 客户端直接使用浏览器原生 window
// - 不存在空 catch 静默降级：如果 DOMPurify 初始化失败，抛出错误而非返回未净化 HTML

import { Marked, marked } from 'marked'
import DOMPurify from 'dompurify'

// 配置全局 marked，保持现有 renderMarkdown 调用方的解析行为。
marked.setOptions({
  gfm: true,
  breaks: false
})

/** MarkdownRenderer 与目录共用的标题信息。 */
export interface MarkdownHeading {
  id: string
  level: number
  text: string
}

/**
 * 将标题文本转换为稳定的 HTML 锚点 id。
 *
 * 保留中文、日文、韩文等 Unicode 字母和数字，避免中文标题生成空 id。
 */
export function slugify(text: string): string {
  const slug = text
    .normalize('NFKC')
    .toLocaleLowerCase()
    .trim()
    .replace(/[^\p{L}\p{N}\s-]/gu, '')
    .replace(/[\s-]+/g, '-')
    .replace(/^-+|-+$/g, '')

  return slug || 'section'
}

/**
 * 判断当前是否在真实浏览器环境中。
 *
 * 引入动机：DOMPurify 需要一个完整、正确的 DOM 实现才能安全净化 HTML。
 * - 真实浏览器：原生 window 提供完整 DOM，DOMPurify 可直接使用
 * - SSR / 测试环境：无 window 或 window 来自 happy-dom 等不完整 DOM 实现，
 *   DOMPurify 无法正确工作，必须使用 jsdom 提供的完整 DOM
 *
 * 检测方式：process.versions.node 是 Node.js 运行时特有的属性，
 * 浏览器环境（包括各种 polyfill）不存在此属性。
 */
const isBrowser = typeof process === 'undefined' || !process.versions?.node

/**
 * 非浏览器环境下使用 jsdom 创建的 DOM 窗口。
 *
 * 引入动机：DOMPurify 依赖 DOM API（window.document、window.Element 等），
 * SSR 环境无 window 对象，测试环境的 happy-dom 不够完整。
 * 使用 jsdom 创建一个符合 DOM 标准的窗口，
 * 传入 DOMPurify 工厂函数 createDOMPurify(window)，使其真正运行。
 *
 * 缓存策略：jsdom 窗口创建开销较大，全局缓存单例，整个进程生命周期复用。
 */
let ssrWindow: Window | null = null

/**
 * 缓存的 DOMPurify 实例。
 *
 * 引入动机：DOMPurify 实例创建和 hook 注册有开销，缓存单例避免每次渲染重复创建。
 * hook 只注册一次，后续 sanitize 调用复用已注册的 hook。
 */
let cachedPurifier: typeof DOMPurify | null = null

/**
 * 惰性加载的 jsdom 模块状态。
 *
 * 引入动机：jsdom 是 Node.js 专用包，不能在浏览器打包中包含。
 * 使用动态 require 加载，仅在非浏览器环境执行。
 */
let jsdomLoaded: boolean = false
let JSDOMClass: typeof import('jsdom').JSDOM | null = null

/**
 * 惰性加载 jsdom 模块。
 *
 * 引入动机：避免在顶层 import 'jsdom' 导致 Vite 客户端构建失败。
 * 仅在非浏览器环境（SSR / 测试）首次调用时加载。
 *
 * 实现说明：
 * - 使用 eval('require') 获取 Node.js require 函数，避免 Vite 尝试解析和打包
 * - 通过 createRequire(import.meta.url) 在 ESM 环境中创建 require 函数
 * - import.meta.server 守卫确保此代码仅在服务端执行，
 *   Vite 客户端构建中 import.meta.server 为 false，整个分支被 tree-shaking 移除
 *
 * @throws 如果 jsdom 加载失败，抛出错误（不静默降级）
 */
function loadJsdom(): void {
  if (jsdomLoaded) {
    return
  }

  // eval('require') 阻止 Vite 静态分析和打包此调用
  // isBrowser 为运行时检测：浏览器中 process.versions.node 不存在，此分支不执行
  // SSR 和测试环境（Node.js）中 isBrowser 为 false，jsdom 被加载
  if (!isBrowser) {
    const nodeRequire = eval('require')
    const { createRequire } = nodeRequire('node:module')
    const req = createRequire(import.meta.url)
    const jsdom = req('jsdom') as typeof import('jsdom')
    JSDOMClass = jsdom.JSDOM
  }

  jsdomLoaded = true
}

/**
 * 获取 DOMPurify 可用的 window 对象。
 *
 * - 真实浏览器：直接返回浏览器 window
 * - SSR / 测试环境：返回 jsdom 创建的窗口（惰性初始化、全局缓存）
 *
 * 引入动机：DOMPurify 默认导出在无 window 环境下退化为无操作；
 * 在 happy-dom 等不完整 DOM 实现下净化不正确。必须传入一个完整 DOM 窗口。
 *
 * 同步实现说明：renderMarkdown 被组件 computed 同步调用，不能改为异步。
 * jsdom 通过 createRequire 同步加载，避免 async/await 传播。
 *
 * @returns DOM 窗口对象
 * @throws 如果 jsdom 初始化失败，抛出错误（不静默降级）
 */
function getDOMWindow(): Window {
  if (isBrowser && typeof window !== 'undefined') {
    return window
  }

  // SSR / 测试环境：惰性初始化 jsdom 窗口
  if (ssrWindow) {
    return ssrWindow
  }

  loadJsdom()
  if (!JSDOMClass) {
    throw new Error('jsdom initialization failed: JSDOM class not available')
  }

  const dom = new JSDOMClass('<!DOCTYPE html><html><body></body></html>', {
    url: 'http://localhost/'
  })
  // jsdom 的 DOMWindow 类型与浏览器 Window 类型不完全匹配，使用类型断言
  const win = dom.window as unknown as Window
  ssrWindow = win

  return win
}

/**
 * 获取已绑定 window 并注册安全 hook 的 DOMPurify 实例。
 *
 * 引入动机：
 * 1. 默认导出的 DOMPurify 在 SSR 下因 getGlobal() 返回 null 而退化为无操作。
 *    使用 DOMPurify(window) 工厂函数创建绑定到真实 DOM 窗口的实例。
 * 2. DOMPurify 默认允许 img/audio/video 等标签的 data: URI（DATA_URI_TAGS），
 *    但 data:text/html 可执行 XSS。通过 uponSanitizeAttribute hook 额外拦截
 *    所有标签上 src/href 属性中的 data: URI，确保仅允许 http/https/mailto。
 *
 * @returns 绑定到当前环境 window 的 DOMPurify 实例
 * @throws 如果 DOMPurify 创建失败，抛出错误（不静默降级）
 */
function getDOMPurifyInstance(): typeof DOMPurify {
  if (cachedPurifier) {
    return cachedPurifier
  }

  const win = getDOMWindow()
  // DOMPurify 的 WindowLike 类型是 Window 的子集，使用类型断言确保兼容
  const instance = DOMPurify(win as unknown as Parameters<typeof DOMPurify>[0])
  if (!instance || typeof instance.sanitize !== 'function') {
    throw new Error('DOMPurify initialization failed: sanitize function not available')
  }

  // 注册 uponSanitizeAttribute hook：拦截 data: URI
  // DOMPurify 默认允许 img/audio/video 等标签的 data: URI（DATA_URI_TAGS），
  // 但 data:text/html 可以执行脚本（XSS），因此必须在所有标签上禁止 data: 协议。
  // ALLOWED_URI_REGEXP 已禁止 data:，但 DATA_URI_TAGS 逻辑会绕过此检查。
  // 此 hook 在 DOMPurify 内部属性检查之后执行，作为最终安全屏障。
  instance.addHook('uponSanitizeAttribute', (_node, data) => {
    const attrName = data.attrName
    const attrValue = data.attrValue

    // 检查 src 和 href 属性中的 data: URI
    if ((attrName === 'src' || attrName === 'href') &&
        attrValue && attrValue.trim().toLowerCase().startsWith('data:')) {
      // 移除属性值，阻止 data: URI 通过
      data.attrValue = ''
    }
  })

  cachedPurifier = instance
  return instance
}

/** Markdown 内容净化配置。标题 id 在白名单中，确保目录锚点不会被移除。 */
const MARKDOWN_SANITIZE_CONFIG = {
  ALLOWED_TAGS: [
    'h1', 'h2', 'h3', 'h4', 'h5', 'h6',
    'p', 'br', 'hr',
    'ul', 'ol', 'li',
    'blockquote', 'code', 'pre',
    'a', 'strong', 'em', 'del', 's',
    'table', 'thead', 'tbody', 'tr', 'th', 'td',
    'img', 'span', 'div',
    'input'
  ],
  ALLOWED_ATTR: ['href', 'src', 'alt', 'title', 'class', 'id', 'type', 'checked', 'disabled'],
  ALLOW_DATA_ATTR: false,
  ALLOWED_URI_REGEXP: /^(?:(?:https?|mailto):)/i
}

function sanitizeMarkdownHtml(rawHtml: string): string {
  const purifier = getDOMPurifyInstance()
  return purifier.sanitize(rawHtml, MARKDOWN_SANITIZE_CONFIG) as string
}

function extractHeadingText(inlineHtml: string): string {
  const document = getDOMWindow().document
  const container = document.createElement('div')
  container.innerHTML = inlineHtml
  return container.textContent?.trim() || ''
}

/**
 * 解析并安全渲染 Markdown，同时返回与真实 HTML heading 一一对应的目录数据。
 *
 * 引入动机：目录必须跳转到渲染后的标题，不能依赖可能与 marked 解析结果不一致的
 * 服务端 outline。使用同一个 Marked 实例生成 heading id 和 headings 列表，保证两者一致。
 *
 * @param markdown — Markdown 原始文本
 * @returns 净化后的 HTML 与标题目录
 * @throws 如果 Markdown 解析或 DOMPurify 净化失败，直接抛出错误
 */
export function renderMarkdownWithOutline(markdown: string): { html: string; headings: MarkdownHeading[] } {
  if (!markdown) {
    return { html: '', headings: [] }
  }

  const headings: MarkdownHeading[] = []
  const usedIds = new Map<string, number>()
  const parser = new Marked({
    gfm: true,
    breaks: false,
    renderer: {
      heading({ tokens, depth }) {
        const inlineHtml = this.parser.parseInline(tokens) as string
        const text = extractHeadingText(inlineHtml)
        const baseId = `heading-${slugify(text)}`
        const occurrence = usedIds.get(baseId) ?? 0
        usedIds.set(baseId, occurrence + 1)
        const id = occurrence === 0 ? baseId : `${baseId}-${occurrence}`

        headings.push({ id, level: depth, text })
        return `<h${depth} id="${id}">${inlineHtml}</h${depth}>\n`
      },
      // 自定义 table renderer：把 GFM 表格包进横向滚动容器，
      // 替换原先对解析结果做 replaceAll('<table>'/…) 的脆弱字符串替换——
      // 后者会把代码块内字面 `</table>` 误闭合，且无法感知真实表格边界。
      table({ header, rows }) {
        const headerHtml = `<thead><tr>${header.map(cell => `<th>${this.parser.parseInline(cell.tokens)}</th>`).join('')}</tr></thead>`
        const bodyHtml = rows.length
          ? `<tbody>${rows.map(row => `<tr>${row.map(cell => `<td>${this.parser.parseInline(cell.tokens)}</td>`).join('')}</tr>`).join('')}</tbody>`
          : ''
        return `<div class="markdown-table-scroll"><table>${headerHtml}${bodyHtml}</table></div>\n`
      }
    }
  })

  const rawHtml = parser.parse(markdown, { async: false }) as string
  return {
    html: sanitizeMarkdownHtml(rawHtml),
    headings
  }
}

/**
 * 安全渲染 Markdown 为 HTML 字符串。
 *
 * 保持既有 API 不变；需要目录数据时使用 renderMarkdownWithOutline。
 *
 * @param markdown — Markdown 原始文本
 * @returns 净化后的 HTML 字符串
 * @throws 如果 Markdown 解析或 DOMPurify 净化失败，直接抛出错误
 */
export function renderMarkdown(markdown: string): string {
  return renderMarkdownWithOutline(markdown).html
}

/**
 * 从 Markdown 提取纯文本摘要（用于搜索结果 snippet 等）。
 *
 * @param markdown — Markdown 原始文本
 * @param maxLength — 最大长度
 * @returns 纯文本摘要
 */
export function extractPlainText(markdown: string, maxLength: number = 200): string {
  if (!markdown) {
    return ''
  }

  // 移除 Markdown 语法
  let text = markdown
    .replace(/^#{1,6}\s+/gm, '') // headings
    .replace(/\*\*([^*]+)\*\*/g, '$1') // bold
    .replace(/\*([^*]+)\*/g, '$1') // italic
    .replace(/`([^`]+)`/g, '$1') // inline code
    .replace(/\[([^\]]+)\]\([^)]+\)/g, '$1') // links
    .replace(/!\[([^\]]*)\]\([^)]+\)/g, '$1') // images
    .replace(/^\s*[-*+]\s+/gm, '') // list items
    .replace(/^\s*\d+\.\s+/gm, '') // numbered list
    .replace(/^\s*>\s+/gm, '') // blockquote
    .replace(/```[\s\S]*?```/g, '') // code blocks
    .replace(/\n{3,}/g, '\n\n') // multiple newlines

  text = text.trim()

  if (text.length > maxLength) {
    text = text.substring(0, maxLength) + '...'
  }

  return text
}
