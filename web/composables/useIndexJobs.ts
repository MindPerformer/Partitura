// composables/useIndexJobs.ts — Index Jobs 列表逻辑
//
// 引入动机：管理端索引任务页（pages/admin/jobs.vue）白屏的根因是
// GET /api/admin/jobs 曾返回 Go 字段名（ID/AttemptCount/LastError/...），
// 页面读取 job.id 得到 undefined，在渲染期执行 job.id.substring(0, 8) 抛 TypeError，
// 整个列表渲染中断，页面空白且没有任何错误提示。
//
// 与 useSearchProfiles 保持同一模式：把 API 响应校验、加载状态和错误处理收敛到 composable，
// 在 API boundary 严格校验 jobs 是数组、total 是有限数值；契约不符时记录 console.error、
// 清空数据并设置本地化错误，停止渲染，绝不让脏数据进入模板。

import type { ApiError, Job, ListJobsResponse } from '~/types/api'

/**
 * 校验服务器返回的任务列表响应是否符合契约。
 *
 * 行为：
 *   - jobs 必须是数组
 *   - total 必须是有限数值
 * 任何不满足都视为契约破坏，阻止继续渲染。
 */
function isListJobsResponse(value: unknown): value is ListJobsResponse {
  if (value === null || typeof value !== 'object') return false
  const v = value as Record<string, unknown>
  return Array.isArray(v.jobs) && typeof v.total === 'number' && Number.isFinite(v.total)
}

/**
 * Index Jobs 列表的受控状态管理。
 *
 * 引入动机：
 *   - 将 API boundary 验证与 UI 状态解耦，便于单元测试。
 *   - 空列表显式初始化为 []，防止 null.length 运行时异常。
 *
 * 返回的 error 是可直接赋值的 ref：页面在重试/触发重建失败时需要写入错误消息。
 *
 * @returns jobs, total, loading, error, load
 */
export function useIndexJobs() {
  // i18n：在 setup 上下文外（如单元测试直接调用）安全降级。
  // 动机：useI18n() 只能在 setup 上下文调用；单元测试在裸环境调用本 composable，
  //   且断言固定的中文错误文案（如「服务器返回的任务列表格式不正确」），
  //   因此提供与 locales/zh.json 对齐的最小 fallback 映射，缺键时返回 key 本身。
  // 为何不返回 key 由组件翻译：错误在 composable 内立即写入 error ref 供模板直接渲染，
  //   组件无法可靠区分「已是文案」与「待翻译 key」，故在边界处就地解析为可读文本。
  let t: (key: string) => string
  try {
    t = useI18n().t
  } catch {
    const fallback: Record<string, string> = {
      'admin.invalidJobsResponse': '服务器返回的任务列表格式不正确',
      'admin.loadJobsFailed': '加载任务列表失败'
    }
    t = (key: string) => fallback[key] || key
  }

  const jobs = ref<Job[]>([])
  const total = ref(0)
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function load(params?: { limit?: number; offset?: number; status?: string }) {
    loading.value = true
    error.value = null

    try {
      const api = useSearchAdminApi()
      const res = await api.listJobs(params)

      if (!isListJobsResponse(res)) {
        // 契约违反：服务器返回了非法响应，不渲染部分数据，记录日志并显示翻译错误。
        console.error('[useIndexJobs] 非法响应', res)
        jobs.value = []
        total.value = 0
        error.value = t('admin.invalidJobsResponse')
        return
      }

      jobs.value = res.jobs
      total.value = res.total
    } catch (err) {
      const apiErr = err as ApiError
      error.value = apiErr.error || t('admin.loadJobsFailed')
    } finally {
      loading.value = false
    }
  }

  return {
    jobs,
    total,
    loading,
    error,
    load
  }
}
