<!-- pages/workspaces/[id]/members.vue — Workspace Members
//
// 引入动机：design/04-WEB-API.md §页面 要求 Workspace Members 页面。
// 按 RBAC 控制成员管理操作的可见性。
-->
<script setup lang="ts">
import type { Member, ApiError } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'
import { WORKSPACE_ROLES, hasMinRole } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const route = useRoute()
const workspaceId = computed(() => route.params.id as string)

const { workspace, canManageMembers, currentMemberRole } = useWorkspaceContext(workspaceId)

const members = ref<Member[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

const showAddModal = ref(false)
const addForm = ref({ user_id: '', role: 'viewer' })
const addLoading = ref(false)
const addError = ref<string | null>(null)

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

async function handleAdd() {
  if (!addForm.value.user_id) {
    addError.value = t('workspace.userIdRequired')
    return
  }
  addLoading.value = true
  addError.value = null
  try {
    const api = useWorkspaceApi()
    const member = await api.addMember(workspaceId.value, addForm.value)
    members.value.push(member)
    showAddModal.value = false
    addForm.value = { user_id: '', role: 'viewer' }
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
    <div class="max-w-4xl mx-auto px-4 py-8">
      <div class="flex items-center justify-between mb-6">
        <h1 class="text-2xl font-bold text-highlighted">{{ t('workspace.members') }}</h1>
        <UButton
          v-if="canManageMembers"
          icon="i-lucide-plus"
          @click="showAddModal = true"
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
                size="xs"
              >{{ member.role }}</UBadge>
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
            <UFormField :label="t('workspace.userId')" name="user_id">
              <UInput v-model="addForm.user_id" placeholder="UUID" class="w-full" />
            </UFormField>
            <UFormField :label="t('workspace.role')" name="role">
              <USelect v-model="addForm.role" :items="roleOptions" value-key="value" label-key="label" class="w-full" />
            </UFormField>
            <ErrorDisplay v-if="addError" :message="addError" />
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="showAddModal = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="addLoading">{{ t('common.add') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>
  </WorkspaceLayout>
</template>
