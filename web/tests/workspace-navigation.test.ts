// tests/workspace-navigation.test.ts — workspace 导航相关 API 契约测试
//
// 引入动机：计划要求所有 workspace 页面使用统一左侧文档树 + 顶部主页入口。
// 文档分页加载的真实行为已由 tests/sidebar-pagination.test.ts 覆盖
// （挂载真实 WorkspaceLayout 驱动 loadDocuments/loadMoreDocuments）。
// 本文件保留真正属于"API 契约"的断言：workspace stats 聚合接口可用。
//
// 说明：此前此文件手抄了一份 loadDocuments 分页循环做断言，与真实实现脱钩
// （违反 TDD「测试驱动真实代码」），已在 Phase 4 移除——分页行为统一由
// sidebar-pagination.test.ts 覆盖。

import { describe, it, expect, beforeEach } from 'vitest'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('Workspace Navigation — stats API 契约', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('workspace stats API 返回聚合统计并被正确调用', async () => {
    ctrl.setResponse({
      stats: {
        total_documents: 10,
        active_documents: 8,
        draft_documents: 1,
        archived_documents: 1,
        member_count: 3,
        recent_revisions: []
      }
    })

    const { useWorkspaceApi } = await import('~/composables/useApi')
    const api = useWorkspaceApi()

    const result = await api.stats('ws-1')

    expect(result.stats.total_documents).toBe(10)
    expect(result.stats.active_documents).toBe(8)
    expect(result.stats.member_count).toBe(3)

    // 验证请求确实打到了 stats 端点
    const call = ctrl.mockFn.mock.calls[0]
    expect(call).toBeTruthy()
    expect(String(call![0])).toContain('/workspaces/ws-1/stats')
  })
})
