// tests/use-workspace-context.test.ts — useWorkspaceContext 缓存/去重/登出清理测试
//
// 引入动机：useWorkspaceContext 维护 per-id 结果缓存 + in-flight 请求去重，
// 并在 verifiedUser 清空（logout/401）时全量清空缓存防止角色越权残留。
// 此测试驱动真实 composable 验证：
// 1. 同一 workspace id 并发挂载只发起一次 get + getMyMembership（in-flight 去重）
// 2. 缓存命中后再次进入不重复请求
// 3. logout（verifiedUser 清空）后缓存被清，重新进入会再次请求

import { describe, it, expect, beforeEach } from 'vitest'
import { ref, nextTick } from 'vue'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

const WORKSPACE = {
  id: 'ws-dedup', name: 'ws', display_name: 'WS', description: '', status: 'active',
  revision_retention_days: 0, revision_max_count: 0, max_document_size_bytes: 0, created_by: 'u1'
}

function wsImpl(wsId: string) {
  return async (url: string, rawOpts: unknown) => {
    const o = rawOpts as { method?: string }
    const method = o.method ?? 'GET'
    if (url.includes('/me/membership')) {
      return { _data: { workspace_id: wsId, role: 'editor' }, status: 200 }
    }
    if (method === 'GET' && url.includes(`/workspaces/${wsId}`)) {
      return { _data: { ...WORKSPACE, id: wsId }, status: 200 }
    }
    throw new Error(`测试未覆盖的请求：${method} ${url}`)
  }
}

/** 统计针对某 workspace id 的 GET /workspaces/{id}（不含 /me/membership 子路径）请求数。 */
function getCallCount(wsId: string): number {
  return ctrl.mockFn.mock.calls.filter(c => {
    const url = String(c[0])
    return url.includes(`/workspaces/${wsId}`) && !url.includes('/me/membership')
  }).length
}

/** 统计 membership 请求数。 */
function membershipCallCount(wsId: string): number {
  return ctrl.mockFn.mock.calls.filter(c => String(c[0]).includes(`/workspaces/${wsId}/me/membership`)).length
}

describe('useWorkspaceContext — in-flight 去重与登出清缓存', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('同一 workspace 并发调用只发起一次 get + membership（in-flight 去重）', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    useAuth().setAuth({
      user: { id: 'u1', username: 't', system_role: 'user' },
      csrf_token: 'token', expires_at: '2025-01-01'
    })

    const wsId = 'ws-dedup-1'
    ctrl.setImpl(wsImpl(wsId))

    const { useWorkspaceContext } = await import('~/composables/useWorkspace')
    const id = ref(wsId)
    // 并发创建两个上下文（同一次页面装配场景）
    const ctxA = useWorkspaceContext(id)
    const ctxB = useWorkspaceContext(id)

    await new Promise(r => setTimeout(r, 200))
    await nextTick()

    // 两个上下文都拿到数据
    expect(ctxA.workspace.value?.id).toBe(wsId)
    expect(ctxB.workspace.value?.id).toBe(wsId)
    // 但只发起一次 workspace get + 一次 membership（去重）
    expect(getCallCount(wsId)).toBe(1)
    expect(membershipCallCount(wsId)).toBe(1)
  })

  it('缓存命中后再次进入同一 workspace 不重复请求', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    useAuth().setAuth({
      user: { id: 'u1', username: 't', system_role: 'user' },
      csrf_token: 'token', expires_at: '2025-01-01'
    })

    const wsId = 'ws-dedup-2'
    ctrl.setImpl(wsImpl(wsId))

    const { useWorkspaceContext } = await import('~/composables/useWorkspace')
    const id = ref(wsId)
    const first = useWorkspaceContext(id)
    await new Promise(r => setTimeout(r, 200))
    expect(first.workspace.value?.id).toBe(wsId)
    expect(getCallCount(wsId)).toBe(1)

    // 第二次创建（另一组件再次进入同一 workspace）：命中缓存，不再请求
    const second = useWorkspaceContext(id)
    await new Promise(r => setTimeout(r, 200))
    await nextTick()

    expect(second.workspace.value?.id).toBe(wsId)
    expect(getCallCount(wsId)).toBe(1)
    expect(membershipCallCount(wsId)).toBe(1)
  })

  it('logout 清空缓存：重新进入同一 workspace 会再次请求', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: 'u1', username: 't', system_role: 'user' },
      csrf_token: 'token', expires_at: '2025-01-01'
    })

    const wsId = 'ws-dedup-3'
    ctrl.setImpl(wsImpl(wsId))

    const { useWorkspaceContext } = await import('~/composables/useWorkspace')
    const id = ref(wsId)
    const first = useWorkspaceContext(id)
    await new Promise(r => setTimeout(r, 200))
    expect(first.workspace.value?.id).toBe(wsId)
    expect(getCallCount(wsId)).toBe(1)

    // 登出：verifiedUser 清空触发模块级 watch，全量清空 contextCache
    auth.clearAuth()
    await nextTick()

    // 重新进入（模拟重新登录后再次访问同一 workspace）：缓存已清，重新请求
    const second = useWorkspaceContext(id)
    await new Promise(r => setTimeout(r, 200))
    await nextTick()

    expect(second.workspace.value?.id).toBe(wsId)
    expect(getCallCount(wsId)).toBe(2)
    expect(membershipCallCount(wsId)).toBe(2)
  })
})
