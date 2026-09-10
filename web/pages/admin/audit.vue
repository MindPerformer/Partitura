<!-- pages/admin/audit.vue — Audit Log
//
// 引入动机：design/04-WEB-API.md §页面 要求 Audit Log 页面。
// 仅 system_admin 可访问。
-->
<script setup lang="ts">
import type { AuditEntry, ApiError } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { formatDate: formatDateUtil } = useFormatDate()

const entries = ref<AuditEntry[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

async function loadAudit() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useAdminApi()
    const res = await api.listAudit({ limit: limit.value, offset: offset.value })
    entries.value = res.entries
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadAuditFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadAudit)

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadAudit()
}

function formatDate(s: string): string {
  return formatDateUtil(s)
}

function formatDetail(detail: unknown): string {
  if (!detail) return ''
  try {
    return JSON.stringify(detail, null, 2)
  } catch {
    return String(detail)
  }
}

useHead({ title: () => t('admin.auditLog') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-4xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">{{ t('admin.auditLog') }}</h1>

      <div v-if="!isSystemAdmin" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('admin.systemAdminRequired') }}</p>
      </div>

      <template v-else>
        <ErrorDisplay v-if="error" :message="error" />

        <div v-if="loading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <UCard v-else-if="entries.length > 0">
          <div class="space-y-2">
            <div
              v-for="entry in entries"
              :key="entry.id"
              class="border border-default rounded-lg p-3"
            >
              <div class="flex items-start justify-between mb-1">
                <div class="flex items-center gap-2">
                  <UBadge variant="subtle" size="sm">{{ entry.action }}</UBadge>
                  <span class="text-xs text-muted">{{ entry.resource_type }}</span>
                </div>
                <span class="text-xs text-dimmed">{{ formatDate(entry.created_at) }}</span>
              </div>
              <p class="text-xs text-muted">
                User: {{ entry.user_id.substring(0, 8) }}...
                <template v-if="entry.workspace_id"> · WS: {{ entry.workspace_id.substring(0, 8) }}...</template>
                <template v-if="entry.resource_id"> · Resource: {{ entry.resource_id.substring(0, 8) }}...</template>
              </p>
              <pre v-if="entry.detail" class="text-xs text-dimmed mt-1 bg-elevated rounded p-2 overflow-x-auto">{{ formatDetail(entry.detail) }}</pre>
            </div>
          </div>
        </UCard>

        <p v-else class="text-center text-muted py-8">{{ t('admin.noAuditEntries') }}</p>

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="handleOffsetChange"
        />
      </template>
    </div>
  </div>
</template>
