// tests/source-form.test.ts — Add Source 表单校验回归测试
//
// 引入动机：edit.vue 的 validateSourceForm 校验 sourceForm 必填与 web 类型 URL 合法性，
// 此前未覆盖。修复后 value 必填、web 类型必须是合法 http/https URL、retrieved_at 必须是
// 合法日期；非法输入不发出 addSource 请求并在字段上显示本地化错误。
//
// 测试策略：mountSuspended 挂载真实 edit.vue（编辑模式），打开 Add Source 弹窗，
// 填非法值提交，断言不发 POST /documents/sources 且显示错误文案。

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended, mockNuxtImport } from '@nuxt/test-utils/runtime'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineComponent, h, ref } from 'vue'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

const routeRef = ref({
  path: '/workspaces/ws-1/documents/edit',
  fullPath: '/workspaces/ws-1/documents/edit?path=docs/a.md',
  params: { id: 'ws-1' } as Record<string, string>,
  query: { path: 'docs/a.md' } as Record<string, string>
})

mockNuxtImport('useRoute', () => {
  return () => routeRef.value
})

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

const DOC = {
  id: 'doc-1', workspace_id: 'ws-1', path: 'docs/a.md', title: 'Doc A',
  type: '', status: 'active', content_markdown: '# A', content_hash: 'h1',
  revision_number: 1, is_special: false, created_by: 'u1', updated_by: 'u1',
  created_at: '2025-01-01T00:00:00Z', updated_at: '2025-01-01T00:00:00Z'
}

const WORKSPACE = {
  id: 'ws-1', name: 'ws', display_name: 'WS', description: '', status: 'active',
  revision_retention_days: 0, revision_max_count: 0, max_document_size_bytes: 0, created_by: 'u1'
}

interface RecordedCall { url: string; method: string; body?: Record<string, unknown> }
let calls: RecordedCall[] = []
let mounted: { unmount: () => void; text: () => string; findAll: (s: string) => Array<{ text: () => string; trigger: (e: string) => Promise<void> }> } | null = null

/** 挂载编辑模式的 edit.vue；$fetch 按 URL 路由到 workspace / membership / read / sources。 */
async function mountEditPage() {
  calls = []
  ctrl.setImpl(async (url, rawOpts) => {
    const o = rawOpts as { method?: string; body?: string }
    const method = o.method ?? 'GET'
    calls.push({ url, method, body: o.body ? JSON.parse(o.body) : undefined })

    if (url.includes('/me/membership')) {
      return { _data: { workspace_id: 'ws-1', role: 'owner' }, status: 200 }
    }
    if (url.includes('/documents/sources')) {
      if (method === 'GET') return { _data: { sources: [], total: 0, limit: 50, offset: 0 }, status: 200 }
      if (method === 'POST') return { _data: { id: 'src-1', document_id: 'doc-1', source_type: 'web', value: '', created_by: 'u1', created_at: '2025-01-01T00:00:00Z' }, status: 201 }
    }
    if (url.includes('/documents/read')) {
      return { _data: DOC, status: 200 }
    }
    if (method === 'GET' && url.includes('/documents')) {
      return { _data: { documents: [], total: 0, limit: 100, offset: 0 }, status: 200 }
    }
    if (method === 'GET' && url.includes('/workspaces/ws-1')) {
      return { _data: WORKSPACE, status: 200 }
    }
    throw new Error(`测试未覆盖的请求：${method} ${url}`)
  })

  const { useAuth } = await import('~/composables/useAuth')
  useAuth().setAuth({
    user: { id: 'u1', username: 'test', system_role: 'user' },
    csrf_token: 'token',
    expires_at: '2025-01-01'
  })

  const Page = await import('~/pages/workspaces/[id]/documents/edit.vue')
  // stub USelect：documentTypes 含 value:'' 的「无类型」项，reka-ui SelectItem
  // 在 jsdom 渲染空串 value 会抛错。校验逻辑不依赖下拉渲染，stub 为占位元素。
  const USelectStub = defineComponent({
    name: 'USelect',
    props: ['modelValue', 'items', 'valueKey', 'labelKey'],
    setup(props) {
      return () => h('div', { 'data-testid': 'uselect', 'data-value': String(props.modelValue) })
    }
  })
  const wrapper = await mountSuspended(Page.default, {
    attachTo: document.body,
    global: { stubs: { AppHeader: true, USelect: USelectStub } }
  })
  await flushPromises()
  mounted = wrapper
  return wrapper
}

/** 是否已发起 POST /documents/sources 请求。 */
function addSourceRequested(): boolean {
  return calls.some(c => c.method === 'POST' && c.url.includes('/documents/sources'))
}

/** 打开 Add Source 弹窗（点击 document.addSource 按钮）。 */
async function openSourceModal(wrapper: { findAll: (s: string) => Array<{ text: () => string; trigger: (e: string) => Promise<void> }> }) {
  const candidates = labelsOf('document.addSource')
  const btn = wrapper.findAll('button').find(b => candidates.includes(b.text().trim()))
  expect(btn, '应渲染「添加来源」按钮').toBeTruthy()
  await btn!.trigger('click')
  await flushPromises()
}

/** 在 source 弹窗中按 i18n label 定位输入框并填值。 */
function fillSourceValue(value: string) {
  const candidates = labelsOf('document.sourceValue')
  const label = Array.from(document.querySelectorAll<HTMLLabelElement>('[data-slot="content"] [data-slot="label"]'))
    .find(el => candidates.includes((el.textContent ?? '').replace(/\s*\*\s*$/, '').trim()))
  if (!label) throw new Error('未找到 sourceValue 字段 label')
  const input = document.getElementById(label.getAttribute('for') ?? '')
  if (!(input instanceof HTMLInputElement)) throw new Error('sourceValue 不是 input')
  input.value = value
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

/** 提交 source 弹窗表单。 */
async function submitSourceForm() {
  const form = document.querySelector<HTMLFormElement>('[data-slot="content"] form')
  if (!form) throw new Error('source 弹窗表单未渲染')
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
  await flushPromises()
}

/** 读取弹窗内当前展示的错误文案。 */
function modalText(): string {
  return document.querySelector('[data-slot="content"]')?.textContent ?? ''
}

describe('Add Source 表单校验（validateSourceForm）', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  afterEach(() => {
    mounted?.unmount()
    mounted = null
    vi.restoreAllMocks()
    // 清理可能残留的弹窗
    document.querySelectorAll('[data-slot="content"]').forEach(el => el.remove())
  })

  it('value 为空时显示必填错误且不发请求', async () => {
    const wrapper = await mountEditPage()
    await openSourceModal(wrapper)

    // value 留空直接提交
    await submitSourceForm()

    expect(addSourceRequested()).toBe(false)
    const text = modalText()
    expect(labelsOf('document.sourceValueRequired').some(l => text.includes(l))).toBe(true)
  })

  it('web 类型 value 不是合法 URL 时显示 URL 校验错误且不发请求', async () => {
    const wrapper = await mountEditPage()
    await openSourceModal(wrapper)

    fillSourceValue('not-a-valid-url')
    await submitSourceForm()

    expect(addSourceRequested()).toBe(false)
    const text = modalText()
    expect(labelsOf('document.sourceValueInvalidUrl').some(l => text.includes(l))).toBe(true)
  })

  it('web 类型合法 https URL 通过校验并发出 addSource 请求', async () => {
    const wrapper = await mountEditPage()
    await openSourceModal(wrapper)

    fillSourceValue('https://example.com/spec')
    await submitSourceForm()

    expect(addSourceRequested()).toBe(true)
    const srcCall = calls.find(c => c.method === 'POST' && c.url.includes('/documents/sources'))
    expect(srcCall?.body?.value).toBe('https://example.com/spec')
    expect(srcCall?.body?.source_type).toBe('web')
  })
})
