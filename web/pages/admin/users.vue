<!-- pages/admin/users.vue — Admin Users
//
// 引入动机：design/04-WEB-API.md §页面 要求 Admin Users 页面。
// 仅 system_admin 可访问。
-->
<script setup lang="ts">
import type { AdminUser, ApiError, CreateUserRequest } from '~/types/api'
import type { SelectItem } from '@nuxt/ui'

definePageMeta({
  middleware: ['auth', 'admin']
})

const { t } = useI18n()
const { isSystemAdmin } = useAuth()
const { systemRoleLabel } = useEnumLabels()

const users = ref<AdminUser[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

const editingUser = ref<AdminUser | null>(null)
const isEditModalOpen = computed({
  get: () => editingUser.value !== null,
  set: (val: boolean) => { if (!val) editingUser.value = null }
})
const editForm = reactive({ system_role: '', workspace_create_perm: false })
const editLoading = ref(false)
const editError = ref<string | null>(null)

// --- 创建用户状态 ---
const isCreateModalOpen = ref(false)
const createForm = reactive({
  username: '',
  email: '',
  password: '',
  workspace_create_perm: false
})
const createLoading = ref(false)
const createError = ref<string | null>(null)

function openCreateUser() {
  createForm.username = ''
  createForm.email = ''
  createForm.password = ''
  createForm.workspace_create_perm = false
  createError.value = null
  isCreateModalOpen.value = true
}

async function handleCreateUser() {
  // 客户端输入校验
  if (!createForm.username.trim()) {
    createError.value = t('admin.usernameRequired')
    return
  }
  if (createForm.username.length > 100) {
    createError.value = t('admin.usernameTooLong')
    return
  }
  if (!createForm.email.trim() || !createForm.email.includes('@')) {
    createError.value = t('admin.emailInvalid')
    return
  }
  if (createForm.password.length < 8) {
    createError.value = t('admin.passwordTooShort')
    return
  }

  createLoading.value = true
  createError.value = null
  try {
    const api = useAdminApi()
    const req: CreateUserRequest = {
      username: createForm.username.trim(),
      email: createForm.email.trim(),
      password: createForm.password,
      workspace_create_perm: createForm.workspace_create_perm
    }
    await api.createUser(req)
    // 成功后清空密码输入并刷新用户列表
    createForm.password = ''
    isCreateModalOpen.value = false
    await loadUsers()
  } catch (err) {
    const apiErr = err as ApiError
    createError.value = apiErr.error || t('admin.createUserFailed')
  } finally {
    createLoading.value = false
  }
}

async function loadUsers() {
  if (!isSystemAdmin.value) return
  loading.value = true
  error.value = null
  try {
    const api = useAdminApi()
    const res = await api.listUsers({ limit: limit.value, offset: offset.value })
    users.value = res.users
    total.value = res.total
  } catch (err) {
    const apiErr = err as ApiError
    error.value = apiErr.error || t('admin.loadUsersFailed')
  } finally {
    loading.value = false
  }
}

onMounted(loadUsers)

function handleOffsetChange(newOffset: number) {
  offset.value = newOffset
  loadUsers()
}

function openEdit(user: AdminUser) {
  editingUser.value = user
  editForm.system_role = user.system_role
  editForm.workspace_create_perm = user.workspace_create_perm
  editError.value = null
}

async function handleSaveEdit() {
  if (!editingUser.value) return
  editLoading.value = true
  editError.value = null
  try {
    const api = useAdminApi()
    const updated = await api.updateUser(editingUser.value.id, {
      system_role: editForm.system_role,
      workspace_create_perm: editForm.workspace_create_perm
    })
    const idx = users.value.findIndex(u => u.id === editingUser.value!.id)
    if (idx >= 0) users.value[idx] = updated
    editingUser.value = null
  } catch (err) {
    const apiErr = err as ApiError
    editError.value = apiErr.error || t('admin.updateUserFailed')
  } finally {
    editLoading.value = false
  }
}

const roleOptions = computed<SelectItem[]>(() => [
  { label: t('admin.roleUser'), value: 'user' },
  { label: t('admin.roleSystemAdmin'), value: 'system_admin' }
])

useHead({ title: () => t('admin.adminUsers') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <AppHeader />

    <div class="max-w-4xl mx-auto px-4 py-8">

      <div v-if="!isSystemAdmin" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t('admin.systemAdminRequired') }}</p>
      </div>

      <template v-else>
        <div class="flex items-center justify-between mb-4">
          <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.adminUsers') }}</h1>
          <UButton icon="i-lucide-user-plus" @click="openCreateUser">{{ t('admin.createUser') }}</UButton>
        </div>

        <ErrorDisplay v-if="error" :message="error" />

        <div v-if="loading" class="flex justify-center py-8">
          <UIcon name="i-lucide-loader-circle" class="w-8 h-8 animate-spin text-muted" />
        </div>

        <UCard v-else-if="users.length > 0">
          <div class="space-y-2">
            <div
              v-for="user in users"
              :key="user.id"
              class="flex items-center justify-between border border-default rounded-lg p-3"
            >
              <div class="flex items-center gap-3">
                <UAvatar :alt="user.username.charAt(0).toUpperCase()" size="sm" />
                <div>
                  <p class="text-sm font-medium text-highlighted">{{ user.username }}</p>
                  <p class="text-xs text-muted">{{ user.email }}</p>
                </div>
              </div>
              <div class="flex items-center gap-2">
                <UBadge :color="user.system_role === 'system_admin' ? 'error' : 'neutral'" variant="subtle" size="sm">{{ systemRoleLabel(user.system_role) }}</UBadge>
                <UBadge v-if="user.workspace_create_perm" color="success" variant="subtle" size="sm">{{ t('admin.workspaceCreateEnabled') }}</UBadge>
                <UButton size="xs" variant="ghost" icon="i-lucide-pencil" @click="openEdit(user)" />
              </div>
            </div>
          </div>
        </UCard>

        <p v-else class="text-center text-muted py-8">{{ t('admin.noUsers') }}</p>

        <Pagination
          v-if="total > limit"
          :total="total"
          :limit="limit"
          :offset="offset"
          @update:offset="handleOffsetChange"
        />
      </template>
    </div>

    <!-- Edit User Modal -->
    <UModal v-model:open="isEditModalOpen">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.editUser', { username: editingUser?.username }) }}</h3>
          <form @submit.prevent="handleSaveEdit" class="space-y-4">
            <UFormField :label="t('admin.systemRole')" name="system_role">
              <USelect
                v-model="editForm.system_role"
                :items="roleOptions"
                value-key="value"
                label-key="label"
                class="w-full"
              />
            </UFormField>
            <UFormField :label="t('admin.workspaceCreatePerm')" name="workspace_create_perm">
              <USwitch v-model="editForm.workspace_create_perm" />
            </UFormField>
            <ErrorDisplay v-if="editError" :message="editError" />
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="editingUser = null">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="editLoading">{{ t('common.save') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>

    <!-- Create User Modal -->
    <UModal v-model:open="isCreateModalOpen">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-4">{{ t('admin.createUser') }}</h3>
          <form @submit.prevent="handleCreateUser" class="space-y-4">
            <UFormField :label="t('auth.username')" name="username" required>
              <UInput
                v-model="createForm.username"
                :placeholder="t('auth.enterUsername')"
                class="w-full"
                maxlength="100"
              />
            </UFormField>
            <UFormField :label="t('admin.email')" name="email" required>
              <UInput
                v-model="createForm.email"
                type="email"
                :placeholder="t('admin.enterEmail')"
                class="w-full"
                maxlength="255"
              />
            </UFormField>
            <UFormField :label="t('auth.password')" name="password" required>
              <UInput
                v-model="createForm.password"
                type="password"
                :placeholder="t('admin.enterPassword')"
                class="w-full"
              />
            </UFormField>
            <UFormField :label="t('admin.workspaceCreatePerm')" name="workspace_create_perm">
              <USwitch v-model="createForm.workspace_create_perm" />
            </UFormField>
            <ErrorDisplay v-if="createError" :message="createError" />
            <div class="flex justify-end gap-2 pt-2">
              <UButton color="neutral" variant="ghost" @click="isCreateModalOpen = false">{{ t('common.cancel') }}</UButton>
              <UButton type="submit" :loading="createLoading" icon="i-lucide-user-plus">{{ t('common.create') }}</UButton>
            </div>
          </form>
        </div>
      </template>
    </UModal>
  </div>
</template>
