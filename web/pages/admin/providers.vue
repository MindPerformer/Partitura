<!-- pages/admin/providers.vue — Provider 配置管理页面
//
// 引入动机：计划要求 system_admin 通过 Web 页面配置 Embedding 和 Reranker Provider。
// Provider API Key 通过 AES-256-GCM 加密后存储于 PostgreSQL，保存后自动热更新。
//
// 安全契约：
//   - API Key 输入仅用于 PUT，成功/失败后立即清空，不缓存、不回显
//   - GET 仅显示是否已设置（api_key_set 布尔值），不显示明文或密文
//   - 页面不存储 API Key 到任何持久状态
//   - 测试请求不持久化
//
// 页面行为：
//   - 加载时 GET /api/admin/providers 获取当前状态
//   - 保存时 PUT /api/admin/providers/{type}，成功后清空 API Key 输入
//   - 测试时 POST /api/admin/providers/{type}/test，不持久化
-->
<script setup lang="ts">
import type {
  ApiError,
  ListProvidersResponse,
  ProviderConfigDTO,
  EmbeddingProviderConfig,
  RerankerProviderConfig,
  PutProviderResponse,
  TestProviderResponse
} from '~/types/api'

definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()

// 加载状态
const loading = ref(false)
const loadError = ref<string | null>(null)

// Provider 状态
const embeddingStatus = ref<ProviderConfigDTO | null>(null)
const rerankerStatus = ref<ProviderConfigDTO | null>(null)

// Embedding 表单
const embApiKey = ref('')
const embConfig = ref<EmbeddingProviderConfig>({
  base_url: '',
  model: '',
  dimensions: 1024,
  timeout_seconds: 30,
  batch_size: 32,
  query_instruction: '',
  document_instruction: ''
})
const embSaving = ref(false)
const embTesting = ref(false)
const embSaveError = ref<string | null>(null)
const embSaveSuccess = ref<string | null>(null)
const embTestResult = ref<TestProviderResponse | null>(null)

// Reranker 表单
const rrApiKey = ref('')
const rrConfig = ref<RerankerProviderConfig>({
  base_url: '',
  model: '',
  timeout_seconds: 10,
  max_candidates: 20
})
const rrSaving = ref(false)
const rrTesting = ref(false)
const rrSaveError = ref<string | null>(null)
const rrSaveSuccess = ref<string | null>(null)
const rrTestResult = ref<TestProviderResponse | null>(null)

useHead({ title: () => t('admin.providers') + ' · ' + t('common.appName') })

async function loadProviders() {
  if (!isSystemAdmin.value) return
  loading.value = true
  loadError.value = null
  try {
    const api = useProviderApi()
    const res = await api.list()
    embeddingStatus.value = res.providers['embedding'] || null
    rerankerStatus.value = res.providers['reranker'] || null

    // 如果已有配置，填充非敏感字段到表单（不填充 API Key）
    if (embeddingStatus.value?.config) {
      const cfg = embeddingStatus.value.config as EmbeddingProviderConfig
      if (cfg.base_url) embConfig.value = { ...embConfig.value, ...cfg }
    }
    if (rerankerStatus.value?.config) {
      const cfg = rerankerStatus.value.config as RerankerProviderConfig
      if (cfg.base_url) rrConfig.value = { ...rrConfig.value, ...cfg }
    }
  } catch (err) {
    const apiErr = err as ApiError
    loadError.value = apiErr.error || t('admin.loadProvidersFailed')
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  loadProviders()
})

async function saveEmbedding() {
  embSaveError.value = null
  embSaveSuccess.value = null

  if (!embApiKey.value) {
    embSaveError.value = t('admin.providerApiKeyRequired')
    return
  }

  embSaving.value = true
  try {
    const api = useProviderApi()
    const res = await api.put('embedding', {
      api_key: embApiKey.value,
      config: embConfig.value
    })
    embSaveSuccess.value = res.message || t('admin.saveProviderSuccess')

    // 安全：成功后立即清空 API Key 输入，不缓存、不回显
    embApiKey.value = ''

    // 刷新状态
    await loadProviders()
  } catch (err) {
    const apiErr = err as ApiError
    embSaveError.value = apiErr.error || t('admin.saveProviderFailed')
    // 失败也清空 API Key 输入——安全原则
    embApiKey.value = ''
  } finally {
    embSaving.value = false
  }
}

async function testEmbedding() {
  embTestResult.value = null
  embSaveError.value = null
  embSaveSuccess.value = null

  if (!embApiKey.value) {
    embSaveError.value = t('admin.providerApiKeyRequired')
    return
  }

  embTesting.value = true
  try {
    const api = useProviderApi()
    embTestResult.value = await api.test('embedding', {
      api_key: embApiKey.value,
      config: embConfig.value
    })
  } catch (err) {
    const apiErr = err as ApiError
    embTestResult.value = {
      status: 'failed',
      message: apiErr.error || t('admin.testFailed')
    }
  } finally {
    embTesting.value = false
    // 测试后清空 API Key 输入——安全原则
    embApiKey.value = ''
  }
}

async function saveReranker() {
  rrSaveError.value = null
  rrSaveSuccess.value = null

  if (!rrApiKey.value) {
    rrSaveError.value = t('admin.providerApiKeyRequired')
    return
  }

  rrSaving.value = true
  try {
    const api = useProviderApi()
    const res = await api.put('reranker', {
      api_key: rrApiKey.value,
      config: rrConfig.value
    })
    rrSaveSuccess.value = res.message || t('admin.saveProviderSuccess')

    // 安全：成功后立即清空 API Key 输入
    rrApiKey.value = ''

    // 刷新状态
    await loadProviders()
  } catch (err) {
    const apiErr = err as ApiError
    rrSaveError.value = apiErr.error || t('admin.saveProviderFailed')
    rrApiKey.value = ''
  } finally {
    rrSaving.value = false
  }
}

async function testReranker() {
  rrTestResult.value = null
  rrSaveError.value = null
  rrSaveSuccess.value = null

  if (!rrApiKey.value) {
    rrSaveError.value = t('admin.providerApiKeyRequired')
    return
  }

  rrTesting.value = true
  try {
    const api = useProviderApi()
    rrTestResult.value = await api.test('reranker', {
      api_key: rrApiKey.value,
      config: rrConfig.value
    })
  } catch (err) {
    const apiErr = err as ApiError
    rrTestResult.value = {
      status: 'failed',
      message: apiErr.error || t('admin.testFailed')
    }
  } finally {
    rrTesting.value = false
    // 测试后清空 API Key 输入
    rrApiKey.value = ''
  }
}
</script>

<template>
  <div class="max-w-4xl mx-auto px-4 py-8">
    <div class="mb-6">
      <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.providers') }}</h1>
      <p class="text-sm text-muted mt-1">{{ t('admin.providersDesc') }}</p>
    </div>

    <!-- 加载中 -->
    <div v-if="loading" class="flex items-center justify-center py-12">
      <UIcon name="i-lucide-loader-2" class="w-6 h-6 animate-spin text-primary" />
      <span class="ml-2 text-muted">{{ t('common.loading') }}</span>
    </div>

    <!-- 加载错误 -->
    <UAlert
      v-else-if="loadError"
      color="error"
      variant="soft"
      :title="loadError"
      :icon="'i-lucide-alert-circle'"
    />

    <div v-else class="space-y-6">
      <!-- Embedding Provider -->
      <UCard>
        <template #header>
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2">
              <UIcon name="i-lucide-vector" class="w-5 h-5 text-primary" />
              <h2 class="text-lg font-semibold">{{ t('admin.embeddingProvider') }}</h2>
            </div>
            <div class="flex items-center gap-2 text-sm">
              <UBadge
                v-if="embeddingStatus?.configured"
                color="success"
                variant="soft"
                size="sm"
                :label="t('admin.providerConfigured')"
              />
              <UBadge
                v-else
                color="neutral"
                variant="soft"
                size="sm"
                :label="t('admin.providerNotConfigured')"
              />
              <UBadge
                v-if="embeddingStatus?.api_key_set"
                color="success"
                variant="soft"
                size="sm"
                :label="t('admin.apiKeySet')"
              />
              <UBadge
                v-else
                color="warning"
                variant="soft"
                size="sm"
                :label="t('admin.apiKeyNotSet')"
              />
            </div>
          </div>
        </template>

        <form @submit.prevent="saveEmbedding" class="space-y-4">
          <UFormField :label="t('admin.apiKey')" :hint="t('admin.apiKeyHint')">
            <UInput
              v-model="embApiKey"
              :placeholder="t('admin.apiKeyPlaceholder')"
              type="password"
              autocomplete="off"
              class="w-full"
            />
          </UFormField>

          <UFormField :label="t('admin.baseUrl')">
            <UInput
              v-model="embConfig.base_url"
              :placeholder="t('admin.baseUrlPlaceholder')"
              class="w-full"
            />
          </UFormField>

          <div class="grid grid-cols-2 gap-4">
            <UFormField :label="t('admin.model')">
              <UInput
                v-model="embConfig.model"
                :placeholder="t('admin.modelPlaceholder')"
                class="w-full"
              />
            </UFormField>

            <UFormField :label="t('admin.dimensions')">
              <UInput
                v-model.number="embConfig.dimensions"
                type="number"
                :placeholder="'1024'"
                class="w-full"
              />
            </UFormField>
          </div>

          <div class="grid grid-cols-3 gap-4">
            <UFormField :label="t('admin.timeoutSeconds')">
              <UInput
                v-model.number="embConfig.timeout_seconds"
                type="number"
                :placeholder="'30'"
                class="w-full"
              />
            </UFormField>

            <UFormField :label="t('admin.batchSize')">
              <UInput
                v-model.number="embConfig.batch_size"
                type="number"
                :placeholder="'32'"
                class="w-full"
              />
            </UFormField>

            <UFormField :label="t('admin.lastUpdated')">
              <div v-if="embeddingStatus?.updated_at" class="text-sm text-muted py-2">
                {{ embeddingStatus.updated_at }}
              </div>
              <div v-else class="text-sm text-muted py-2">—</div>
            </UFormField>
          </div>

          <div class="grid grid-cols-2 gap-4">
            <UFormField :label="t('admin.queryInstruction')">
              <UInput
                v-model="embConfig.query_instruction"
                :placeholder="'（可选）'"
                class="w-full"
              />
            </UFormField>

            <UFormField :label="t('admin.documentInstruction')">
              <UInput
                v-model="embConfig.document_instruction"
                :placeholder="'（可选）'"
                class="w-full"
              />
            </UFormField>
          </div>

          <!-- 错误/成功提示 -->
          <UAlert
            v-if="embSaveError"
            color="error"
            variant="soft"
            :title="embSaveError"
            :icon="'i-lucide-alert-circle'"
          />
          <UAlert
            v-if="embSaveSuccess"
            color="success"
            variant="soft"
            :title="embSaveSuccess"
            :icon="'i-lucide-check-circle'"
          />
          <UAlert
            v-if="embTestResult"
            :color="embTestResult.status === 'ok' ? 'success' : embTestResult.status === 'unavailable' ? 'warning' : 'error'"
            variant="soft"
            :title="embTestResult.message || (embTestResult.status === 'ok' ? t('admin.testSuccess') : t('admin.testFailed'))"
            :icon="embTestResult.status === 'ok' ? 'i-lucide-check-circle' : 'i-lucide-alert-circle'"
          />

          <div class="flex gap-2">
            <UButton
              type="submit"
              color="primary"
              :loading="embSaving"
              :disabled="embSaving"
              :label="embSaving ? t('admin.saving') : t('admin.saveProvider')"
            />
            <UButton
              type="button"
              color="neutral"
              variant="outline"
              :loading="embTesting"
              :disabled="embTesting"
              :label="embTesting ? t('admin.testing') : t('admin.testProvider')"
              @click="testEmbedding"
            />
          </div>
        </form>
      </UCard>

      <!-- Reranker Provider -->
      <UCard>
        <template #header>
          <div class="flex items-center justify-between">
            <div class="flex items-center gap-2">
              <UIcon name="i-lucide-list-ordered" class="w-5 h-5 text-primary" />
              <h2 class="text-lg font-semibold">{{ t('admin.rerankerProvider') }}</h2>
            </div>
            <div class="flex items-center gap-2 text-sm">
              <UBadge
                v-if="rerankerStatus?.configured"
                color="success"
                variant="soft"
                size="sm"
                :label="t('admin.providerConfigured')"
              />
              <UBadge
                v-else
                color="neutral"
                variant="soft"
                size="sm"
                :label="t('admin.providerNotConfigured')"
              />
              <UBadge
                v-if="rerankerStatus?.api_key_set"
                color="success"
                variant="soft"
                size="sm"
                :label="t('admin.apiKeySet')"
              />
              <UBadge
                v-else
                color="warning"
                variant="soft"
                size="sm"
                :label="t('admin.apiKeyNotSet')"
              />
            </div>
          </div>
        </template>

        <form @submit.prevent="saveReranker" class="space-y-4">
          <UFormField :label="t('admin.apiKey')" :hint="t('admin.apiKeyHint')">
            <UInput
              v-model="rrApiKey"
              :placeholder="t('admin.apiKeyPlaceholder')"
              type="password"
              autocomplete="off"
              class="w-full"
            />
          </UFormField>

          <UFormField :label="t('admin.baseUrl')">
            <UInput
              v-model="rrConfig.base_url"
              :placeholder="t('admin.baseUrlPlaceholder')"
              class="w-full"
            />
          </UFormField>

          <div class="grid grid-cols-2 gap-4">
            <UFormField :label="t('admin.model')">
              <UInput
                v-model="rrConfig.model"
                :placeholder="t('admin.modelPlaceholder')"
                class="w-full"
              />
            </UFormField>

            <UFormField :label="t('admin.lastUpdated')">
              <div v-if="rerankerStatus?.updated_at" class="text-sm text-muted py-2">
                {{ rerankerStatus.updated_at }}
              </div>
              <div v-else class="text-sm text-muted py-2">—</div>
            </UFormField>
          </div>

          <div class="grid grid-cols-2 gap-4">
            <UFormField :label="t('admin.timeoutSeconds')">
              <UInput
                v-model.number="rrConfig.timeout_seconds"
                type="number"
                :placeholder="'10'"
                class="w-full"
              />
            </UFormField>

            <UFormField :label="t('admin.maxCandidates')">
              <UInput
                v-model.number="rrConfig.max_candidates"
                type="number"
                :placeholder="'20'"
                class="w-full"
              />
            </UFormField>
          </div>

          <!-- 错误/成功提示 -->
          <UAlert
            v-if="rrSaveError"
            color="error"
            variant="soft"
            :title="rrSaveError"
            :icon="'i-lucide-alert-circle'"
          />
          <UAlert
            v-if="rrSaveSuccess"
            color="success"
            variant="soft"
            :title="rrSaveSuccess"
            :icon="'i-lucide-check-circle'"
          />
          <UAlert
            v-if="rrTestResult"
            :color="rrTestResult.status === 'ok' ? 'success' : rrTestResult.status === 'unavailable' ? 'warning' : 'error'"
            variant="soft"
            :title="rrTestResult.message || (rrTestResult.status === 'ok' ? t('admin.testSuccess') : t('admin.testFailed'))"
            :icon="rrTestResult.status === 'ok' ? 'i-lucide-check-circle' : 'i-lucide-alert-circle'"
          />

          <div class="flex gap-2">
            <UButton
              type="submit"
              color="primary"
              :loading="rrSaving"
              :disabled="rrSaving"
              :label="rrSaving ? t('admin.saving') : t('admin.saveProvider')"
            />
            <UButton
              type="button"
              color="neutral"
              variant="outline"
              :loading="rrTesting"
              :disabled="rrTesting"
              :label="rrTesting ? t('admin.testing') : t('admin.testProvider')"
              @click="testReranker"
            />
          </div>
        </form>
      </UCard>
    </div>
  </div>
</template>
