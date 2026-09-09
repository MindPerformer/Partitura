<!-- pages/workspaces/[id]/documents/revision.vue — Revision Viewer
//
// 引入动机：design/04-WEB-API.md §页面 要求查看历史版本内容。
// 通过 rev 查询参数指定要查看的版本号。
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
const revNumber = computed(() => parseInt(route.query.rev as string))

const { workspace } = useWorkspaceContext(workspaceId)
const { formatDate: formatDateUtil } = useFormatDate()

const revision = ref<Revision | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)

async function loadRevision() {
  if (!workspaceId.value || !docPath.value || !revNumber.value) return
  loading.value = true
  error.value = null
  try {
    const api = useDocumentApi()
    revision.value = await api.revision(workspaceId.value, docPath.value, revNumber.value)
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 404) {
      error.value = t('document.revisionNotFound')
    } else {
      error.value = apiErr.error || t('document.revisionLoadFailed')
    }
  } finally {
    loading.value = false
  }
}

watch([workspaceId, docPath, revNumber], () => loadRevision(), { immediate: true })

function formatDate(s: string): string {
  return formatDateUtil(s)
}

useHead({ title: () => `${t('document.revision')} #${revNumber} · ${t('common.appName')}` })
</script>

<template>
  <WorkspaceLayout>
    <div class="max-w-4xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <div>
          <h1 class="text-2xl font-bold text-highlighted">
            {{ t('document.revision') }} #{{ revNumber }}
          </h1>
          <p class="text-sm text-muted mt-1">{{ docPath }}</p>
        </div>
        <div class="flex gap-2">
          <UButton
            variant="ghost"
            icon="i-lucide-clock"
            :to="`/workspaces/${workspaceId}/documents/history?path=${encodeURIComponent(docPath)}`"
          >{{ t('document.history') }}</UButton>
          <UButton
            variant="ghost"
            icon="i-lucide-book-open"
            :to="`/workspaces/${workspaceId}/documents/read?path=${encodeURIComponent(docPath)}`"
          >{{ t('document.current') }}</UButton>
        </div>
      </div>

      <div v-if="loading" class="flex justify-center py-8">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <ErrorDisplay v-else-if="error" :message="error" />

      <div v-else-if="revision">
        <UCard class="mb-4">
          <div class="flex items-center gap-4 text-sm text-muted">
            <div>
              <span class="font-medium">{{ t('document.revision') }}:</span> #{{ revision.revision_number }}
            </div>
            <div>
              <span class="font-medium">{{ t('document.created') }}:</span> {{ formatDate(revision.created_at) }}
            </div>
            <div>
              <span class="font-medium">{{ t('document.createdBy') }}:</span> {{ revision.created_by.substring(0, 8) }}...
            </div>
            <div>
              <span class="font-medium">{{ t('document.hash') }}:</span> {{ revision.content_hash.substring(0, 16) }}...
            </div>
          </div>
        </UCard>

        <div class="mb-6 pb-4 border-b border-default">
          <h2 class="text-xl font-bold text-highlighted">{{ revision.title }}</h2>
        </div>

        <MarkdownRenderer :content="revision.content_markdown" />
      </div>
    </div>
  </WorkspaceLayout>
</template>
