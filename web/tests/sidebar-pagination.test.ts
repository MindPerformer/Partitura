// tests/sidebar-pagination.test.ts — WorkspaceLayout 文档分页加载行为测试
//
// 引入动机：WorkspaceLayout 的文档 sidebar 通过 loadDocuments 分页循环加载全部文档，
// 超过 100 条时继续请求第二页；MAX_DOC_PAGES=50 作为后端 total 异常时的硬熔断；
// loadMoreDocuments 在达到上限后继续分页。此测试挂载真实组件驱动真实逻辑，验证：
// 1. 文档数 > 100 时分页请求第二页，所有文档传入 WorkspaceSidebar
// 2. 首屏失败 / 第二页失败时错误可观察（ErrorDisplay 渲染）
// 3. 达到 MAX_DOC_PAGES 熔断上限时显示「加载更多」，点击后继续分页
// 4. 非法 total（total < 实际返回数）安全终止，不死循环
//
// 测试策略：
// - mockNuxtImport('useRoute') 提供可控路由，使 loadDocuments 拿到 workspaceId
// - mountSuspended 挂载真实 WorkspaceLayout（stub AppHeader / WorkspaceSidebar）
// - createFetchMock.setImpl 按 URL 中的 offset 返回对应分页数据
// - 通过 WorkspaceSidebar stub 的 data-doc-count 断言真实传入的文档数量

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended, mockNuxtImport } from '@nuxt/test-utils/runtime'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineComponent, h, ref } from 'vue'
import { createFetchMock } from './setup'
import type { DocumentListItem, ListDocumentsResponse } from '~/types/api'

const ctrl = createFetchMock()

// ============================================================
// 可控路由 mock：mockNuxtImport 在模块顶层调用一次，返回共享 route ref，
// 每个用例挂载前修改 route.value.params.id 使 useWorkspaceContext 按 id 加载。
// ============================================================
const routeRef = ref({
  path: '/',
  fullPath: '/',
  params: {} as Record<string, string>,
  query: {} as Record<string, string>
})

mockNuxtImport('useRoute', () => {
  return () => routeRef.value
})

// ============================================================
// locale 文案（同 search-profiles.test.ts 的模式：fs 读原始 JSON，断言接受中英）
// ============================================================
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

function makeDoc(i: number): DocumentListItem {
  return {
    id: `doc-${i}`,
    path: `docs/doc-${i}.md`,
    title: `Document ${i}`,
    type: '',
    status: 'active',
    content_hash: `hash-${i}`,
    revision_number: 1,
    is_special: false,
    updated_by: 'user-1',
    updated_at: '2025-01-01T00:00:00Z'
  }
}

const WORKSPACE = {
  id: 'ws-x',
  name: 'ws',
  display_name: 'WS',
  description: '',
  status: 'active',
  revision_retention_days: 0,
  revision_max_count: 0,
  max_document_size_bytes: 0,
  created_by: 'u1'
}

/** 供每个用例设置的 workspace id（每个用例使用不同 id 避免 contextCache 命中干扰断言） */
let currentWsId = 'ws-x'

/** WorkspaceSidebar stub：把传入的 documents 数量渲染成 data-doc-count，便于断言真实 prop。 */
const SidebarStub = defineComponent({
  name: 'WorkspaceSidebar',
  props: ['documents', 'workspaceId', 'currentPath', 'canEdit'],
  setup(props) {
    return () => h('div', { 'data-testid': 'sidebar', 'data-doc-count': String(props.documents.length) })
  }
})

/** 以已登录身份挂载 WorkspaceLayout；$fetch 按 URL 路由到 workspace / membership / documents 接口。 */
async function mountLayout(totalDocs: number, opts: { failOnCall?: number; docsPerPage?: number } = {}) {
  const perPage = opts.docsPerPage ?? 100
  const failOnCall = opts.failOnCall ?? -1
  let docCallCount = 0

  ctrl.setImpl(async (url, rawOpts) => {
    const o = rawOpts as { method?: string }
    const method = o.method ?? 'GET'
    if (url.includes('/me/membership')) {
      return { _data: { workspace_id: currentWsId, role: 'owner' }, status: 200 }
    }
    if (method === 'GET' && url.includes(`/workspaces/${currentWsId}/documents`)) {
      docCallCount++
      if (failOnCall > 0 && docCallCount === failOnCall) {
        throw { response: { status: 503, _data: { error: 'service unavailable' } }, message: 'FetchError' }
      }
      const u = new URL(url, 'http://test.local')
      const offset = parseInt(u.searchParams.get('offset') || '0', 10)
      const limit = parseInt(u.searchParams.get('limit') || String(perPage), 10)
      const docs: DocumentListItem[] = []
      const end = Math.min(offset + limit, totalDocs)
      for (let i = offset; i < end; i++) docs.push(makeDoc(i))
      return { _data: { documents: docs, total: totalDocs, limit, offset } as ListDocumentsResponse, status: 200 }
    }
    if (method === 'GET' && url.includes(`/workspaces/${currentWsId}`)) {
      return { _data: { ...WORKSPACE, id: currentWsId }, status: 200 }
    }
    throw new Error(`测试未覆盖的请求：${method} ${url}`)
  })

  const Layout = await import('~/components/WorkspaceLayout.vue')
  const wrapper = await mountSuspended(Layout.default, {
    attachTo: document.body,
    global: { stubs: { AppHeader: true, WorkspaceSidebar: SidebarStub } }
  })
  await flushPromises()
  return wrapper
}

/** 读取 sidebar stub 上记录的文档数（真实传入 WorkspaceSidebar 的 documents prop 长度）。 */
function loadedDocCount(): number {
  const el = document.querySelector('[data-testid="sidebar"]')
  return el ? parseInt(el.getAttribute('data-doc-count') ?? '0', 10) : -1
}

/** 找到「加载更多」按钮（中英文案都接受）。 */
function findLoadMoreButton(wrapper: { findAll: (s: string) => Array<{ text: () => string; trigger: (e: string) => Promise<void> }> }) {
  const candidates = labelsOf('common.loadMore')
  return wrapper.findAll('button').find(b => candidates.includes(b.text().trim()))
}

describe('WorkspaceLayout 文档分页加载（真实组件）', () => {
  beforeEach(() => {
    ctrl.reset()
    routeRef.value = { path: '/', fullPath: '/', params: {}, query: {} }
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('文档数 > 100 时自动分页加载第二页，全部文档传入 sidebar', async () => {
    currentWsId = 'ws-p1'
    routeRef.value.params.id = 'ws-p1'
    const wrapper = await mountLayout(250)

    await flushPromises()
    expect(loadedDocCount()).toBe(250)
    // 发起了 3 次文档分页请求（offset 0/100/200）
    const docCalls = ctrl.mockFn.mock.calls.filter(c => String(c[0]).includes('/documents'))
    expect(docCalls.length).toBe(3)
    expect(String(docCalls[1]![0])).toContain('offset=100')
    expect(String(docCalls[2]![0])).toContain('offset=200')
    wrapper.unmount()
  })

  it('文档数恰好 100 时只请求一页，不显示「加载更多」', async () => {
    currentWsId = 'ws-p2'
    routeRef.value.params.id = 'ws-p2'
    const wrapper = await mountLayout(100)

    await flushPromises()
    expect(loadedDocCount()).toBe(100)
    const docCalls = ctrl.mockFn.mock.calls.filter(c => String(c[0]).includes('/documents'))
    expect(docCalls.length).toBe(1)
    expect(findLoadMoreButton(wrapper)).toBeUndefined()
    wrapper.unmount()
  })

  it('首屏 API 失败时错误可观察且不渲染部分数据', async () => {
    currentWsId = 'ws-p3'
    routeRef.value.params.id = 'ws-p3'
    const wrapper = await mountLayout(0, { failOnCall: 1 })

    await flushPromises()
    // documents 为空
    expect(loadedDocCount()).toBe(0)
    // docsError 渲染到 ErrorDisplay
    expect(wrapper.text()).toContain('service unavailable')
    wrapper.unmount()
  })

  it('第二页失败时 documents 清空、错误可观察', async () => {
    currentWsId = 'ws-p4'
    routeRef.value.params.id = 'ws-p4'
    // total=250 需 3 页；第 2 页失败
    const wrapper = await mountLayout(250, { failOnCall: 2 })

    await flushPromises()
    // 真实实现：catch 分支将 documents 清空
    expect(loadedDocCount()).toBe(0)
    expect(wrapper.text()).toContain('service unavailable')
    wrapper.unmount()
  })

  it('非法 total（total < 已加载数）第一页即安全终止，不死循环', async () => {
    currentWsId = 'ws-p5'
    routeRef.value.params.id = 'ws-p5'
    // 模拟 total 异常偏小（5），但每页返回 100 条
    ctrl.setImpl(async (url, rawOpts) => {
      const o = rawOpts as { method?: string }
      const method = o.method ?? 'GET'
      if (url.includes('/me/membership')) {
        return { _data: { workspace_id: 'ws-p5', role: 'owner' }, status: 200 }
      }
      if (method === 'GET' && url.includes('/workspaces/ws-p5/documents')) {
        const u = new URL(url, 'http://test.local')
        const offset = parseInt(u.searchParams.get('offset') || '0', 10)
        const docs: DocumentListItem[] = []
        for (let i = offset; i < offset + 100; i++) docs.push(makeDoc(i))
        return { _data: { documents: docs, total: 5, limit: 100, offset }, status: 200 }
      }
      if (method === 'GET' && url.includes('/workspaces/ws-p5')) {
        return { _data: { ...WORKSPACE, id: 'ws-p5' }, status: 200 }
      }
      throw new Error(`测试未覆盖的请求：${method} ${url}`)
    })

    const Layout = await import('~/components/WorkspaceLayout.vue')
    const wrapper = await mountSuspended(Layout.default, {
      attachTo: document.body,
      global: { stubs: { AppHeader: true, WorkspaceSidebar: SidebarStub } }
    })
    await flushPromises()

    // all.length=100 >= total=5，第一页即终止
    const docCalls = ctrl.mockFn.mock.calls.filter(c => String(c[0]).includes('/documents'))
    expect(docCalls.length).toBe(1)
    expect(loadedDocCount()).toBe(100)
    wrapper.unmount()
  })

  it('达到 MAX_DOC_PAGES 熔断后显示「加载更多」，点击后继续分页直到 total', async () => {
    currentWsId = 'ws-p6'
    routeRef.value.params.id = 'ws-p6'
    // total = 50*100 + 60 = 5060：首轮 50 页熔断后 docsHasMore=true
    const wrapper = await mountLayout(5060)

    await flushPromises()
    // 熔断：首轮 50 页全部拉取，共 5000 条，未达 total → 显示「加载更多」
    const initialCalls = ctrl.mockFn.mock.calls.filter(c => String(c[0]).includes('/documents'))
    expect(initialCalls.length).toBe(50)
    expect(loadedDocCount()).toBe(5000)

    const moreBtn = findLoadMoreButton(wrapper)
    expect(moreBtn).toBeTruthy()

    // 点击「加载更多」：继续分页 1 页（offset 5000）后 all=5060 >= total → 结束
    await moreBtn!.trigger('click')
    await flushPromises()

    const allCalls = ctrl.mockFn.mock.calls.filter(c => String(c[0]).includes('/documents'))
    expect(allCalls.length).toBe(51)
    expect(String(allCalls[50]![0])).toContain('offset=5000')
    expect(loadedDocCount()).toBe(5060)
    // 已达 total，「加载更多」消失
    expect(findLoadMoreButton(wrapper)).toBeUndefined()
    wrapper.unmount()
  })
})
