<!-- pages/admin/search-evaluation.vue — Search Evaluation
//
// 引入动机：design/04-WEB-API.md §页面 要求 Search Evaluation 页面。
// 仅 system_admin 可访问。支持数据集管理、添加评测条目、运行评测、查看结果。
-->
<script setup lang="ts">
import type { EvaluationDataset, SearchProfile, ApiError } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()

const datasets = ref<EvaluationDataset[]>([])
const profiles = ref<SearchProfile[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

const showCreateDataset = ref(false)
const datasetForm = ref({ name: '', description: '' })
const datasetLoading = ref(false)

const showAddItem = ref(false)
const selectedDataset = ref<EvaluationDataset | null>(null)
const itemForm = ref({ query: '', relevance_grade: 1, query_class: 'general', expected_documents: '' })

const showRunEval = ref(false)
const runForm = ref({ dataset_id: '', profile_id: '' })
const runLoading = ref(false)
const runResult = ref<string | null>(null)

async function loadDatasets() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useSearchAdminApi()
    const [dsRes, profRes] = await Promise.all([
      api.listDatasets({ limit: limit.value, offset: offset.value }),
      api.listProfiles({ limit: 100 })
    ])
    datasets.value = dsRes.datasets
    total.value = dsRes.total
    profiles.value = profRes.profiles
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadEvaluationFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadDatasets)

async function handleCreateDataset() {
  if (!datasetForm.value.name) return
  datasetLoading.value = true
  try {
    const api = useSearchAdminApi()
    await api.createDataset(datasetForm.value)
    showCreateDataset.value = false
    datasetForm.value = { name: '', description: '' }
    await loadDatasets()
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.createDatasetFailed')
  } finally {
    datasetLoading.value = false
  }
}

async function handleAddItem() {
  if (!selectedDataset.value || !itemForm.value.query) return
  try {
    const api = useSearchAdminApi()
    const docs = itemForm.value.expected_documents
      ? itemForm.value.expected_documents.split(',').map(s => ({ document_id: s.trim() })).filter(d => d.document_id)
      : []
    await api.addItem(selectedDataset.value.id, {
      query: itemForm.value.query,
      expected_documents: docs,
      relevance_grade: itemForm.value.relevance_grade,
      query_class: itemForm.value.query_class
    })
    showAddItem.value = false
    itemForm.value = { query: '', relevance_grade: 1, query_class: 'general', expected_documents: '' }
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.addItemFailed')
  }
}

async function handleRunEval() {
  if (!runForm.value.dataset_id || !runForm.value.profile_id) return
  runLoading.value = true
  runResult.value = null
  try {
    const api = useSearchAdminApi()
    const res = await api.runEvaluation({
      dataset_id: runForm.value.dataset_id,
      profile_id: runForm.value.profile_id
    })
    runResult.value = t('admin.evaluationJobEnqueued', { jobId: res.job_id })
    showRunEval.value = false
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.runEvaluationFailed')
  } finally {
    runLoading.value = false
  }
}

const datasetItems = computed<SelectItem[]>(() =>
  datasets.value.map(d => ({ label: d.name, value: d.id }))
)

const profileItems = computed<SelectItem[]>(() =>
  profiles.value.map(p => ({ label: p.name, value: p.id }))
)

useHead({ title: () => t('admin.searchEvaluation') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-4xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.searchEvaluation') }}</h1>
        <div class="flex gap-2">
          <UButton variant="outline" @click="showRunEval = true">{{ t('admin.runEvaluation') }}</UButton>
          <UButton icon="i-lucide-plus" @click="showCreateDataset = true">{{ t('admin.newDataset') }}</UButton>
        </div>
      </div>

      <div v-if="!isSystemAdmin" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('admin.systemAdminRequired') }}</p>
      </div>

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />
        <div v-if="runResult" class="rounded-lg border border-success/20 bg-success/10 p-3 mb-4 text-sm text-success">{{ runResult }}</div>

        <div v-if="loading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <div v-else-if="datasets.length > 0" class="space-y-3">
          <div
            v-for="ds in datasets"
            :key="ds.id"
            class="border border-default rounded-lg p-4"
          >
            <div class="flex items-start justify-between">
              <div>
                <h3 class="font-medium text-highlighted">{{ ds.name }}</h3>
                <p class="text-xs text-muted mt-1">{{ ds.description }}</p>
              </div>
              <UButton size="xs" variant="ghost" icon="i-lucide-plus" @click="selectedDataset = ds; showAddItem = true">{{ t('admin.addItem') }}</UButton>
            </div>
          </div>
        </div>

        <p v-else class="text-center text-muted py-8">{{ t('admin.noDatasets') }}</p>
      </template>
    </div>

    <!-- Create Dataset Modal -->
    <UModal v-model:open="showCreateDataset">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.createDataset') }}</h3>
          <form @submit.prevent="handleCreateDataset" class="space-y-4">
            <UFormField :label="t('workspace.name')" name="name">
              <UInput v-model="datasetForm.name" class="w-full" />
            </UFormField>
            <UFormField :label="t('workspace.description')" name="description">
              <UTextarea v-model="datasetForm.description" class="w-full" />
            </UFormField>
            <div class="flex justify-end gap-2">
              <UButton color="neutral" variant="ghost" @click="showCreateDataset = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="datasetLoading">{{ t('common.create') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>

    <!-- Add Item Modal -->
    <UModal v-model:open="showAddItem">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.addItemTo', { name: selectedDataset?.name }) }}</h3>
          <form @submit.prevent="handleAddItem" class="space-y-4">
            <UFormField :label="t('admin.query')" name="query">
              <UInput v-model="itemForm.query" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.expectedDocuments')" name="expected_documents">
              <UInput v-model="itemForm.expected_documents" placeholder="uuid1, uuid2" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.relevanceGrade')" name="relevance_grade">
              <UInput v-model.number="itemForm.relevance_grade" type="number" min="0" max="3" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.queryClass')" name="query_class">
              <UInput v-model="itemForm.query_class" class="w-full" />
            </UFormField>
            <div class="flex justify-end gap-2">
              <UButton color="neutral" variant="ghost" @click="showAddItem = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit">{{ t('common.add') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>

    <!-- Run Evaluation Modal -->
    <UModal v-model:open="showRunEval">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.runEvaluation') }}</h3>
          <form @submit.prevent="handleRunEval" class="space-y-4">
            <UFormField :label="t('admin.searchEvaluation')" name="dataset_id">
              <USelect v-model="runForm.dataset_id" :items="datasetItems" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <UFormField :label="t('admin.searchProfiles')" name="profile_id">
              <USelect v-model="runForm.profile_id" :items="profileItems" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <div class="flex justify-end gap-2">
              <UButton color="neutral" variant="ghost" @click="showRunEval = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="runLoading">{{ t('common.run') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>
  </div>
</template>
