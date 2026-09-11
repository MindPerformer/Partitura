// tests/members.test.ts — 工作区成员页「添加成员」候选选择回归测试
//
// 回归对象：UInputMenu 的 resetSearchTermOnSelect 默认 true，选中候选时会把
// searchTerm（绑定 addForm.username）先置空再由 onCandidateSelect 回填，
// 中间态 "" 触发 username watcher 误判输入不一致而清空 selectedCandidate，
// 导致提交时报「请先从候选列表选择用户」。修复后选中即可直接提交。
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended, mockNuxtImport } from '@nuxt/test-utils/runtime'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { ref } from 'vue'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

const routeRef = ref({
  path: '/workspaces/ws-1/members',
  fullPath: '/workspaces/ws-1/members',
  params: { id: 'ws-1' } as Record<string, string>,
  query: {} as Record<string, string>
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

const WORKSPACE = {
  id: 'ws-1', name: 'ws', display_name: 'WS', description: '', status: 'active',
  revision_retention_days: 0, revision_max_count: 0, max_document_size_bytes: 0, created_by: 'u1'
}
const OWNER_MEMBER = { id: 'm-0', user_id: 'u1', username: 'alice', email: 'a@x.com', role: 'owner' }
const CANDIDATE = { id: 'u-2', user_id: 'u-2', username: 'bob', email: 'b@x.com', role: 'viewer' }

const calls: Array<{ method: string; url: string; body?: unknown }> = []

async function mountPage() {
  ctrl.setImpl(async (url, rawOpts) => {
    const o = rawOpts as { method?: string; body?: unknown }
    const method = o.method ?? 'GET'
    calls.push({ method, url, body: o.body })
    if (url.includes('/me/membership')) {
      return { _data: { workspace_id: 'ws-1', role: 'owner' }, status: 200 }
    }
    if (method === 'GET' && url.includes('/members/candidates')) {
      return { _data: { users: [CANDIDATE] }, status: 200 }
    }
    if (method === 'GET' && url.includes('/members')) {
      return { _data: { members: [OWNER_MEMBER], total: 1, limit: 20, offset: 0 }, status: 200 }
    }
    if (method === 'GET' && url.includes('/workspaces/ws-1')) {
      return { _data: WORKSPACE, status: 200 }
    }
    if (method === 'POST' && url.includes('/members')) {
      return { _data: CANDIDATE, status: 201 }
    }
    throw new Error(`测试未覆盖的请求：${method} ${url}`)
  })

  const Page = await import('~/pages/workspaces/[id]/members.vue')
  const wrapper = await mountSuspended(Page.default, { attachTo: document.body })
  await flushPromises()
  return wrapper
}

describe('members.vue — 添加成员候选选择', () => {
  beforeEach(() => {
    ctrl.reset()
    calls.length = 0
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
    document.body.innerHTML = ''
  })

  it('从候选列表选中用户后提交不应提示重新选择', async () => {
    const wrapper = await mountPage()

    // 打开添加弹窗
    const addBtn = wrapper.findAll('button').find(b => labelsOf('workspace.addMember').includes(b.text().trim()))
    expect(addBtn).toBeTruthy()
    await addBtn!.trigger('click')
    await flushPromises()

    // 找到弹窗中的输入框（combobox input）
    const input = document.querySelector<HTMLInputElement>('[data-slot="content"] input[role="combobox"], [data-slot="content"] input')
    expect(input).toBeTruthy()

    // 输入触发候选搜索（防抖 250ms）
    input!.value = 'bo'
    input!.dispatchEvent(new Event('input', { bubbles: true }))
    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()

    // 点击候选项
    const items = Array.from(document.querySelectorAll('[role="option"], [data-slot="item"]'))
    const opt = items.find(el => el.textContent?.includes('bob'))
    expect(opt, '应渲染候选 bob').toBeTruthy()
    ;(opt as HTMLElement).dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await flushPromises()
    await vi.advanceTimersByTimeAsync(50)
    await flushPromises()

    // 提交表单
    const form = document.querySelector<HTMLFormElement>('[data-slot="content"] form')
    form!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    await flushPromises()

    const content = document.querySelector('[data-slot="content"]')
    const text = content?.textContent ?? ''
    const requiredMsgs = labelsOf('workspace.memberCandidateRequired')
    expect(requiredMsgs.some(m => text.includes(m)), '不应显示「请先从候选列表选择用户」').toBe(false)

    const post = calls.find(c => c.method === 'POST' && c.url.includes('/members'))
    expect(post, '应发出添加成员请求').toBeTruthy()
  })
})
