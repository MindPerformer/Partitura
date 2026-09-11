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
const toast = useToast()

const members = ref<Member[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

const showAddModal = ref(false)
const addForm = ref({ username: '', role: 'viewer' })
const selectedCandidate = ref<Member | undefined>(undefined)
const candidates = ref<Member[]>([])
const candidateLoading = ref(false)
const candidateSearched = ref(false)
const candidateError = ref<string | null>(null)
const addLoading = ref(false)
const addError = ref<string | null>(null)
let candidateTimer: ReturnType<typeof setTimeout> | undefined
let candidateSearchVersion = 0

// 移除成员确认（UModal 取代原生 confirm，i18n/暗色/焦点管理统一）
const showRemoveModal = ref(false)
const memberToRemove = ref<Member | null>(null)
const removeLoading = ref(false)

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

// 候选搜索防抖：username 变化后延迟 250ms 触发 listMemberCandidates；
// version 计数丢弃过期响应，candidateTimer 在卸载/重置时清理。
watch(() => addForm.value.username, (value) => {
  const version = ++candidateSearchVersion
  candidateError.value = null
  candidateSearched.value = false
  candidates.value = []
  if (candidateTimer) clearTimeout(candidateTimer)

  // 输入文本与已选候选不一致时清除选中态，强制用户重新从列表选择，
  // 避免 selectedCandidate 残留旧对象而 username 已变的错配提交。
  if (selectedCandidate.value && selectedCandidate.value.username !== value.trim()) {
    selectedCandidate.value = undefined
  }

  const query = value.trim()
  if (!query || !workspaceId.value || !canManageMembers.value) return

  candidateTimer = setTimeout(async () => {
    candidateLoading.value = true
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

// UInputMenu 的候选条目：显示 username + email，value 取整个 Member 以便提交校验。
const candidateItems = computed<Member[]>(() => candidates.value)

function onCandidateSelect(member: Member | undefined) {
  selectedCandidate.value = member
  if (member) {
    addForm.value.username = member.username
  }
  candidateError.value = null
}

function resetAddForm() {
  candidateSearchVersion++
  if (candidateTimer) clearTimeout(candidateTimer)
  addForm.value = { username: '', role: 'viewer' }
  selectedCandidate.value = undefined
  candidates.value = []
  candidateSearched.value = false
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
    toast.add({ title: t('workspace.memberAdded'), color: 'success' })
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
    toast.add({ title: t('workspace.roleUpdated'), color: 'success' })
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('workspace.updateRoleFailed')
  }
}

function promptRemove(member: Member) {
  memberToRemove.value = member
  showRemoveModal.value = true
}

async function confirmRemove() {
  if (!memberToRemove.value) return
  removeLoading.value = true
  try {
    const api = useWorkspaceApi()
    await api.removeMember(workspaceId.value, memberToRemove.value.user_id)
    members.value = members.value.filter(m => m.user_id !== memberToRemove.value!.user_id)
    showRemoveModal.value = false
    memberToRemove.value = null
    toast.add({ title: t('workspace.memberRemoved'), color: 'success' })
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('workspace.removeMemberFailed')
  } finally {
    removeLoading.value = false
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
                :aria-label="t('workspace.removeMember')"
                @click="promptRemove(member)"
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
              <!-- 候选下拉改用 UInputMenu（combobox）：自带 listbox/方向键/Esc/外点关闭 a11y。
                   :reset-search-term-on-select="false" 是必需的——该 prop 默认 true，
                   选中项时组件会把 searchTerm（绑定 addForm.username）置空，
                   中间态 "" 触发 username watcher 误判「输入与选中不一致」而清掉
                   selectedCandidate，导致提交时报「请先从候选列表选择用户」。 -->
              <UInputMenu
                v-model="selectedCandidate"
                v-model:search-term="addForm.username"
                :items="candidateItems"
                :loading="candidateLoading"
                :placeholder="t('workspace.usernamePlaceholder')"
                :reset-search-term-on-select="false"
                label-key="username"
                class="w-full"
                open-on-focus
                @update:model-value="onCandidateSelect"
              >
                <template #item="{ item }">
                  <span class="flex items-center gap-2 min-w-0">
                    <UAvatar :alt="item.username.charAt(0).toUpperCase()" size="xs" />
                    <span class="min-w-0">
                      <span class="block truncate text-sm font-medium text-highlighted">{{ item.username }}</span>
                      <span class="block truncate text-xs text-muted">{{ item.email }}</span>
                    </span>
                  </span>
                </template>
                <template #empty>
                  <span class="px-3 py-2 text-sm text-muted">
                    {{ candidateSearched ? t('workspace.noCandidateResults') : t('workspace.searchingCandidates') }}
                  </span>
                </template>
              </UInputMenu>
              <ErrorDisplay v-if="candidateError" :message="candidateError" class="mt-2" />
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

    <!-- 移除成员确认 -->
    <UModal v-model:open="showRemoveModal">
      <template #content>
        <div class="p-6">
          <div class="flex items-start gap-3 mb-4">
            <UIcon name="i-lucide-triangle-alert" class="w-6 h-6 text-error flex-shrink-0" />
            <div>
              <h3 class="text-lg font-semibold text-highlighted">{{ t('workspace.removeMember') }}</h3>
              <p class="text-sm text-muted mt-1">
                {{ t('workspace.removeMemberConfirm', { name: memberToRemove?.username ?? '' }) }}
              </p>
            </div>
          </div>
          <div class="flex justify-end gap-2 mt-6">
            <UButton color="neutral" variant="ghost" @click="showRemoveModal = false; memberToRemove = null">{{ t('common.cancel') }}</UButton>
            <UButton color="error" :loading="removeLoading" @click="confirmRemove">{{ t('workspace.removeMember') }}</UButton>
          </div>
        </div>
      </template>
    </UModal>
  </WorkspaceLayout>
</template>
