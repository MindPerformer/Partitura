<!-- pages/workspaces/[id]/documents/diff.vue — Diff Viewer
//
// 引入动机：design/04-WEB-API.md §页面 要求 Diff Viewer 页面。
// 当前/任意 revision 实际对比，避免伪静态差异。
// 通过 rev_a 和 rev_b 查询参数指定对比的两个版本。
-->
<script setup lang="ts">
import type { Revision, ApiError } from '~/types/api'
import { computeLineDiff, computeDiffStats, type DiffLine, type DiffStats } from '~/utils/diff'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)
const docPath = computed(() => route.query.path as string)
const revA = computed(() => parseInt(route.query.rev_a as string))
const revB = computed(() => parseInt(route.query.rev_b as string))

// 参数缺失/非法（path 缺、rev_a 缺失或 NaN、rev_b 提供但 NaN）时给明确错误态。
const paramError = computed(() => {
  if (!docPath.value) return t('document.pathMissing')
  // rev_a 必须为正整数版本号；缺失/NaN/0 均视为非法参数。
  if (Number.isNaN(revA.value) || revA.value < 1) return t('document.revParamInvalid')
  if (route.query.rev_b !== undefined && (Number.isNaN(revB.value) || revB.value < 1)) return t('document.revParamInvalid')
  return null
})

const { workspace } = useWorkspaceContext(workspaceId)

const revAData = ref<Revision | null>(null)
const revBData = ref<Revision | null>(null)
const diffLines = ref<DiffLine[]>([])
const stats = ref<DiffStats>({ additions: 0, deletions: 0, unchanged: 0 })
const loading = ref(false)
const error = ref<string | null>(null)

async function loadDiff() {
  if (!workspaceId.value || !docPath.value || !revA.value) return
  if (paramError.value) { error.value = paramError.value; return }
  loading.value = true
  error.value = null

  try {
    const api = useDocumentApi()

    // 获取两个 revision
    const promises: Promise<Revision>[] = [api.revision(workspaceId.value, docPath.value, revA.value)]
    if (revB.value) {
      promises.push(api.revision(workspaceId.value, docPath.value, revB.value))
    } else {
      // 如果没有 rev_b，获取当前文档作为对比
      const currentDoc = await api.read(workspaceId.value, docPath.value)
      revAData.value = await promises[0]!
      revBData.value = {
        id: '',
        document_id: currentDoc.id,
        revision_number: currentDoc.revision_number,
        path: currentDoc.path,
        title: currentDoc.title,
        content_markdown: currentDoc.content_markdown,
        content_hash: currentDoc.content_hash,
        status: currentDoc.status,
        created_by: currentDoc.updated_by,
        created_by_username: currentDoc.updated_by_username,
        created_at: currentDoc.updated_at
      }
      diffLines.value = computeLineDiff(revAData.value!.content_markdown, revBData.value!.content_markdown)
      stats.value = computeDiffStats(diffLines.value)
      loading.value = false
      return
    }

    const [a, b] = await Promise.all(promises)
    revAData.value = a ?? null
    revBData.value = b ?? null
    if (a && b) {
      diffLines.value = computeLineDiff(a.content_markdown, b.content_markdown)
      stats.value = computeDiffStats(diffLines.value)
    }
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('document.diffLoadFailed')
  } finally {
    loading.value = false
  }
}

watch([workspaceId, docPath, revA, revB], () => loadDiff(), { immediate: true })

useHead({ title: () => t('document.diffViewer') + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <div class="max-w-3xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <div>
          <h1 class="text-2xl font-bold text-highlighted">{{ t('document.diffViewer') }}</h1>
          <DocBreadcrumb :workspace-id="workspaceId" :path="docPath" class="mt-1" />
        </div>
        <UButton
          variant="ghost"
          icon="i-lucide-arrow-left"
          :to="`/workspaces/${workspaceId}/documents/history?path=${encodeURIComponent(docPath)}`"
        >{{ t('common.back') }}</UButton>
      </div>

      <ErrorDisplay v-if="paramError" :message="paramError" />

      <div v-else-if="loading" class="flex justify-center py-8">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <ErrorDisplay v-else-if="error" :message="error" />

      <div v-else>
        <!-- Diff stats -->
        <div class="flex items-center gap-4 mb-4 text-sm">
          <span class="text-muted">
            <template v-if="revB">{{ t('document.comparing', { a: revA, b: revB }) }}</template>
            <template v-else>{{ t('document.comparingCurrent', { a: revA }) }}</template>
          </span>
          <span class="text-success">+{{ stats.additions }}</span>
          <span class="text-error">-{{ stats.deletions }}</span>
        </div>

        <!-- Diff content -->
        <div class="rounded-lg border border-default overflow-hidden">
          <div class="bg-elevated font-mono text-sm overflow-x-auto">
            <table class="w-full">
              <tbody>
                <!-- 提升暗色对比度：背景 /10→/20，左侧 2px 语义色条强化增删行识别 -->
                <tr
                  v-for="(line, i) in diffLines"
                  :key="i"
                  :class="{
                    'bg-success/20 border-l-2 border-l-success': line.type === 'added',
                    'bg-error/20 border-l-2 border-l-error': line.type === 'removed'
                  }"
                >
                  <td class="w-12 text-right text-xs text-muted px-2 select-none border-r border-default">
                    {{ line.oldLineNumber ?? '' }}
                  </td>
                  <td class="w-12 text-right text-xs text-muted px-2 select-none border-r border-default">
                    {{ line.newLineNumber ?? '' }}
                  </td>
                  <td class="w-6 text-center select-none border-r border-default">
                    <span v-if="line.type === 'added'" class="text-success">+</span>
                    <span v-else-if="line.type === 'removed'" class="text-error">-</span>
                    <span v-else>&nbsp;</span>
                  </td>
                  <td class="px-3 py-0.5 whitespace-pre-wrap break-all">
                    {{ line.content || '&nbsp;' }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </div>
  </WorkspaceLayout>
</template>
