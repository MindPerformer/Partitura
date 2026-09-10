<!-- pages/workspaces/[id]/members.vue — Workspace Members
//
// 引入动机：design/04-WEB-API.md §页面 要求 Workspace Members 页面。
// 按 RBAC 控制成员管理操作的可见性。
-->
<script setup lang="ts">
import type { Member, ApiError } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)

const { workspace, canManageMembers, currentMemberRole } = useWorkspaceContext(workspaceId)
const { workspaceRoleLabel } = useEnumLabels()

const members = ref<Member[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

const showAddModal = ref(false)
const addForm = ref({ username: '', role: 'viewer' })
const selectedCandidate = ref<Member | null>(null)
const candidates = ref<Member[]>([])
const candidateLoading = ref(false)
const candidateSearched = ref(false)
const candidateOpen = ref(false)
const candidateError = ref<string | null>(null)
const addLoading = ref(false)
const addError = ref<string | null>(null)
let candidateTimer: ReturnType<typeof setTimeout> | undefined
let candidateSearchVersion = 0

async function loadMembers() {
  if (!workspaceId.value) return
  loading.value = true
  error.value = null
  try {
    const api = useWorkspaceApi()
    const res = await api.listMembers(workspaceId.value, { limit: limit.value, offset: offset.value })
    members.value = res.members
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('workspace.loadFailed')
  } finally {
    loading.value = false
  }
}

watch(workspaceId, () => loadMembers(), { immediate: true })

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadMembers()
}

watch(() => addForm.value.username, (value) => {
  const version = ++candidateSearchVersion
  selectedCandidate.value = null
  candidateError.value = null
  candidateSearched.value = false
  candidates.value = []
  candidateOpen.value = false
  if (candidateTimer) clearTimeout(candidateTimer)

  const query = value.trim()
  if (!query || !workspaceId.value || !canManageMembers.value) return

  candidateTimer = setTimeout(async () => {
    candidateLoading.value = true
    candidateOpen.value = true
    try {
      const res = await useWorkspaceApi().listMemberCandidates(workspaceId.value, query, 8)
      if (version !== candidateSearchVersion) return
      candidates.value = res.users
      candidateSearched.value = true
    } catch (err) {
      if (version !== candidateSearchVersion) return
      const apiErr = err as ApiError
      candidateError.value = apiErr.error || t('workspace.candidateSearchFailed')
      candidateSearched.value = true
    } finally {
      if (version === candidateSearchVersion) candidateLoading.value = false
    }
  }, 250)
})

function selectCandidate(candidate: Member) {
  selectedCandidate.value = candidate
  addForm.value.username = candidate.username
  candidateOpen.value = false
  candidateError.value = null
}

function resetAddForm() {
  candidateSearchVersion++
  if (candidateTimer) clearTimeout(candidateTimer)
  addForm.value = { username: '', role: 'viewer' }
  selectedCandidate.value = null
  candidates.value = []
  candidateSearched.value = false
  candidateOpen.value = false
  candidateError.value = null
  addError.value = null
}

function openAddModal() {
  resetAddForm()
  showAddModal.value = true
}

async function handleAdd() {
  if (!selectedCandidate.value || selectedCandidate.value.username !== addForm.value.username.trim()) {
    addError.value = t('workspace.memberCandidateRequired')
    return
  }
  addLoading.value = true
  addError.value = null
  try {
    const api = useWorkspaceApi()
    await api.addMember(workspaceId.value, { username: selectedCandidate.value.username, role: addForm.value.role })
    showAddModal.value = false
    resetAddForm()
    await loadMembers()
  } catch (err) {
    const apiErr = err as ApiError
    if (apiErr.status === 409) {
      addError.value = t('workspace.memberExists')
    } else if (apiErr.status === 404) {
      addError.value = t('workspace.userNotFound')
    } else {
      addError.value = apiErr.error || t('workspace.addMemberFailed')
    }
  } finally {
    addLoading.value = false
  }
}

onBeforeUnmount(() => {
  if (candidateTimer) clearTimeout(candidateTimer)
})

async function handleUpdateRole(member: Member, newRole: string) {
  try {
    const api = useWorkspaceApi()
    const updated = await api.updateMemberRole(workspaceId.value, member.user_id, { role: newRole })
    const idx = members.value.findIndex(m => m.user_id === member.user_id)
    if (idx >= 0) members.value[idx] = updated
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('workspace.updateRoleFailed')
  }
}

async function handleRemove(member: Member) {
  if (!confirm(t('workspace.removeMemberConfirm', { name: member.username }))) return
  try {
    const api = useWorkspaceApi()
    await api.removeMember(workspaceId.value, member.user_id)
    members.value = members.value.filter(m => m.user_id !== member.user_id)
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('workspace.removeMemberFailed')
  }
}

const roleOptions = computed<SelectItem[]>(() => [
  { label: t('workspace.roleViewer'), value: 'viewer' },
  { label: t('workspace.roleEditor'), value: 'editor' },
  { label: t('workspace.roleAdmin'), value: 'admin' }
  // owner 不能通过此界面设置
])

useHead({ title: () => t('workspace.members') + ' · ' + t('common.appName') })
</script>

<template>
  <WorkspaceLayout>
    <div class="mx-auto min-w-0 max-w-4xl px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('workspace.members') }}</h1>
        <UButton
          v-if="canManageMembers"
          icon="i-lucide-plus"
          @click="openAddModal"
        >{{ t('workspace.addMember') }}</UButton>
      </div>

      <ErrorDisplay v-if="error" :message="error" />

      <div v-if="loading" class="flex justify-center py-8">
        <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
      </div>

      <UCard v-else-if="members.length > 0">
        <div class="space-y-2">
          <div
            v-for="member in members"
            :key="member.id"
            class="flex items-center justify-between border border-default rounded-lg p-3"
          >
            <div class="flex items-center gap-3">
              <UAvatar :alt="member.username.charAt(0).toUpperCase()" size="sm" />
              <div>
                <p class="text-sm font-medium text-highlighted">{{ member.username }}</p>
                <p class="text-xs text-muted">{{ member.email }}</p>
              </div>
            </div>
            <div class="flex items-center gap-2">
              <UBadge
                :color="member.role === 'owner' ? 'warning' : member.role === 'admin' ? 'info' : member.role === 'editor' ? 'success' : 'neutral'"
                variant="subtle"
                size="sm"
              >{{ workspaceRoleLabel(member.role) }}</UBadge>
              <USelect
                v-if="canManageMembers && member.role !== 'owner'"
                :model-value="member.role"
                :items="roleOptions"
                value-key="value"
                label-key="label"
                size="xs"
                class="w-24"
                @update:model-value="(val) => handleUpdateRole(member, String(val))"
              />
              <UButton
                v-if="canManageMembers && member.role !== 'owner'"
                size="xs"
                variant="ghost"
                color="error"
                icon="i-lucide-trash-2"
                @click="handleRemove(member)"
              />
            </div>
          </div>
        </div>
      </UCard>

      <p v-else class="text-center text-muted py-8">{{ t('workspace.noMembers') }}</p>

      <Pagination
        v-if="total > limit"
        :total="total"
        :limit="limit"
        :offset="offset"
        @update:offset="handleOffsetChange"
      />
    </div>

    <!-- Add Member Modal -->
    <UModal v-model:open="showAddModal">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('workspace.addMember') }}</h3>
          <form @submit.prevent="handleAdd" class="space-y-4">
            <UFormField :label="t('workspace.username')" name="username">
              <div class="relative">
                <UInput
                  v-model="addForm.username"
                  :placeholder="t('workspace.usernamePlaceholder')"
                  autocomplete="off"
                  class="w-full"
                  @focus="candidateOpen = addForm.username.trim().length > 0"
                />
                <div
                  v-if="candidateOpen && addForm.username.trim()"
                  class="absolute left-0 right-0 top-full z-10 mt-1 overflow-hidden rounded-lg border border-default bg-default shadow-lg"
                >
                  <div v-if="candidateLoading" class="flex items-center gap-2 px-3 py-3 text-sm text-muted">
                    <UIcon name="i-lucide-loader-circle" class="h-4 w-4 animate-spin" />
                    {{ t('workspace.searchingCandidates') }}
                  </div>
                  <ErrorDisplay v-else-if="candidateError" :message="candidateError" class="m-2" />
                  <div v-else-if="candidateSearched && candidates.length === 0" class="px-3 py-3 text-sm text-muted">
                    {{ t('workspace.noCandidateResults') }}
                  </div>
                  <button
                    v-for="candidate in candidates"
                    :key="candidate.user_id"
                    type="button"
                    class="flex w-full items-center gap-3 px-3 py-2 text-left transition-colors hover:bg-elevated"
                    @mousedown.prevent
                    @click="selectCandidate(candidate)"
                  >
                    <UAvatar :alt="candidate.username.charAt(0).toUpperCase()" size="xs" />
                    <span class="min-w-0">
                      <span class="block truncate text-sm font-medium text-highlighted">{{ candidate.username }}</span>
                      <span class="block truncate text-xs text-muted">{{ candidate.email }}</span>
                    </span>
                  </button>
                </div>
              </div>
            </UFormField>
            <p v-if="selectedCandidate" class="text-xs text-success">
              {{ t('workspace.selectedCandidate', { username: selectedCandidate.username }) }}
            </p>
            <UFormField :label="t('workspace.role')" name="role">
              <USelect v-model="addForm.role" :items="roleOptions" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <ErrorDisplay v-if="addError" :message="addError" />
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="showAddModal = false; resetAddForm()">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="addLoading">{{ t('common.add') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>
  </WorkspaceLayout>
</template>
