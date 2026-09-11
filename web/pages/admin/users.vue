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
const { isSystemAdmin, currentUser } = useAuth()
const { systemRoleLabel } = useEnumLabels()

const users = ref<AdminUser[]>([])
const total = ref(0)
const limit = ref(20)
const offset = ref(0)
const loading = ref(false)
const error = ref<string | null>(null)

// 服务端筛选：后端 /admin/users 支持 system_role + q 查询参数
const searchQuery = ref('')
const roleFilter = ref('')

function applyFilters() {
  offset.value = 0
  loadUsers()
}

const editingUser = ref<AdminUser | null>(null)
const isEditModalOpen = computed({
  get: () => editingUser.value !== null,
  set: (val: boolean) => { if (!val) editingUser.value = null }
})
const editForm = reactive({ system_role: '', workspace_create_perm: false })
const editLoading = ref(false)
const editError = ref<string | null>(null)

/**
 * 角色变更确认：自我降级（自己把 system_admin 降级）或提权为 system_admin 都属于危险操作，
 * 在保存前通过 UModal 二次确认，防止误操作导致失管或越权。
 */
const roleConfirmOpen = ref(false)
const roleConfirmCopy = ref({ title: '', body: '' })
const roleConfirmLoading = ref(false)

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
    const res = await api.listUsers({
      limit: limit.value,
      offset: offset.value,
      system_role: roleFilter.value || undefined,
      q: searchQuery.value.trim() || undefined
    })
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

// ---- 键盘导航：j/k 上下移动选中行，Enter/o 打开编辑，Escape 清除选中 ----
const selectedIndex = ref(-1)
const tableEl = ref<HTMLElement | null>(null)

// 数据变化时清选中，避免指向已不存在的行。
watch(users, () => { selectedIndex.value = -1 })

function moveSelection(delta: number) {
  if (users.value.length === 0) return
  const next = selectedIndex.value < 0
    ? (delta > 0 ? 0 : users.value.length - 1)
    : Math.min(Math.max(selectedIndex.value + delta, 0), users.value.length - 1)
  selectedIndex.value = next
  tableEl.value?.querySelectorAll('tbody tr')[next]?.scrollIntoView({ block: 'nearest' })
}

function openSelected() {
  const user = users.value[selectedIndex.value]
  if (user) openEdit(user)
}

function clearSelection() {
  selectedIndex.value = -1
}

useHotkey('j', () => moveSelection(1))
useHotkey('k', () => moveSelection(-1))
useHotkey('enter', openSelected)
useHotkey('o', openSelected)
useHotkey('escape', clearSelection)

/**
 * 提交编辑前的角色变更风险检查。
 * 返回 true 表示需要二次确认（此时已打开确认弹窗）；false 表示可直接提交。
 */
function requestSaveWithRoleCheck(): void {
  const user = editingUser.value
  if (!user) return
  const isSelf = currentUser.value?.id === user.id
  const demotingSelf = isSelf && user.system_role === 'system_admin' && editForm.system_role !== 'system_admin'
  const promotingToAdmin = user.system_role !== 'system_admin' && editForm.system_role === 'system_admin'

  if (demotingSelf) {
    roleConfirmCopy.value = { title: t('admin.systemRole'), body: t('admin.demoteSelfConfirm') }
    roleConfirmOpen.value = true
    return
  }
  if (promotingToAdmin) {
    roleConfirmCopy.value = { title: t('admin.systemRole'), body: t('admin.promoteToAdminConfirm', { username: user.username }) }
    roleConfirmOpen.value = true
    return
  }
  // 无角色风险：直接提交
  void doSaveEdit()
}

/** 确认弹窗确认后执行真实保存。 */
async function confirmRoleChange() {
  roleConfirmOpen.value = false
  await doSaveEdit()
}

async function doSaveEdit() {
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

const roleFilterOptions = computed<SelectItem[]>(() => [
  { label: t('admin.all'), value: '' },
  { label: t('admin.roleUser'), value: 'user' },
  { label: t('admin.roleSystemAdmin'), value: 'system_admin' }
])

useHead({ title: () => t('admin.adminUsers') + ' · ' + t('common.appName') })
</script>

<template>
  <div>
    <!-- 顶栏由 layouts/default.vue 统一注入 -->
    <div class="max-w-6xl mx-auto px-4 py-8">

      <EmptyState
        v-if="!isSystemAdmin"
        icon="i-lucide-lock"
        :title="t('admin.systemAdminRequired')"
      />

      <template v-else>
        <div class="flex items-center justify-between mb-4">
          <h1 class="text-2xl font-bold text-highlighted">{{ t('admin.adminUsers') }}</h1>
          <UButton icon="i-lucide-user-plus" @click="openCreateUser">{{ t('admin.createUser') }}</UButton>
        </div>

        <ErrorDisplay v-if="error" :message="error" />

        <div class="flex gap-2 mb-4">
          <UInput
            v-model="searchQuery"
            :placeholder="t('admin.searchUsersPlaceholder')"
            icon="i-lucide-search"
            class="w-64"
            @keyup.enter="applyFilters"
          />
          <USelect
            v-model="roleFilter"
            :items="roleFilterOptions"
            value-key="value"
            label-key="label"
            class="w-40"
            :aria-label="t('admin.filterByRole')"
            @update:model-value="applyFilters"
          />
          <UButton size="sm" variant="outline" icon="i-lucide-search" @click="applyFilters">{{ t('common.search') }}</UButton>
        </div>

        <div v-if="loading" class="flex justify-center py-8" role="status">
          <UIcon name="i-lucide-loader-circle" aria-hidden="true" class="w-8 h-8 animate-spin text-muted" />
          <span class="sr-only">{{ t('common.loading') }}</span>
        </div>

        <UCard v-else-if="users.length > 0" class="overflow-x-auto">
          <table class="w-full text-sm" ref="tableEl">
            <thead>
              <tr class="text-left text-muted border-b border-default">
                <th class="py-2 pr-3 font-medium">{{ t('auth.username') }}</th>
                <th class="py-2 pr-3 font-medium">{{ t('admin.systemRole') }}</th>
                <th class="py-2 pr-3 font-medium sr-only">{{ t('common.edit') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(user, idx) in users"
                :key="user.id"
                class="border-b border-default last:border-0 transition-colors"
                :class="idx === selectedIndex ? 'bg-elevated' : ''"
                :aria-selected="idx === selectedIndex"
              >
                <td class="py-2 pr-3">
                  <div class="flex items-center gap-3">
                    <UAvatar :alt="user.username.charAt(0).toUpperCase()" size="sm" />
                    <div>
                      <p class="text-sm font-medium text-highlighted">{{ user.username }}</p>
                      <p class="text-xs text-muted">{{ user.email }}</p>
                    </div>
                  </div>
                </td>
                <td class="py-2 pr-3">
                  <UBadge :color="user.system_role === 'system_admin' ? 'error' : 'neutral'" variant="subtle" size="sm">{{ systemRoleLabel(user.system_role) }}</UBadge>
                  <UBadge v-if="user.workspace_create_perm" color="success" variant="subtle" size="sm" class="ml-1">{{ t('admin.workspaceCreateEnabled') }}</UBadge>
                </td>
                <td class="py-2">
                  <UButton size="xs" variant="ghost" icon="i-lucide-pencil" :aria-label="t('common.edit')" @click="openEdit(user)" />
                </td>
              </tr>
            </tbody>
          </table>
        </UCard>

        <EmptyState v-else icon="i-lucide-users" :title="t('admin.noUsers')" />

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
          <form @submit.prevent="requestSaveWithRoleCheck" class="space-y-4">
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

    <!-- 角色变更确认弹窗（自我降级 / 提权为 system_admin） -->
    <UModal v-model:open="roleConfirmOpen">
      <template #content>
        <div class="p-6">
          <h3 class="text-lg font-semibold mb-2">{{ roleConfirmCopy.title }}</h3>
          <p class="text-sm text-muted mb-4">{{ roleConfirmCopy.body }}</p>
          <div class="flex justify-end gap-2">
            <UButton color="neutral" variant="ghost" @click="roleConfirmOpen = false">{{ t('common.cancel') }}</UButton>
            <UButton color="warning" :loading="roleConfirmLoading" @click="confirmRoleChange">{{ t('common.confirm') }}</UButton>
          </div>
        </div>
      </template>
    </UModal>
  </div>
</template>
