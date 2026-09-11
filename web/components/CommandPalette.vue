<!-- CommandPalette.vue — 全局命令面板 + 快捷键帮助
//
// 引入动机：UI/UX 优化阶段提供 GitHub/Linear 风格的命令面板（Ctrl+K），
// 让用户无需鼠标即可跳转页面、切换工作区、新建文档、切语言、登出；
// 同时提供 `?` 快捷键帮助面板提升可发现性与键盘可访问性。
//
// 结构：
// - UModal 包裹 UCommandPalette：UModal 提供 overlay/焦点圈定/Esc 关闭，
//   UCommandPalette 提供搜索输入 + listbox + 方向键导航。
// - 第二个 UModal 提供 `?` 快捷键帮助面板（用 UCommandPalette 不适合静态表格，
//   用普通列表 + UKbd 渲染更清晰）。
//
// 数据：
// - workspace 列表来自 useWorkspaceApi().list（打开面板时拉取，limit 100）。
// - 静态命令分组（跳转/新建/账户与系统）始终可用；工作区分组仅在登录且有
//   workspace 时出现；当前 workspace 内的"新建文档/搜索/成员/设置"命令仅在
//   路由处于 workspace 上下文（route.params.id 存在）时出现。
//
// 全局快捷键（在 default.vue 挂载时注册，组件卸载自动注销）：
// - mod+k / ctrl+k：打开命令面板
// - /：聚焦当前 workspace 顶栏搜索框（仅 workspace 上下文）
// - ?：打开快捷键帮助面板
// - Escape：由 UModal/浏览器原生处理关闭
//
// 可访问性：UCommandPalette 自带 listbox 语义与方向键导航；
// 这里为模态框补充 aria-label。
-->
<script setup lang="ts">
import type { CommandPaletteGroup, CommandPaletteItem } from '@nuxt/ui'
import type { Workspace } from '~/types/api'

const { t, locales, setLocale } = useI18n()
const route = useRoute()
const router = useRouter()
const { isAuthenticated, clearAuth } = useAuth()

const open = ref(false)
const helpOpen = ref(false)
const workspaces = ref<Workspace[]>([])
const workspacesLoading = ref(false)

// 当前路由所处的 workspace id（workspace scoped 页面为 route.params.id）。
const workspaceId = computed(() => (route.params.id as string | undefined) ?? undefined)

// 拉取 workspace 列表（面板打开时调用；登录才拉，登出/未登录保持空）。
async function loadWorkspaces() {
  if (!isAuthenticated.value) {
    workspaces.value = []
    return
  }
  workspacesLoading.value = true
  try {
    const res = await useWorkspaceApi().list({ limit: 100 })
    workspaces.value = res.workspaces
  } catch (err) {
    // 面板数据加载失败不阻塞面板使用：置空列表 + 控制台可见（不静默吞掉）。
    workspaces.value = []
    if (import.meta.dev) {
      console.error('[CommandPalette] 加载工作区列表失败', err)
    }
  } finally {
    workspacesLoading.value = false
  }
}

function openPalette() {
  open.value = true
  loadWorkspaces()
}

function openHelp() {
  helpOpen.value = true
}

// 聚焦顶栏 workspace 搜索框（`/` 快捷键）。
// 顶栏 UInput 渲染为 input 元素；通过限定在 header 内查找避免误聚焦其它输入框。
function focusHeaderSearch() {
  if (typeof document === 'undefined') return
  const input = document.querySelector<HTMLElement>('header input')
  input?.focus()
}

async function handleLogout() {
  try {
    await useAuthApi().logout()
  } catch (err) {
    // 登出 API 失败仍清理前端状态，但记录日志（不静默吞掉）。
    if (import.meta.dev) {
      console.error('[CommandPalette] logout 接口失败，继续清理本地认证态', err)
    }
  }
  clearAuth()
  open.value = false
  router.push('/login')
}

function go(path: string) {
  open.value = false
  router.push(path)
}

async function switchLocale(code: string) {
  open.value = false
  await setLocale(code as 'zh' | 'en')
}

// ---- 命令分组 ----
const groups = computed<CommandPaletteGroup<CommandPaletteItem>[]>(() => {
  const result: CommandPaletteGroup<CommandPaletteItem>[] = []

  // 1. 当前 workspace 上下文命令（仅 workspace 页面）。
  if (workspaceId.value) {
    const id = workspaceId.value
    result.push({
      id: 'workspace',
      label: t('palette.groupWorkspace'),
      items: [
        { label: t('palette.newDocument'), icon: 'i-lucide-file-plus', kbds: ['n'], onSelect: () => go(`/workspaces/${id}/documents/edit`) },
        { label: t('palette.searchWorkspace'), icon: 'i-lucide-search', kbds: ['/'], onSelect: () => go(`/workspaces/${id}/search`) },
        { label: t('palette.members'), icon: 'i-lucide-users', onSelect: () => go(`/workspaces/${id}/members`) },
        { label: t('palette.workspaceSettings'), icon: 'i-lucide-settings', onSelect: () => go(`/workspaces/${id}/settings`) }
      ]
    })
  }

  // 2. 跳转导航（始终可用）。
  result.push({
    id: 'navigate',
    label: t('palette.groupNavigate'),
    items: [
      { label: t('palette.goHome'), icon: 'i-lucide-home', kbds: ['g', 'h'], onSelect: () => go('/') },
      { label: t('palette.goWorkspaces'), icon: 'i-lucide-folder', onSelect: () => go('/') },
      { label: t('palette.account'), icon: 'i-lucide-user-cog', onSelect: () => go('/account') },
      { label: t('palette.deviceSessions'), icon: 'i-lucide-key', onSelect: () => go('/device-sessions') },
      { label: t('palette.systemHealth'), icon: 'i-lucide-activity', onSelect: () => go('/system-health') }
    ]
  })

  // 3. 切换工作区（仅登录且有 workspace）。
  if (workspaces.value.length > 0) {
    result.push({
      id: 'workspaces',
      label: t('palette.groupSwitchWorkspace'),
      items: workspaces.value.map(ws => ({
        label: ws.display_name || ws.name,
        description: ws.name,
        icon: 'i-lucide-folder-open',
        onSelect: () => go(`/workspaces/${ws.id}`)
      }))
    })
  }

  // 4. 语言切换。
  result.push({
    id: 'language',
    label: t('palette.groupLanguage'),
    items: ((locales.value ?? []) as Array<{ code: string; name: string }>).map(l => ({
      label: l.name,
      icon: 'i-lucide-languages',
      onSelect: () => switchLocale(l.code)
    }))
  })

  // 5. 账户与系统。
  result.push({
    id: 'account',
    label: t('palette.groupAccount'),
    items: [
      { label: t('palette.showShortcuts'), icon: 'i-lucide-keyboard', kbds: ['?'], onSelect: () => { open.value = false; openHelp() } },
      { label: t('palette.logout'), icon: 'i-lucide-log-out', onSelect: handleLogout }
    ]
  })

  return result
})

// ---- 注册全局快捷键 ----
// 组件卸载（布局销毁）自动注销；组合键在输入框中仍生效（allowInInput 默认 false，
// 但带 mod 修饰键的组合本来就在输入框生效——见 useHotkeys 的抑制规则）。
useHotkeyWithHelp('mod+k', () => { openPalette() }, t('palette.openCommandPalette'))
useHotkeyWithHelp('ctrl+k', () => { openPalette() }, t('palette.openCommandPalette'))
// `/` 聚焦搜索：仅在 workspace 上下文（有顶栏搜索框）且非输入框时生效。
useHotkeyWithHelp('/', () => { focusHeaderSearch() }, t('palette.focusSearch'))
// `?` = shift+/，在 combo 中直接写 "?"（shift 由浏览器产生，e.key 已是 '?'）。
useHotkeyWithHelp('?', () => { openHelp() }, t('palette.showShortcuts'))
// g h 双键序列：先 g 后 h 回首页。
useHotkeyWithHelp('g h', () => { go('/') }, t('palette.goHome'))

// 帮助面板条目：来自 useHotkeys 帮助清单（helpRegistry 为模块内非响应式 Map，
// 故用 helpOpen 作为响应式依赖——面板每次打开时重新取快照）。
const helpItems = computed(() => (helpOpen.value, getHotkeysHelp()))

// 把 combo 串拆为 UKbd 的展示 token：修饰键 "mod" 映射为 "meta"（UKbd 内部
// 按平台渲染为 ⌘/Ctrl），序列键按空格拆分。如 "mod+k"→["meta","k"]、"g h"→["g","h"]。
function comboToKbds(combo: string): string[] {
  return combo.split(/[\s+]+/).filter(Boolean).map(tok => (tok === 'mod' ? 'meta' : tok))
}
</script>

<template>
  <!-- 命令面板模态框 -->
  <UModal
    v-model:open="open"
    :title="t('common.commandPalette')"
    :ui="{ content: 'sm:max-w-xl' }"
  >
    <template #content>
      <UCommandPalette
        :groups="groups"
        :loading="workspacesLoading"
        :placeholder="t('palette.placeholder')"
        :aria-label="t('common.commandPalette')"
        close
        class="h-96"
        @update:open="open = $event"
      />
    </template>
  </UModal>

  <!-- 快捷键帮助面板 -->
  <UModal
    v-model:open="helpOpen"
    :title="t('common.shortcuts')"
    :ui="{ content: 'sm:max-w-md' }"
  >
    <template #content>
      <div class="p-5" :aria-label="t('common.shortcuts')">
        <ul class="space-y-2">
          <li
            v-for="item in helpItems"
            :key="item.combo"
            class="flex items-center justify-between gap-4 text-sm"
          >
            <span class="text-muted">{{ item.description }}</span>
            <span class="flex shrink-0 items-center gap-1">
              <UKbd v-for="k in comboToKbds(item.combo)" :key="k" :value="k" />
            </span>
          </li>
        </ul>
      </div>
    </template>
  </UModal>
</template>
