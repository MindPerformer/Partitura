// tests/workspace-switch.test.ts — Workspace 切换测试
//
// 引入动机：design/04-WEB-API.md §Frontend 要求切换 workspace 后清理文档/search 本地状态。
// 测试 useWorkspaceContext 在 workspace ID 变化时重新加载。

import { describe, it, expect, beforeEach } from 'vitest'
import { ref } from 'vue'
import { createFetchMock } from './setup'

const ctrl = createFetchMock()

describe('Workspace Switch', () => {
  beforeEach(() => {
    ctrl.reset()
  })

  it('workspace context 加载 workspace 和成员（owner 角色）', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: 'user-1', username: 'test', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    let callCount = 0
    ctrl.setImpl(async () => {
      callCount++
      if (callCount === 1) {
        return {
          _data: {
            id: 'ws-1', name: 'test', display_name: 'Test', description: '',
            status: 'active', revision_retention_days: 7, revision_max_count: 30,
            max_document_size_bytes: 2097152, created_by: 'user-1'
          }, status: 200
        }
      } else {
        // GET /api/workspaces/{id}/me/membership 返回当前用户在此 workspace 的角色
        return {
          _data: {
            workspace_id: 'ws-1', role: 'owner'
          }, status: 200
        }
      }
    })

    const { useWorkspaceContext } = await import('~/composables/useWorkspace')
    const wsId = ref('ws-1')
    const ctx = useWorkspaceContext(wsId)

    await new Promise(resolve => setTimeout(resolve, 200))

    expect(ctx.workspace.value?.id).toBe('ws-1')
    expect(ctx.currentMemberRole.value).toBe('owner')
    expect(ctx.canRead.value).toBe(true)
    expect(ctx.canEdit.value).toBe(true)
    expect(ctx.canManageMembers.value).toBe(true)
    expect(ctx.isOwner.value).toBe(true)
  })

  it('workspace 404 设置 error 状态', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: 'user-1', username: 'test', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    ctrl.setError({
      response: { status: 404, _data: { error: 'workspace not found' } },
      message: 'FetchError'
    })

    const { useWorkspaceContext } = await import('~/composables/useWorkspace')
    const wsId = ref('ws-404')
    const ctx = useWorkspaceContext(wsId)

    await new Promise(resolve => setTimeout(resolve, 200))

    expect(ctx.workspace.value).toBeNull()
    expect(ctx.error.value).toContain('workspace not found')
    expect(ctx.loading.value).toBe(false)
  })

  it('workspace 403 设置 error 状态', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: 'user-1', username: 'test', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    ctrl.setError({
      response: { status: 403, _data: { error: 'access denied' } },
      message: 'FetchError'
    })

    const { useWorkspaceContext } = await import('~/composables/useWorkspace')
    const wsId = ref('ws-403')
    const ctx = useWorkspaceContext(wsId)

    await new Promise(resolve => setTimeout(resolve, 200))

    expect(ctx.workspace.value).toBeNull()
    expect(ctx.error.value).toContain('access denied')
  })

  it('viewer 角色只有 canRead 权限', async () => {
    const { useAuth } = await import('~/composables/useAuth')
    const auth = useAuth()
    auth.setAuth({
      user: { id: 'user-2', username: 'viewer', system_role: 'user' },
      csrf_token: 'token',
      expires_at: '2025-01-01'
    } as never)

    let callCount = 0
    ctrl.setImpl(async () => {
      callCount++
      if (callCount === 1) {
        return {
          _data: {
            id: 'ws-1', name: 'test', display_name: 'Test', description: '',
            status: 'active', revision_retention_days: 7, revision_max_count: 30,
            max_document_size_bytes: 2097152, created_by: 'user-1'
          }, status: 200
        }
      } else {
        // GET /api/workspaces/{id}/me/membership 返回当前用户在此 workspace 的角色
        return {
          _data: {
            workspace_id: 'ws-1', role: 'viewer'
          }, status: 200
        }
      }
    })

    const { useWorkspaceContext } = await import('~/composables/useWorkspace')
    const wsId = ref('ws-viewer')
    const ctx = useWorkspaceContext(wsId)

    await new Promise(resolve => setTimeout(resolve, 200))

    expect(ctx.canRead.value).toBe(true)
    expect(ctx.canEdit.value).toBe(false)
    expect(ctx.canArchive.value).toBe(false)
    expect(ctx.canManageMembers.value).toBe(false)
    expect(ctx.isOwner.value).toBe(false)
  })
})
