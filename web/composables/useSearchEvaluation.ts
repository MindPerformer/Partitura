// composables/useSearchEvaluation.ts — Search Evaluation 列表加载逻辑
//
// 引入动机：评测页面同时加载 datasets 和 profiles；任一 API 空结果若为 null，
// 直接传给 Nuxt UI 选择器会触发 null.length 白屏。这里在 API boundary 严格校验响应。

import type {
  ApiError,
  EvaluationDataset,
  ListDatasetsResponse,
  ListProfilesResponse,
  SearchProfile
} from '~/types/api'

function isListDatasetsResponse(value: unknown): value is ListDatasetsResponse {
  if (value === null || typeof value !== 'object') return false
  const response = value as Record<string, unknown>
  return Array.isArray(response.datasets) && typeof response.total === 'number' && Number.isFinite(response.total)
}

function isListProfilesResponse(value: unknown): value is ListProfilesResponse {
  if (value === null || typeof value !== 'object') return false
  const response = value as Record<string, unknown>
  return Array.isArray(response.profiles) && typeof response.total === 'number' && Number.isFinite(response.total)
}

export function useSearchEvaluation() {
  let t: (key: string) => string
  try {
    t = useI18n().t
  } catch {
    const fallback: Record<string, string> = {
      'admin.invalidEvaluationResponse': '服务器返回的评测列表格式不正确',
      'admin.loadEvaluationFailed': '加载评测数据失败'
    }
    t = (key: string) => fallback[key] || key
  }

  const datasets = ref<EvaluationDataset[]>([])
  const profiles = ref<SearchProfile[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function load(params?: { limit?: number; offset?: number }) {
    loading.value = true
    error.value = null
    try {
      const api = useSearchAdminApi()
      const [datasetsResponse, profilesResponse] = await Promise.all([
        api.listDatasets(params),
        api.listProfiles({ limit: 100 })
      ])

      if (!isListDatasetsResponse(datasetsResponse)) {
        console.error('[useSearchEvaluation] 数据集列表响应非法', datasetsResponse)
        datasets.value = []
        profiles.value = []
        error.value = t('admin.invalidEvaluationResponse')
        return
      }
      if (!isListProfilesResponse(profilesResponse)) {
        console.error('[useSearchEvaluation] profile 列表响应非法', profilesResponse)
        datasets.value = datasetsResponse.datasets
        profiles.value = []
        error.value = t('admin.invalidEvaluationResponse')
        return
      }

      datasets.value = datasetsResponse.datasets
      profiles.value = profilesResponse.profiles
    } catch (err) {
      const apiErr = err as ApiError
      error.value = apiErr.error || t('admin.loadEvaluationFailed')
    } finally {
      loading.value = false
    }
  }

  return { datasets, profiles, loading, error, load }
}
