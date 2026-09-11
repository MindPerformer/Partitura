// composables/useSearchEvaluation.ts — Search Evaluation 列表加载逻辑
//
// 引入动机：评测页面同时加载 datasets 和 profiles；任一 API 空结果若为 null，
// 直接传给 Nuxt UI 选择器会触发 null.length 白屏。这里在 API boundary 严格校验响应。

import type {
  ApiError,
  EvaluationDataset,
  EvaluationItem,
  EvaluationResult,
  ListDatasetsResponse,
  ListEvaluationResultsResponse,
  ListItemsResponse,
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

function isListEvaluationResultsResponse(value: unknown): value is ListEvaluationResultsResponse {
  if (value === null || typeof value !== 'object') return false
  const response = value as Record<string, unknown>
  return Array.isArray(response.results) && typeof response.total === 'number' && Number.isFinite(response.total)
}

function isListItemsResponse(value: unknown): value is ListItemsResponse {
  if (value === null || typeof value !== 'object') return false
  const response = value as Record<string, unknown>
  return Array.isArray(response.items) && typeof response.total === 'number' && Number.isFinite(response.total)
}

export function useSearchEvaluation() {
  // i18n：在 setup 上下文外（如单元测试直接调用）安全降级。
  // 动机：单元测试在裸环境调用本 composable 并断言固定中文错误文案，
  //   因此提供与 locales/zh.json 对齐的最小 fallback 映射，缺键时返回 key 本身。
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
  const datasetsTotal = ref(0)
  const profiles = ref<SearchProfile[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function load(params?: { limit?: number; offset?: number; status?: string }) {
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
        datasetsTotal.value = 0
        profiles.value = []
        error.value = t('admin.invalidEvaluationResponse')
        return
      }
      if (!isListProfilesResponse(profilesResponse)) {
        console.error('[useSearchEvaluation] profile 列表响应非法', profilesResponse)
        datasets.value = datasetsResponse.datasets
        datasetsTotal.value = datasetsResponse.total
        profiles.value = []
        error.value = t('admin.invalidEvaluationResponse')
        return
      }

      datasets.value = datasetsResponse.datasets
      datasetsTotal.value = datasetsResponse.total
      profiles.value = profilesResponse.profiles
    } catch (err) {
      const apiErr = err as ApiError
      error.value = apiErr.error || t('admin.loadEvaluationFailed')
    } finally {
      loading.value = false
    }
  }

  /**
   * 加载指定 dataset 的评测结果（分页）。
   *
   * 引入动机：搜索结果评测历史需要展示；后端提供 GET /admin/evaluation/results
   * （需 dataset_id 查询参数）。响应契约在 API boundary 校验，非法时返回空结果并报错。
   */
  async function listResults(datasetId: string, params?: { limit?: number; offset?: number }) {
    try {
      const api = useSearchAdminApi()
      const res = await api.listEvaluationResults(datasetId, params)
      if (!isListEvaluationResultsResponse(res)) {
        console.error('[useSearchEvaluation] 评测结果列表响应非法', res)
        return { results: [] as EvaluationResult[], total: 0 }
      }
      return { results: res.results, total: res.total }
    } catch (err) {
      const apiErr = err as ApiError
      // 结果加载失败不覆盖主列表错误，向上抛出由调用方决定展示位置
      throw apiErr
    }
  }

  /**
   * 加载指定 dataset 的评测条目（分页）。
   *
   * 引入动机：后端提供 GET /admin/evaluation/datasets/{id}/items。
   * 响应契约在 API boundary 校验，非法时返回空结果并报错。
   */
  async function listItems(datasetId: string, params?: { limit?: number; offset?: number }) {
    const api = useSearchAdminApi()
    const res = await api.listItems(datasetId, params)
    if (!isListItemsResponse(res)) {
      console.error('[useSearchEvaluation] 评测条目列表响应非法', res)
      return { items: [] as EvaluationItem[], total: 0 }
    }
    return { items: res.items, total: res.total }
  }

  /**
   * 删除指定 dataset 中的单条评测条目。
   * 引入动机：后端提供 DELETE /admin/evaluation/datasets/{id}/items/{itemId}。
   */
  async function deleteItem(datasetId: string, itemId: string) {
    const api = useSearchAdminApi()
    await api.deleteItem(datasetId, itemId)
  }

  /**
   * 归档（软删）指定数据集：status 置为 archived，保留全部条目与历史评测结果。
   * 归档为 API 层终态，无 restore；归档后 RunEvaluation/AddItem 返回 409。
   * 引入动机：后端 DELETE /admin/evaluation/datasets/{id} 已改为软删语义。
   */
  async function archiveDataset(datasetId: string) {
    const api = useSearchAdminApi()
    await api.deleteDataset(datasetId)
  }

  return { datasets, datasetsTotal, profiles, loading, error, load, listResults, listItems, deleteItem, archiveDataset }
}
