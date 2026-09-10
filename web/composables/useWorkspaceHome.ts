// composables/useWorkspaceHome.ts — 工作区主页数据加载
//
// 引入动机：工作区主页需要同时加载 PROJECT.md 和 workspace 统计信息。
// 将加载状态从页面中抽出，避免 immediate watch 在 ref 初始化前执行导致 TDZ 错误，
// 同时让主页数据逻辑可以独立进行行为级测试。

import type { ApiError, Document, WorkspaceStats } from '~/types/api'

/**
 * 工作区主页的异步数据和加载状态。
 *
 * @param workspaceId — workspace UUID，支持响应式引用或普通字符串
 * @returns PROJECT.md、统计数据、加载状态、错误状态和重新加载函数
 */
export function useWorkspaceHome(workspaceId: Ref<string | undefined> | string) {
  const idRef = computed(() => unref(workspaceId))

  let t: (key: string) => string
  try {
    t = useI18n().t
  } catch {
    const fallback: Record<string, string> = {
      'workspace.loadStatsFailed': '加载统计失败'
    }
    t = (key: string) => fallback[key] || key
  }

  // 所有响应式状态必须在 immediate watch 之前完成初始化。
  // Vue 的 immediate watch 会在 setup 中同步执行回调；若 watch 提前声明，
  // 回调访问这些 const ref 会触发 Cannot access ... before initialization。
  const projectDoc = ref<Document | null>(null)
  const projectLoading = ref(false)
  const stats = ref<WorkspaceStats | null>(null)
  const statsLoading = ref(false)
  const statsError = ref<string | null>(null)

  async function loadProjectDoc() {
    const id = idRef.value
    if (!id) {
      projectDoc.value = null
      return
    }

    projectLoading.value = true
    try {
      const api = useDocumentApi()
      projectDoc.value = await api.read(id, 'PROJECT.md')
    } catch (err) {
      const apiErr = err as ApiError
      if (apiErr.status !== 404 && import.meta.dev) {
        console.error('[useWorkspaceHome] 加载 PROJECT.md 失败', err)
      }
      projectDoc.value = null
    } finally {
      projectLoading.value = false
    }
  }

  async function loadStats() {
    const id = idRef.value
    if (!id) {
      stats.value = null
      statsError.value = null
      return
    }

    statsLoading.value = true
    statsError.value = null
    try {
      const api = useWorkspaceApi()
      const res = await api.stats(id)
      stats.value = res.stats
    } catch (err) {
      const apiErr = err as ApiError
      statsError.value = apiErr.error || t('workspace.loadStatsFailed')
      stats.value = null
    } finally {
      statsLoading.value = false
    }
  }

  // 必须放在所有 ref 和加载函数之后，确保 immediate 回调不会访问 TDZ 状态。
  watch(idRef, () => {
    loadProjectDoc()
    loadStats()
  }, { immediate: true })

  async function reload() {
    await Promise.all([loadProjectDoc(), loadStats()])
  }

  return {
    projectDoc,
    projectLoading,
    stats,
    statsLoading,
    statsError,
    reload
  }
}
