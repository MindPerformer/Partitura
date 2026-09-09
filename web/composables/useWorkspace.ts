// composables/useWorkspace.ts — 当前 workspace 上下文管理
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 workspace 上下文根据 URL 加载。
// 切换 workspace 后清理文档/search 本地状态。
// 当前 workspace 不存在/无权限安全处理。
//
// 设计原则：
// - workspace 上下文基于 URL 路由参数 {id} 加载
// - 当前成员角色用于 RBAC 可见性控制
// - 切换 workspace 时清理本地状态

import type { Workspace, MyMembershipResponse } from '~/types/api'
import { hasMinRole, WORKSPACE_ROLES } from '~/types/api'

/**
 * useWorkspaceContext — 加载和管理当前 workspace 上下文
 *
 * @param workspaceId — workspace UUID（通常来自路由参数）
 * @returns 响应式 workspace、成员角色、权限判断函数
 */
export function useWorkspaceContext(workspaceId: Ref<string | undefined> | string) {
  const idRef = computed(() => unref(workspaceId))
  const workspace = ref<Workspace | null>(null)
  const currentMemberRole = ref<string | null>(null)
  const loading = ref(false)
  const error = ref<string | null>(null)

  // i18n：在 setup 上下文外（如单元测试直接调用）安全降级
  let t: (key: string, params?: Record<string, unknown>) => string
  try {
    const i18n = useI18n()
    t = i18n.t
  } catch {
    t = (key: string) => key
  }

  async function loadWorkspace() {
    const id = idRef.value
    if (!id) {
      workspace.value = null
      currentMemberRole.value = null
      error.value = null
      return
    }

    loading.value = true
    error.value = null

    try {
      const api = useWorkspaceApi()
      workspace.value = await api.get(id)

      // 直接使用 GET /api/workspaces/{id}/me/membership 获取当前用户角色，
      // 避免反查 members 第一页推断本人角色（分页缺失会导致错误判断）。
      const membership: MyMembershipResponse = await api.getMyMembership(id)
      currentMemberRole.value = membership.role
    } catch (err) {
      const apiErr = err as { error?: string; status?: number }
      if (apiErr.status === 404 || apiErr.status === 403) {
        error.value = apiErr.error || t('errors.workspaceNotFoundOrInaccessible')
      } else {
        error.value = apiErr.error || t('errors.loadWorkspaceFailed')
      }
      workspace.value = null
      currentMemberRole.value = null
    } finally {
      loading.value = false
    }
  }

  // workspace ID 变化时重新加载
  watch(idRef, () => {
    loadWorkspace()
  }, { immediate: true })

  // RBAC 权限判断
  const canRead = computed(() => hasMinRole(currentMemberRole.value ?? '', WORKSPACE_ROLES.VIEWER))
  const canEdit = computed(() => hasMinRole(currentMemberRole.value ?? '', WORKSPACE_ROLES.EDITOR))
  const canArchive = computed(() => hasMinRole(currentMemberRole.value ?? '', WORKSPACE_ROLES.ADMIN))
  const canManageMembers = computed(() => hasMinRole(currentMemberRole.value ?? '', WORKSPACE_ROLES.ADMIN))
  const canSettings = computed(() => hasMinRole(currentMemberRole.value ?? '', WORKSPACE_ROLES.ADMIN))
  const isOwner = computed(() => currentMemberRole.value === WORKSPACE_ROLES.OWNER)

  return {
    workspace,
    currentMemberRole,
    loading,
    error,
    canRead,
    canEdit,
    canArchive,
    canManageMembers,
    canSettings,
    isOwner,
    reload: loadWorkspace
  }
}
