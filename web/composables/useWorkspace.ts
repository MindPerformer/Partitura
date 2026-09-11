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
// - per-id 结果缓存 + in-flight 请求去重：同一 workspace id 的
//   get + getMyMembership 在一次页面装配中只打一次；
//   切换 workspace 时丢弃离开 id 的缓存项，logout/401（verifiedUser 清空）
//   时全量清空，避免角色越权残留。

import type { Workspace, MyMembershipResponse } from '~/types/api'
import { hasMinRole, WORKSPACE_ROLES } from '~/types/api'
import { verifiedUser } from '~/composables/useAuth'

/** 单个 workspace 上下文的缓存结果 */
interface WorkspaceContextResult {
  workspace: Workspace
  role: string | null
}

// per-id 结果缓存与 in-flight Promise（模块级，跨组件实例共享）。
// 动机：useWorkspaceContext 会被 WorkspaceLayout 与多个子页面/组件重复调用，
// 没有缓存会导致同一 workspace 反复打 get+membership；in-flight Map 合并并发调用。
const contextCache = new Map<string, WorkspaceContextResult>()
const inflightRequests = new Map<string, Promise<WorkspaceContextResult>>()

// 认证态失效（logout / 401 → verifiedUser 清空）时全量清空缓存，
// 防止前一用户的 workspace 角色信息残留到下一登录会话。
watch(verifiedUser, (user) => {
  if (!user) {
    contextCache.clear()
  }
})

/** 主动失效某个 workspace 的缓存（例如成员/设置变更后强制刷新） */
export function invalidateWorkspaceContext(id: string): void {
  contextCache.delete(id)
}

/** 清空全部 workspace 上下文缓存（logout/会话切换） */
export function clearWorkspaceContextCache(): void {
  contextCache.clear()
}

/**
 * 解析 workspace 上下文：缓存命中直接返回；否则合并并发请求。
 * 失败结果不写入缓存（下次进入可重试），in-flight 项在 finally 中移除。
 */
function resolveWorkspaceContext(id: string): Promise<WorkspaceContextResult> {
  const cached = contextCache.get(id)
  if (cached) {
    return Promise.resolve(cached)
  }
  const inflight = inflightRequests.get(id)
  if (inflight) {
    return inflight
  }
  const request = (async () => {
    const api = useWorkspaceApi()
    const workspace = await api.get(id)
    // 直接使用 GET /api/workspaces/{id}/me/membership 获取当前用户角色，
    // 避免反查 members 第一页推断本人角色（分页缺失会导致错误判断）。
    const membership: MyMembershipResponse = await api.getMyMembership(id)
    const result: WorkspaceContextResult = { workspace, role: membership.role }
    contextCache.set(id, result)
    return result
  })()
  inflightRequests.set(id, request)
  // 用 then 的双分支清理而不是 .finally()：finally 返回的派生 Promise 在
  // 原始请求 reject 时会产生 unhandled rejection；then 的第二个回调已处理它。
  request.then(
    () => { if (inflightRequests.get(id) === request) inflightRequests.delete(id) },
    () => { if (inflightRequests.get(id) === request) inflightRequests.delete(id) }
  )
  return request
}

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
      const result = await resolveWorkspaceContext(id)
      // 并发切换保护：请求完成后 id 已变化时丢弃过期结果，不回写。
      if (idRef.value !== id) return
      workspace.value = result.workspace
      currentMemberRole.value = result.role
    } catch (err) {
      if (idRef.value !== id) return
      const apiErr = err as { error?: string; status?: number }
      if (apiErr.status === 404 || apiErr.status === 403) {
        error.value = apiErr.error || t('errors.workspaceNotFoundOrInaccessible')
      } else {
        error.value = apiErr.error || t('errors.loadWorkspaceFailed')
      }
      workspace.value = null
      currentMemberRole.value = null
    } finally {
      if (idRef.value === id) {
        loading.value = false
      }
    }
  }

  // workspace ID 变化时重新加载。
  // 离开的 id 对应缓存被丢弃：角色属于"当前用户在该 workspace 的授权快照"，
  // 切走后不保留可减少越权残留面；再次进入会重新拉取最新授权。
  watch(idRef, (newId, oldId) => {
    if (oldId && oldId !== newId) {
      contextCache.delete(oldId)
    }
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
