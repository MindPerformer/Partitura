// composables/useSearchProfiles.ts — Search Profile 列表逻辑
//
// 引入动机：Phase6 WP1 修复 search-profiles 页面崩溃。
// 将 API 响应验证、加载状态和错误处理抽出为可测试的 composable，
// 在 API boundary 严格校验 profiles 是数组、total 是 number，
// 契约不符时设置本地化错误并停止渲染，不使用泛滥 optional chaining。

import type { ApiError, ListProfilesResponse, SearchProfile } from '~/types/api'

/**
 * 校验服务器返回的 profile 列表响应是否符合契约。
 *
 * 行为：
 *   - profiles 必须是数组
 *   - total 必须是有限数值
 * 任何不满足都视为契约破坏，阻止继续渲染。
 */
function isListProfilesResponse(value: unknown): value is ListProfilesResponse {
  if (value === null || typeof value !== 'object') return false
  const v = value as Record<string, unknown>
  return Array.isArray(v.profiles) && typeof v.total === 'number' && Number.isFinite(v.total)
}

/**
 * Search Profile 列表的受控状态管理。
 *
 * 引入动机：
 *   - 将 API boundary 验证与 UI 状态解耦，便于单元测试。
 *   - 空列表显式初始化为 []，防止 null.length 运行时异常。
 *
 * @returns profiles, total, loading, error, load
 */
export function useSearchProfiles() {
  // i18n：在 setup 上下文外（如单元测试直接调用）安全降级
  // 测试需要断言中文错误消息，因此提供最小 fallback 映射。
  let t: (key: string) => string
  try {
    t = useI18n().t
  } catch {
    const fallback: Record<string, string> = {
      'admin.invalidProfilesResponse': '服务器返回的配置列表格式不正确',
      'admin.loadProfilesFailed': '加载配置列表失败'
    }
    t = (key: string) => fallback[key] || key
  }

  const profiles = ref<SearchProfile[]>([])
  const total = ref(0)
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function load(params?: { limit?: number; offset?: number; status?: string }) {
    loading.value = true
    error.value = null

    try {
      const api = useSearchAdminApi()
      const res = await api.listProfiles(params)

      if (!isListProfilesResponse(res)) {
        // 契约违反：服务器返回了非法响应，不渲染部分数据，记录日志并显示翻译错误。
        console.error('[useSearchProfiles] 非法响应', res)
        profiles.value = []
        total.value = 0
        error.value = t('admin.invalidProfilesResponse')
        return
      }

      profiles.value = res.profiles
      total.value = res.total
    } catch (err) {
      const apiErr = err as ApiError
      error.value = apiErr.error || t('admin.loadProfilesFailed')
    } finally {
      loading.value = false
    }
  }

  return {
    profiles,
    total,
    loading,
    error,
    load
  }
}
