// tests/search-profiles.test.ts — Search Profiles 响应契约、新建版本与归档测试
//
// 引入动机：Phase6 WP1 修复 search-profiles 运行时崩溃。
// 覆盖 null、空数组、snake_case 正常数据、非法响应等边界，
// 确保 API boundary 验证不通过时显示本地化错误、profiles 为 []、不抛异常。
//
// 后续扩展：搜索配置"修改 = 基于现有配置新建版本"、"删除 = 归档"落地在
// pages/admin/search-profiles.vue。这里额外覆盖 API 契约（方法/路径/请求体）
// 与页面交互（预填源 profile、弹窗暴露并提交全部 25 个可调参数、归档按钮可见性、409 报错展示）。

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

// ============================================================
// locale 文案读取
//
// 测试环境无法直接 import locales/*.json 取原文（该路径会被 @nuxtjs/i18n 的
// 构建插件转换为编译后的消息对象），因此用 fs 读取原始 JSON。
// 页面断言同时接受中英文案，不依赖测试环境的语言检测结果（默认可能是 en）。
// ============================================================

const zhLocale = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'zh.json'), 'utf-8')) as Record<string, unknown>
const enLocale = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'en.json'), 'utf-8')) as Record<string, unknown>

function localeText(locale: Record<string, unknown>, path: string): unknown {
  return path.split('.').reduce<unknown>(
    (acc, key) => (acc as Record<string, unknown> | undefined)?.[key],
    locale
  )
}

/** 返回某个 i18n key 的中英文案（两者都必须存在）。 */
function labelsOf(path: string): string[] {
  const zh = localeText(zhLocale, path)
  const en = localeText(enLocale, path)
  if (typeof zh !== 'string' || typeof en !== 'string') {
    throw new Error(`locale 缺少 ${path}`)
  }
  return [zh, en]
}

describe('useSearchProfiles', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('null 响应被识别为非法契约，显示错误且 profiles 为 []', async () => {
    ctrl.setResponse(null)

    const { useSearchProfiles } = await import('~/composables/useSearchProfiles')
    const { profiles, total, error, load } = useSearchProfiles()

    await load()

    expect(error.value).toContain('服务器返回的配置列表格式不正确')
    expect(profiles.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('空数组响应正常处理，profiles 为 []，无错误', async () => {
    ctrl.setResponse({ profiles: [], total: 0, limit: 20, offset: 0 })

    const { useSearchProfiles } = await import('~/composables/useSearchProfiles')
    const { profiles, total, error, load } = useSearchProfiles()

    await load()

    expect(error.value).toBeNull()
    expect(profiles.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('snake_case 正常数据被正确解析并暴露', async () => {
    const profile = {
      id: 'sp-1',
      name: 'default',
      version: 1,
      status: 'active',
      embedding_provider: 'openai-compatible',
      embedding_model: 'qwen3-embedding',
      embedding_dimensions: 1024,
      embedding_query_instruction: 'query',
      embedding_document_instruction: 'doc',
      chunk_algorithm_version: 'v1',
      chunk_target_size: 512,
      chunk_overlap: 64,
      chunk_parent_section_behavior: 'include',
      title_boost: 2,
      heading_boost: 1.5,
      path_boost: 1,
      tags_boost: 0.5,
      body_boost: 1,
      analyzer: 'standard',
      lexical_top_k: 50,
      vector_top_k: 50,
      rrf_k: 60,
      reranker_provider: 'openai-compatible',
      reranker_model: 'qwen3-reranker',
      reranker_candidate_count: 20,
      reranker_final_count: 10,
      max_chunks_per_document: 3,
      merge_adjacent_chunks: true,
      es_index_name: 'idx_v1',
      max_p95_latency_ms: 2000,
      max_reranker_cost_per_query: 0.01,
      created_by: 'u1',
      created_at: '2025-01-01T00:00:00Z',
      activated_at: '2025-01-02T00:00:00Z'
    }
    ctrl.setResponse({ profiles: [profile], total: 1, limit: 20, offset: 0 })

    const { useSearchProfiles } = await import('~/composables/useSearchProfiles')
    const { profiles, total, error, load } = useSearchProfiles()

    await load()

    expect(error.value).toBeNull()
    expect(total.value).toBe(1)
    expect(profiles.value.length).toBe(1)

    const p = profiles.value[0]
    if (!p) throw new Error('expected profile at index 0')
    expect(p.embedding_model).toBe('qwen3-embedding')
    expect(p.lexical_top_k).toBe(50)
    expect(p.max_p95_latency_ms).toBe(2000)
    expect(p.activated_at).toBe('2025-01-02T00:00:00Z')
  })

  it('非法响应（profiles 不是数组）显示错误且 profiles 为 []', async () => {
    ctrl.setResponse({ profiles: null, total: 0, limit: 20, offset: 0 })

    const { useSearchProfiles } = await import('~/composables/useSearchProfiles')
    const { profiles, total, error, load } = useSearchProfiles()

    await load()

    expect(error.value).not.toBeNull()
    expect(profiles.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('total 不是 number 被视为非法响应', async () => {
    ctrl.setResponse({ profiles: [], total: 'zero' })

    const { useSearchProfiles } = await import('~/composables/useSearchProfiles')
    const { profiles, total, error, load } = useSearchProfiles()

    await load()

    expect(error.value).not.toBeNull()
    expect(profiles.value).toEqual([])
    expect(total.value).toBe(0)
  })

  it('API 异常被正确捕获为错误消息', async () => {
    ctrl.setError({
      response: { status: 500, _data: { error: 'internal error' } },
      message: 'FetchError'
    })

    const { useSearchProfiles } = await import('~/composables/useSearchProfiles')
    const { error, load } = useSearchProfiles()

    await load()

    expect(error.value).toBe('internal error')
  })
})

// ============================================================
// useSearchAdminApi — 新建版本 / 归档 契约
// ============================================================

describe('useSearchAdminApi — createProfileVersion / archiveProfile', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('createProfileVersion 调用 POST /admin/search-profiles/{id}/versions 并携带覆盖字段', async () => {
    ctrl.setResponse({ id: 'sp-2', name: 'v2', version: 4, status: 'draft' })

    const { useSearchAdminApi } = await import('~/composables/useApi')
    const res = await useSearchAdminApi().createProfileVersion('sp-1', {
      name: 'v2',
      embedding_dimensions: 512
    })

    expect(res.id).toBe('sp-2')
    expect(res.status).toBe('draft')

    expect(ctrl.mockFn).toHaveBeenCalledTimes(1)
    const call = ctrl.mockFn.mock.calls[0]!
    expect(call[0] as string).toContain('/admin/search-profiles/sp-1/versions')
    const opts = call[1] as { method: string; body: string }
    expect(opts.method).toBe('POST')
    // 未提供的字段不出现在请求体中，由服务端继承源 profile
    expect(JSON.parse(opts.body)).toEqual({ name: 'v2', embedding_dimensions: 512 })
  })

  it('createProfileVersion 允许只覆盖单个字段', async () => {
    ctrl.setResponse({ id: 'sp-2', version: 4, status: 'draft' })

    const { useSearchAdminApi } = await import('~/composables/useApi')
    await useSearchAdminApi().createProfileVersion('sp-1', { title_boost: 3 })

    const call = ctrl.mockFn.mock.calls[0]!
    const opts = call[1] as { method: string; body: string }
    expect(opts.method).toBe('POST')
    expect(JSON.parse(opts.body)).toEqual({ title_boost: 3 })
  })

  it('archiveProfile 调用 POST /admin/search-profiles/{id}/archive 且不带请求体', async () => {
    ctrl.setResponse({ status: 'archived' })

    const { useSearchAdminApi } = await import('~/composables/useApi')
    const res = await useSearchAdminApi().archiveProfile('sp-1')

    expect(res.status).toBe('archived')

    const call = ctrl.mockFn.mock.calls[0]!
    expect(call[0] as string).toContain('/admin/search-profiles/sp-1/archive')
    const opts = call[1] as { method: string; body?: unknown }
    expect(opts.method).toBe('POST')
    expect(opts.body).toBeUndefined()
  })

  it('archiveProfile 409 时抛出带 status 和后端消息的错误', async () => {
    ctrl.setError({
      response: { status: 409, _data: { error: 'active profile cannot be archived' } },
      message: 'FetchError'
    })

    const { useSearchAdminApi } = await import('~/composables/useApi')

    try {
      await useSearchAdminApi().archiveProfile('sp-1')
      expect.fail('active profile 归档应当抛出 409')
    } catch (err: unknown) {
      const apiErr = err as { status: number; error: string }
      expect(apiErr.status).toBe(409)
      expect(apiErr.error).toBe('active profile cannot be archived')
    }
  })
})

// ============================================================
// search-profiles 页面 — 新建版本 / 归档 交互
// ============================================================

/** 页面 defaultForm 的硬编码默认值之一，用于确认弹窗预填并非来自默认值。 */
const DEFAULT_FORM_EMBEDDING_MODEL = 'qwen3-embedding'

/** 被选中的源 profile：各字段值刻意与页面 defaultForm 的默认值全部不同。 */
const sourceProfile = {
  id: 'sp-1',
  name: 'profile-custom-name',
  version: 3,
  status: 'draft',
  embedding_provider: 'profile-embedding-provider',
  embedding_model: 'profile-model-x',
  embedding_dimensions: 768,
  embedding_query_instruction: 'query-instruction',
  embedding_document_instruction: 'document-instruction',
  chunk_target_size: 999,
  chunk_overlap: 11,
  title_boost: 9,
  heading_boost: 8,
  path_boost: 0.7,
  tags_boost: 0.6,
  body_boost: 7,
  analyzer: 'profile-analyzer',
  lexical_top_k: 11,
  vector_top_k: 22,
  rrf_k: 33,
  reranker_provider: 'profile-reranker-provider',
  reranker_model: 'profile-reranker',
  reranker_candidate_count: 77,
  reranker_final_count: 17,
  max_chunks_per_document: 4,
  merge_adjacent_chunks: false,
  max_p95_latency_ms: 1234,
  max_reranker_cost_per_query: 0.42,
  es_index_name: 'idx_v3',
  created_by: 'u1',
  created_at: '2025-01-01T00:00:00Z'
}

/**
 * 弹窗必须暴露的全部可调参数（25 个，与 defaultForm / 后端 createProfileRequest 一致）。
 *
 * 引入动机：新建版本请求体现在是"整份覆盖"（name + 24 个参数），不再只发送 11 个字段；
 * 该清单同时用于断言"全部字段都渲染且都被发送"，避免新增参数时漏掉 UI 或请求体。
 */
const PROFILE_FIELDS = [
  'name',
  'embedding_provider',
  'embedding_model',
  'embedding_dimensions',
  'embedding_query_instruction',
  'embedding_document_instruction',
  'chunk_target_size',
  'chunk_overlap',
  'title_boost',
  'heading_boost',
  'path_boost',
  'tags_boost',
  'body_boost',
  'analyzer',
  'lexical_top_k',
  'vector_top_k',
  'rrf_k',
  'reranker_provider',
  'reranker_model',
  'reranker_candidate_count',
  'reranker_final_count',
  'max_chunks_per_document',
  'merge_adjacent_chunks',
  'max_p95_latency_ms',
  'max_reranker_cost_per_query'
]

interface RecordedCall {
  url: string
  method: string
  body?: Record<string, unknown>
}

interface MountPageOptions {
  /** versions 接口返回体，默认返回一个 draft 新版本 */
  versionResponse?: Record<string, unknown>
  /** versions 接口抛出的错误，用于覆盖创建失败分支 */
  versionError?: unknown
  /** archive 接口抛出的错误，默认成功返回 {"status":"archived"} */
  archiveError?: unknown
}

let calls: RecordedCall[] = []
let mounted: { unmount: () => void } | null = null

/** 以 system_admin 身份挂载页面，并把 $fetch 桩按 URL 路由到列表 / 新建版本 / 归档接口。 */
async function mountPage(profiles: Array<Record<string, unknown>>, options: MountPageOptions = {}) {
  calls = []
  const { useAuth } = await import('~/composables/useAuth')
  useAuth().setAuth({
    user: { id: '1', username: 'admin', system_role: 'system_admin' },
    csrf_token: 'token',
    expires_at: '2025-01-01'
  } as never)

  ctrl.setImpl(async (url, rawOptions) => {
    const opts = rawOptions as { method?: string; body?: string }
    const method = opts.method ?? 'GET'
    calls.push({ url, method, body: opts.body ? JSON.parse(opts.body) : undefined })

    if (url.includes('/versions')) {
      if (options.versionError) throw options.versionError
      return { _data: options.versionResponse ?? { ...sourceProfile, id: 'sp-2', version: 4 }, status: 201 }
    }
    if (url.includes('/archive')) {
      if (options.archiveError) throw options.archiveError
      return { _data: { status: 'archived' }, status: 200 }
    }
    // 创建配置：POST /admin/search-profiles（不带子路径）
    if (method === 'POST' && url.includes('/admin/search-profiles')) {
      return { _data: { ...sourceProfile, id: 'sp-created', version: 1 }, status: 201 }
    }
    if (method === 'GET' && url.includes('/admin/search-profiles')) {
      return { _data: { profiles, total: profiles.length, limit: 20, offset: 0 }, status: 200 }
    }
    throw new Error(`测试未覆盖的请求：${method} ${url}`)
  })

  const Page = await import('~/pages/admin/search-profiles.vue')
  const wrapper = await mountSuspended(Page.default, {
    // 弹窗内容 teleport 到 document.body，挂到 body 才能与真实弹窗交互。
    // AppHeader 在本页只负责 workspace 切换，stub 掉以免它请求 /workspaces。
    // 注意：不要 stub teleport，否则 reka-ui 的 portal 不会渲染弹窗内容。
    attachTo: document.body,
    global: { stubs: { AppHeader: true } }
  })
  await flushPromises()
  mounted = wrapper
  return wrapper
}

/** 只依赖 VTU 包装器中实际用到的两个方法，避免在测试里引入组件泛型参数。 */
interface ButtonFinder {
  findAll: (selector: string) => Array<{ text: () => string; trigger: (event: string) => Promise<void> }>
}

/** 在页面卡片中按 i18n key 查找按钮（中英文案都接受）。 */
function findButton(wrapper: ButtonFinder, path: string) {
  const candidates = labelsOf(path)
  return wrapper.findAll('button').find(button => candidates.includes(button.text().trim()))
}

/** 弹窗内容是否已挂载（Nuxt UI 的 Modal 内容 teleport 到 body）。 */
function modalContent(): Element | null {
  return document.querySelector('[data-slot="content"]')
}

/** 轮询等待条件成立：弹窗关闭经过 reka-ui 的离场处理，单次 flushPromises 不够。 */
async function waitFor(condition: () => boolean, failureMessage: string, timeoutMs = 1000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (condition()) return
    await new Promise(resolvePromise => setTimeout(resolvePromise, 10))
  }
  throw new Error(failureMessage)
}

/** 按 i18n key 定位弹窗中的字段控件：通过 label[for] → #id 关联，不依赖字段顺序。 */
function modalField(path: string): HTMLElement {
  const candidates = labelsOf(path)
  const label = Array.from(document.querySelectorAll<HTMLLabelElement>('[data-slot="content"] [data-slot="label"]'))
    .find(element => candidates.includes((element.textContent ?? '').trim()))
  if (!label) {
    throw new Error(`弹窗中未找到字段 ${path}（${candidates.join(' / ')}）`)
  }
  const field = document.getElementById(label.getAttribute('for') ?? '')
  if (!field) {
    throw new Error(`字段 ${path} 未关联到控件`)
  }
  return field
}

/** 单行输入框（含 number 类型）字段。 */
function modalInput(path: string): HTMLInputElement {
  const field = modalField(path)
  if (!(field instanceof HTMLInputElement)) {
    throw new Error(`字段 ${path} 不是 input 控件`)
  }
  return field
}

/** 多行文本字段（embedding query/document instruction）。 */
function modalTextarea(path: string): HTMLTextAreaElement {
  const field = modalField(path)
  if (!(field instanceof HTMLTextAreaElement)) {
    throw new Error(`字段 ${path} 不是 textarea 控件`)
  }
  return field
}

/** 开关字段（merge_adjacent_chunks）：reka-ui 的 switch 渲染为 button[role="switch"]。 */
function modalSwitch(path: string): HTMLElement {
  const field = modalField(path)
  if (field.getAttribute('role') !== 'switch') {
    throw new Error(`字段 ${path} 不是开关控件`)
  }
  return field
}

/** 模拟用户输入：v-model 监听原生 input 事件。 */
function typeInto(input: HTMLInputElement | HTMLTextAreaElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

/** 提交当前弹窗表单（弹窗内容 teleport 到 body，因此从 document 取表单）。 */
async function submitModalForm() {
  const form = document.querySelector<HTMLFormElement>('[data-slot="content"] form')
  if (!form) {
    throw new Error('弹窗表单未渲染')
  }
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
  await flushPromises()
}

/** 记录页面发出的列表请求数量。 */
function listCallCount(): number {
  return calls.filter(call => call.method === 'GET' && call.url.includes('/admin/search-profiles')).length
}

describe('search-profiles 页面 — 新建版本与归档操作', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  afterEach(() => {
    mounted?.unmount()
    mounted = null
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('draft profile 渲染新建版本与归档按钮', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }])

    expect(findButton(wrapper, 'admin.newProfileVersion')).toBeTruthy()
    expect(findButton(wrapper, 'admin.archiveProfile')).toBeTruthy()
  })

  it('active profile 不渲染归档按钮（后端会以 409 拒绝）', async () => {
    const wrapper = await mountPage([{ ...sourceProfile, status: 'active' }])

    expect(findButton(wrapper, 'admin.archiveProfile')).toBeUndefined()
    // 既有行为保持不变：active 不显示激活按钮、非 archived 不显示回滚按钮
    expect(findButton(wrapper, 'admin.activateProfile')).toBeUndefined()
    expect(findButton(wrapper, 'admin.rollbackProfile')).toBeUndefined()
  })

  it('archived profile 不渲染归档按钮，但仍可回滚', async () => {
    const wrapper = await mountPage([{ ...sourceProfile, status: 'archived' }])

    expect(findButton(wrapper, 'admin.archiveProfile')).toBeUndefined()
    expect(findButton(wrapper, 'admin.rollbackProfile')).toBeTruthy()
  })

  it('归档需要二次确认，确认后 POST 归档接口并刷新列表', async () => {
    const confirmFn = vi.fn((_message?: string) => true)
    vi.stubGlobal('confirm', confirmFn)
    const wrapper = await mountPage([{ ...sourceProfile }])

    await findButton(wrapper, 'admin.archiveProfile')!.trigger('click')
    await flushPromises()

    expect(labelsOf('admin.archiveProfileConfirm')).toContain(confirmFn.mock.calls[0]?.[0])

    const archiveCall = calls.find(call => call.url.includes('/archive'))
    expect(archiveCall?.method).toBe('POST')
    expect(archiveCall?.url).toContain('/admin/search-profiles/sp-1/archive')
    expect(archiveCall?.body).toBeUndefined()

    // 归档成功后刷新列表：初次加载 + 归档后各一次
    expect(listCallCount()).toBe(2)
  })

  it('取消二次确认时不发起归档请求', async () => {
    vi.stubGlobal('confirm', vi.fn((_message?: string) => false))
    const wrapper = await mountPage([{ ...sourceProfile }])

    await findButton(wrapper, 'admin.archiveProfile')!.trigger('click')
    await flushPromises()

    expect(calls.some(call => call.url.includes('/archive'))).toBe(false)
  })

  it('归档 409 时展示后端返回的错误消息', async () => {
    vi.stubGlobal('confirm', vi.fn((_message?: string) => true))
    const wrapper = await mountPage([{ ...sourceProfile }], {
      archiveError: {
        response: { status: 409, _data: { error: '此搜索配置处于生效状态，不能归档' } },
        message: 'FetchError'
      }
    })

    await findButton(wrapper, 'admin.archiveProfile')!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('此搜索配置处于生效状态，不能归档')
  })

  it('归档 409 且后端未给出消息时展示本地化冲突提示', async () => {
    vi.stubGlobal('confirm', vi.fn((_message?: string) => true))
    const wrapper = await mountPage([{ ...sourceProfile }], {
      archiveError: { response: { status: 409, _data: {} }, message: '' }
    })

    await findButton(wrapper, 'admin.archiveProfile')!.trigger('click')
    await flushPromises()

    const text = wrapper.text()
    expect(labelsOf('admin.archiveActiveConflict').some(label => text.includes(label))).toBe(true)
  })

  it('新建版本弹窗渲染并预填全部 25 个字段，预填值来自被选中 profile 而非默认值', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }])

    await findButton(wrapper, 'admin.newProfileVersion')!.trigger('click')
    await flushPromises()

    // 单行输入：字段全部来自被选中 profile 的现有值
    expect(modalInput('workspace.name').value).toBe('profile-custom-name')
    expect(modalInput('admin.embeddingProvider').value).toBe('profile-embedding-provider')
    expect(modalInput('admin.embeddingModel').value).toBe('profile-model-x')
    expect(modalInput('admin.dimensions').value).toBe('768')
    expect(modalInput('admin.chunkTargetSize').value).toBe('999')
    expect(modalInput('admin.chunkOverlap').value).toBe('11')
    expect(modalInput('admin.titleBoost').value).toBe('9')
    expect(modalInput('admin.headingBoost').value).toBe('8')
    expect(modalInput('admin.pathBoost').value).toBe('0.7')
    expect(modalInput('admin.tagsBoost').value).toBe('0.6')
    expect(modalInput('admin.bodyBoost').value).toBe('7')
    expect(modalInput('admin.analyzer').value).toBe('profile-analyzer')
    expect(modalInput('admin.lexicalTopK').value).toBe('11')
    expect(modalInput('admin.vectorTopK').value).toBe('22')
    expect(modalInput('admin.rrfK').value).toBe('33')
    expect(modalInput('admin.rerankerProvider').value).toBe('profile-reranker-provider')
    expect(modalInput('admin.rerankerModel').value).toBe('profile-reranker')
    expect(modalInput('admin.rerankerCandidateCount').value).toBe('77')
    expect(modalInput('admin.rerankerFinalCount').value).toBe('17')
    expect(modalInput('admin.maxChunksPerDocument').value).toBe('4')
    expect(modalInput('admin.maxP95LatencyMs').value).toBe('1234')
    expect(modalInput('admin.maxRerankerCostPerQuery').value).toBe('0.42')

    // 多行文本：对检索质量影响最大的 embedding instruction（旧实现只能继承）
    expect(modalTextarea('admin.embeddingQueryInstruction').value).toBe('query-instruction')
    expect(modalTextarea('admin.embeddingDocumentInstruction').value).toBe('document-instruction')

    // 布尔开关：源 profile 为 false，而 defaultForm 默认值为 true
    expect(modalSwitch('admin.mergeAdjacentChunks').getAttribute('aria-checked')).toBe('false')

    // 明确排除"使用 defaultForm 硬编码默认值"的可能
    expect(modalInput('admin.embeddingModel').value).not.toBe(DEFAULT_FORM_EMBEDDING_MODEL)
    expect(modalInput('admin.rerankerFinalCount').value).not.toBe('20')
    expect(modalInput('admin.maxP95LatencyMs').value).not.toBe('500')
    expect(modalInput('admin.analyzer').value).not.toBe('standard')
  })

  it('提交发送全部 25 个字段（整份覆盖），成功后关闭弹窗、刷新列表并提示新版本为 draft', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }])

    await findButton(wrapper, 'admin.newProfileVersion')!.trigger('click')
    await flushPromises()

    typeInto(modalInput('admin.embeddingModel'), 'edited-model')
    await submitModalForm()

    const versionCall = calls.find(call => call.url.includes('/versions'))
    expect(versionCall?.method).toBe('POST')
    expect(versionCall?.url).toContain('/admin/search-profiles/sp-1/versions')

    const body = versionCall?.body ?? {}
    // 请求体恰好是全部 25 个可调参数：不再依赖服务端对未提供字段的隐式继承
    expect(Object.keys(body).sort()).toEqual([...PROFILE_FIELDS].sort())
    expect(body.name).toBe('profile-custom-name')
    expect(body.embedding_model).toBe('edited-model')
    expect(body.embedding_dimensions).toBe(768)
    expect(body.rrf_k).toBe(33)

    // 新暴露的字段确实被发送，而不只是渲染了输入框
    expect(body.embedding_query_instruction).toBe('query-instruction')
    expect(body.embedding_document_instruction).toBe('document-instruction')
    expect(body.tags_boost).toBe(0.6)
    expect(body.reranker_model).toBe('profile-reranker')
    expect(body.reranker_final_count).toBe(17)
    expect(body.max_p95_latency_ms).toBe(1234)
    expect(body.max_reranker_cost_per_query).toBe(0.42)
    expect(body.merge_adjacent_chunks).toBe(false)
    expect(body.analyzer).toBe('profile-analyzer')

    // 服务端托管的只读字段绝不能出现在请求体（后端严格解码会直接 400）
    for (const forbidden of ['version', 'status', 'es_index_name', 'created_by', 'created_at', 'activated_at']) {
      expect(body).not.toHaveProperty(forbidden)
    }

    await waitFor(() => modalContent() === null, '提交成功后弹窗未关闭')

    // 创建成功后刷新列表：初次加载 + 创建后各一次
    expect(listCallCount()).toBe(2)

    // 提示用户新版本是 draft，需要激活才生效
    const text = wrapper.text()
    expect(labelsOf('admin.profileVersionCreated').some(label => text.includes(label))).toBe(true)
  })

  it('新建版本可编辑新暴露的参数，编辑后的值随请求体发送', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }])

    await findButton(wrapper, 'admin.newProfileVersion')!.trigger('click')
    await flushPromises()

    typeInto(modalTextarea('admin.embeddingQueryInstruction'), 'edited-query-instruction')
    typeInto(modalInput('admin.maxP95LatencyMs'), '4321')
    typeInto(modalInput('admin.rerankerFinalCount'), '3')
    // 开关从源 profile 的 false 切换为 true
    modalSwitch('admin.mergeAdjacentChunks').click()
    await flushPromises()

    expect(modalSwitch('admin.mergeAdjacentChunks').getAttribute('aria-checked')).toBe('true')

    await submitModalForm()

    const body = calls.find(call => call.url.includes('/versions'))?.body ?? {}
    expect(body.embedding_query_instruction).toBe('edited-query-instruction')
    expect(body.max_p95_latency_ms).toBe(4321)
    expect(body.reranker_final_count).toBe(3)
    expect(body.merge_adjacent_chunks).toBe(true)
  })

  it('创建弹窗渲染全部 25 个字段，并以 defaultForm 默认值提交完整请求体', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }])

    await findButton(wrapper, 'admin.newProfile')!.trigger('click')
    await flushPromises()

    // 创建弹窗仍使用 defaultForm 默认值（不被列表中的 profile 影响）
    expect(modalInput('admin.embeddingProvider').value).toBe('openai-compatible')
    expect(modalInput('admin.embeddingModel').value).toBe(DEFAULT_FORM_EMBEDDING_MODEL)
    expect(modalInput('admin.rerankerModel').value).toBe('qwen3-reranker')
    expect(modalInput('admin.analyzer').value).toBe('standard')
    expect(modalInput('admin.maxP95LatencyMs').value).toBe('500')
    expect(modalInput('admin.maxRerankerCostPerQuery').value).toBe('0.01')
    expect(modalInput('admin.tagsBoost').value).toBe('1')
    expect(modalInput('admin.pathBoost').value).toBe('1')
    expect(modalTextarea('admin.embeddingQueryInstruction').value).toBe('')
    expect(modalSwitch('admin.mergeAdjacentChunks').getAttribute('aria-checked')).toBe('true')

    // 新暴露的字段可编辑，并随创建请求一起提交
    typeInto(modalInput('workspace.name'), 'new-profile')
    typeInto(modalInput('admin.rerankerModel'), 'edited-reranker')
    typeInto(modalInput('admin.tagsBoost'), '3.5')
    await submitModalForm()

    const createCall = calls.find(call => call.method === 'POST' && call.url.endsWith('/admin/search-profiles'))
    expect(createCall).toBeTruthy()
    const body = createCall?.body ?? {}
    expect(Object.keys(body).sort()).toEqual([...PROFILE_FIELDS].sort())
    expect(body.name).toBe('new-profile')
    expect(body.reranker_model).toBe('edited-reranker')
    expect(body.tags_boost).toBe(3.5)
    expect(body.embedding_dimensions).toBe(1024)
    expect(body.analyzer).toBe('standard')

    await waitFor(() => modalContent() === null, '创建成功后弹窗未关闭')
    expect(listCallCount()).toBe(2)
    expect(findButton(wrapper, 'admin.newProfileVersion')).toBeTruthy()
  })

  it('新建版本失败时在弹窗内展示后端错误并保持弹窗打开', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }], {
      versionError: {
        response: { status: 400, _data: { error: 'embedding_dimensions 必须大于 0' } },
        message: 'FetchError'
      }
    })

    await findButton(wrapper, 'admin.newProfileVersion')!.trigger('click')
    await flushPromises()

    typeInto(modalInput('admin.dimensions'), '0')
    await submitModalForm()

    expect(modalContent()?.textContent ?? '').toContain('embedding_dimensions 必须大于 0')
    // 失败时弹窗保持打开，用户可修正后重试
    expect(modalContent()).not.toBeNull()
    // 失败不刷新列表（只有挂载时的那一次）
    expect(listCallCount()).toBe(1)
  })

  it('新建版本失败且后端未给出消息时展示本地化失败提示', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }], {
      versionError: { response: { status: 500, _data: {} }, message: '' }
    })

    await findButton(wrapper, 'admin.newProfileVersion')!.trigger('click')
    await flushPromises()
    await submitModalForm()

    const text = modalContent()?.textContent ?? ''
    expect(labelsOf('admin.createProfileVersionFailed').some(label => text.includes(label))).toBe(true)
  })

  it('名称为空时不做请求，直接在弹窗内提示必填', async () => {
    const wrapper = await mountPage([{ ...sourceProfile }])

    await findButton(wrapper, 'admin.newProfileVersion')!.trigger('click')
    await flushPromises()

    typeInto(modalInput('workspace.name'), '')
    await submitModalForm()

    expect(calls.some(call => call.url.includes('/versions'))).toBe(false)
    const text = modalContent()?.textContent ?? ''
    expect(labelsOf('admin.profileNameRequired').some(label => text.includes(label))).toBe(true)
  })
})

