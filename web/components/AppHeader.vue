<!-- AppHeader.vue — 顶部导航栏
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 Topbar 包含 workspace/search/user。
// 在 workspace 上下文中显示当前 workspace 名、搜索入口、用户菜单。
//
// Phase 6 i18n：所有用户可见文案使用 t() 翻译，加入可访问语言选择控件。
-->
<script setup lang="ts">
import type { DropdownMenuItem } from '@nuxt/ui'
import type { Workspace } from '~/types/api'

const props = defineProps<{
  workspaceId?: string
  workspaceName?: string
}>()

const { t, locale, locales, setLocale } = useI18n()
const { currentUser, isAuthenticated, isSystemAdmin, clearAuth } = useAuth()
const { systemRoleLabel } = useEnumLabels()
const route = useRoute()
const router = useRouter()

const searchQuery = ref('')

// Workspace switcher：只调用 GET /api/workspaces，切换时清理旧状态并更新路由。
const workspaces = ref<Workspace[]>([])
const wsLoading = ref(false)
const wsError = ref<string | null>(null)

const workspaceItems = computed<DropdownMenuItem[][]>(() => {
  const items: DropdownMenuItem[] = workspaces.value.map(ws => ({
    label: ws.display_name || ws.name,
    onSelect: () => switchWorkspace(ws.id)
  }))
  if (items.length === 0) {
    items.push({ label: t('workspace.noWorkspaces'), type: 'label', disabled: true })
  }
  return [items]
})

async function loadWorkspaces() {
  if (!isAuthenticated.value) {
    workspaces.value = []
    wsError.value = null
    return
  }
  wsLoading.value = true
  wsError.value = null
  try {
    const api = useWorkspaceApi()
    const res = await api.list({ limit: 100 })
    workspaces.value = res.workspaces
  } catch (err) {
    const apiErr = err as { error?: string }
    wsError.value = apiErr.error || t('workspace.loadFailed')
    workspaces.value = []
  } finally {
    wsLoading.value = false
  }
}

function switchWorkspace(id: string) {
  if (id === props.workspaceId) {
    return
  }
  // 清理旧 workspace 状态并更新路由
  searchQuery.value = ''
  router.push(`/workspaces/${id}`)
}

onMounted(() => {
  loadWorkspaces()
})

watch(isAuthenticated, (authed) => {
  if (authed) {
    loadWorkspaces()
  } else {
    workspaces.value = []
    wsError.value = null
  }
})

// 语言切换选项
const localeItems = computed<DropdownMenuItem[][]>(() => {
  const items: DropdownMenuItem[] = (locales.value as Array<{ code: string; name: string }>).map(l => ({
    label: l.name,
    onSelect: () => switchLocale(l.code)
  }))
  return [items]
})

// 当前语言名称
const currentLocaleName = computed(() => {
  const current = (locales.value as Array<{ code: string; name: string }>).find(l => l.code === locale.value)
  return current?.name ?? locale.value
})

// 切换语言并持久化到 cookie
async function switchLocale(code: string) {
  await setLocale(code as 'zh' | 'en')
}

async function handleLogout() {
  try {
    const api = useAuthApi()
    await api.logout()
  } catch {
    // logout 失败也清理前端状态
  }
  clearAuth()
  router.push('/login')
}

function performSearch() {
  if (!props.workspaceId || !searchQuery.value.trim()) return
  router.push(`/workspaces/${props.workspaceId}/search?q=${encodeURIComponent(searchQuery.value.trim())}`)
}

const userItems = computed<DropdownMenuItem[][]>(() => {
  const infoRow: DropdownMenuItem[] = [
    { label: currentUser.value?.username ?? '', slot: 'info', type: 'label', disabled: true }
  ]
  const actions: DropdownMenuItem[] = [
    { label: t('auth.accountSettings'), icon: 'i-lucide-user-cog', to: '/account' },
    { label: t('auth.deviceSessions'), icon: 'i-lucide-key', to: '/device-sessions' }
  ]
  if (isSystemAdmin.value) {
    actions.push({ label: t('admin.adminUsers'), icon: 'i-lucide-users', to: '/admin/users' })
    actions.push({ label: t('admin.adminWorkspaces'), icon: 'i-lucide-folder', to: '/admin/workspaces' })
    actions.push({ label: t('admin.searchProfiles'), icon: 'i-lucide-settings', to: '/admin/search-profiles' })
    actions.push({ label: t('admin.searchEvaluation'), icon: 'i-lucide-bar-chart-3', to: '/admin/search-evaluation' })
    actions.push({ label: t('admin.indexJobs'), icon: 'i-lucide-cog', to: '/admin/jobs' })
    actions.push({ label: t('admin.auditLog'), icon: 'i-lucide-file-text', to: '/admin/audit' })
  }
  const logoutRow: DropdownMenuItem[] = [
    { label: t('auth.logout'), icon: 'i-lucide-log-out', onSelect: handleLogout }
  ]
  return [infoRow, actions, logoutRow]
})
</script>

<template>
  <header class="sticky top-0 z-40 border-b border-default bg-default">
    <div class="flex h-14 items-center justify-between gap-2 px-2 sm:px-4">
      <!-- Left: optional page control + logo + workspace switcher -->
      <div class="flex min-w-0 items-center gap-2 sm:gap-4">
        <slot name="leading" />
        <NuxtLink to="/" class="flex shrink-0 items-center gap-2 font-semibold text-highlighted">
          <UIcon name="i-lucide-book-open" class="w-5 h-5 text-primary" />
          <span class="hidden sm:inline">Partitura</span>
        </NuxtLink>
        <UDropdownMenu :items="workspaceItems">
          <UButton
            color="neutral"
            variant="ghost"
            size="sm"
            icon="i-lucide-folder-open"
            :aria-label="t('workspace.switchWorkspace')"
            :loading="wsLoading"
          >
            <span class="hidden sm:inline max-w-48 truncate">
              {{ workspaceName || t('workspace.switchWorkspace') }}
            </span>
          </UButton>
        </UDropdownMenu>
      </div>

      <!-- Center: Search (only in workspace context) -->
      <div v-if="workspaceId" class="mx-2 hidden max-w-md flex-1 sm:block lg:mx-4">
        <UInput
          v-model="searchQuery"
          :placeholder="t('search.searchInWorkspace')"
          icon="i-lucide-search"
          class="w-full"
          @keyup.enter="performSearch"
        />
      </div>
      <UButton
        v-if="workspaceId"
        class="sm:hidden"
        color="neutral"
        variant="ghost"
        icon="i-lucide-search"
        :aria-label="t('common.search')"
        :to="`/workspaces/${workspaceId}/search`"
      />

      <!-- Right: Language switcher + User menu -->
      <div class="flex items-center gap-2">
        <!-- Language switcher -->
        <UDropdownMenu :items="localeItems">
          <UButton
            color="neutral"
            variant="ghost"
            size="sm"
            icon="i-lucide-languages"
            :aria-label="t('common.language')"
          >
            <span class="text-xs hidden sm:inline">{{ currentLocaleName }}</span>
          </UButton>
        </UDropdownMenu>

        <UDropdownMenu :items="userItems">
          <UButton
            color="neutral"
            variant="ghost"
            class="flex items-center gap-2"
          >
            <UAvatar :alt="currentUser?.username?.charAt(0)?.toUpperCase()" size="xs" />
            <span class="text-sm hidden sm:inline">{{ currentUser?.username }}</span>
            <UIcon name="i-lucide-chevron-down" class="w-4 h-4" />
          </UButton>
          <template #info="{ item }">
            <div class="py-1">
              <p class="font-medium text-highlighted">{{ currentUser?.username }}</p>
              <p class="text-xs text-muted">{{ systemRoleLabel(currentUser?.system_role) }}</p>
            </div>
          </template>
        </UDropdownMenu>
      </div>
    </div>
  </header>
</template>
