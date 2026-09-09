<!-- pages/workspaces/[id]/documents/history.vue — Revision History
//
// 引入动机：design/04-WEB-API.md §页面 要求 Revision History 页面。
// 列出文档的所有版本，支持查看指定版本和对比差异。
-->
<script setup lang="ts">
import type { Revision, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)
const docPath = computed(() => route.query.path as string)

const { workspace } = useWorkspaceContext(workspaceId)
const { formatDate: formatDateUtil } = useFormatDate()

const revisions = ref<Revision[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

async function loadHistory() {
  if (!workspaceId.value || !docPath.value) return
  loading.value = true
  error.value = null
  try {
    const api = useDocumentApi()
    const res = await api.history(workspaceId.value, docPath.value, { limit: limit.value, offset: offset.value })
    revisions.value = res.revisions
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.loadFailed')
  } finally {
    loading.value = false
  }
}

watch([workspaceId, docPath], () => loadHistory(), { immediate: true })

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadHistory()
}

function formatDate(s: string): string {
  return formatDateUtil(s)
}

function viewDiff(revA: number, revB?: number) {
  const query: Record<string, string> = { path: docPath.value!, rev_a: String(revA) }
  if (revB !== undefined) query.rev_b = String(revB)
  navigateTo(`/workspaces/${workspaceId.value}/documents/diff?${new URLSearchParams(query).toString()}`)
}

useHead({ title: () => t('document.revisionHistory') + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <div class="max-w-4xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <div>
          <h1 class="text-2xl font-bold text-highlighted">{{ t('document.revisionHistory') }}</h1>
          <p class="text-sm text-muted mt-1">{{ docPath }}</p>
        </div>
        <UButton
          variant="ghost"
          icon="i-lucide-arrow-left"
          :to="`/workspaces/${workspaceId}/documents/read?path=${encodeURIComponent(docPath)}`"
        >{{ t('common.back') }}</UButton>
      </div>

      <ErrorDisplay v-if="error" :message="error" />

      <div v-if="loading" class="flex justify-center py-8">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <UCard v-else-if="revisions.length > 0">
        <div class="space-y-2">
          <div
            v-for="(rev, index) in revisions"
            :key="rev.id"
            class="flex items-center justify-between border border-default rounded-lg p-3"
          >
            <div class="flex items-center gap-3">
              <div class="w-10 h-10 rounded-full bg-elevated flex items-center justify-center text-sm font-medium">
                #{{ rev.revision_number }}
              </div>
              <div>
                <p class="text-sm font-medium text-highlighted">{{ rev.title }}</p>
                <p class="text-xs text-muted">{{ formatDate(rev.created_at) }} · {{ rev.created_by.substring(0, 8) }}...</p>
              </div>
            </div>
            <div class="flex items-center gap-1">
              <UButton
                size="xs"
                variant="ghost"
                icon="i-lucide-eye"
                :to="`/workspaces/${workspaceId}/documents/revision?path=${encodeURIComponent(docPath)}&rev=${rev.revision_number}`"
              >{{ t('common.view') }}</UButton>
              <UButton
                v-if="index < revisions.length - 1"
                size="xs"
                variant="ghost"
                icon="i-lucide-arrow-left-right"
                @click="viewDiff(rev.revision_number, revisions[index + 1]!.revision_number)"
              >{{ t('document.diffWithPrev') }}</UButton>
              <UButton
                v-if="index > 0"
                size="xs"
                variant="ghost"
                icon="i-lucide-arrow-left-right"
                @click="viewDiff(revisions[0]!.revision_number, rev.revision_number)"
              >{{ t('document.diffWithCurrent') }}</UButton>
            </div>
          </div>
        </div>
      </UCard>

      <p v-else class="text-center text-muted py-8">{{ t('document.noRevisions') }}</p>

      <Pagination
        v-if="total > limit"
        :total="total"
        :limit="limit"
        :offset="offset"
        @update:offset="handleOffsetChange"
      />
    </div>
  </WorkspaceLayout>
</template>
