// tests/search-profiles.test.ts — Search Profiles 响应契约与错误状态测试
//
// 引入动机：Phase6 WP1 修复 search-profiles 运行时崩溃。
// 覆盖 null、空数组、snake_case 正常数据、非法响应等边界，
// 确保 API boundary 验证不通过时显示本地化错误、profiles 为 []、不抛异常。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

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
