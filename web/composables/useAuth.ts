// composables/useAuth.ts — 认证状态管理
//
// 引入动机：design/04-WEB-API.md §Security 要求 secure cookie 认证。
// 认证状态基于服务端 session cookie（HttpOnly），前端通过 login 获取用户信息，
// 通过 GET /api/auth/me 做权威验证，通过 API 401 响应判断 session 过期。
//
// 安全：
// - 不将 password/session/access/refresh token 存入 localStorage/sessionStorage/URL/日志
// - pkw_user cookie 只保存非敏感显示数据（username/avatar 占位），客户端可写，
//   绝不作为授权依据——授权判断一律以服务端验证结果 verifiedUser 为准
// - 认证状态通过 API 请求结果验证，不依赖前端可篡改的状态
//
// pkw_user 的 secure 契约（与后端 session cookie 对齐）：
// - 后端 session cookie 的 Secure 由部署期变量 COOKIE_SECURE 控制（server config 默认 true）。
// - 前后端同源、经同一 Nginx 反代，浏览器能否存入这两个 cookie 由同一传输条件决定。
// - 因此 pkw_user 的 Secure 不能依赖构建期值（import.meta.dev），必须按页面运行时
//   协议决定：window.location.protocol === 'https:' 时才带 Secure。HTTP（含 dev
//   localhost 与部署方主动关闭 COOKIE_SECURE 的 HTTP 内网）一律不写 Secure，保证
//   pkw_user 与 session cookie 同生同灭，登录显示态与真实会话一致。
// - 实现上：useCookie 仅用于读取/hydration（readonly，secure 定型于创建时无法做
//   运行时判定），实际写盘统一走 writeAuthCookie/expireAuthCookie 手动 document.cookie。
//
// 模块级状态说明（动机）：
// useState/useCookie/useRuntimeConfig 等 Nuxt composable 只能在 Nuxt 上下文
// （setup/middleware/事件处理）中调用；apiFetch 的 401 处理发生在异步回调里，
// 此时调用 useAuth() 会丢失 Nuxt 上下文。因此把权威态 verifiedUser、显示态
// displayUser、me() 探测标记 authChecked 提升为模块级 ref，useApi.ts 的
// handleUnauthorized() 可以脱离 Nuxt 上下文安全操作它们。
// cookie 的读写仍集中在 setAuth/setCurrentUser/clearAuth（这些在 setup/事件
// 上下文中被调用）；所有写盘路径（含 clearAuthState 的异步清理）统一收敛到
// writeAuthCookie/expireAuthCookie，保证 SameSite/Path/Max-Age/Secure 一致。

import type { CurrentUserResponse, LoginResponse } from '~/types/api'
import { apiFetch } from '~/composables/useApi'

/** 认证用户信息（非敏感显示数据） */
export interface AuthUser {
  id: string
  username: string
  email?: string
  system_role: string
  workspace_create_perm?: boolean
}

const AUTH_COOKIE_NAME = 'pkw_user'

/** pkw_user 显示态 cookie 的有效期（秒），与后端 session 生命周期对齐 */
const AUTH_COOKIE_MAX_AGE = 86400

/**
 * 权威认证态：仅由服务端响应写入（login /auth/me），是 isAuthenticated/isSystemAdmin 的判据。
 * 模块级导出供 useApi.ts（401 清理）与 useWorkspace.ts（登出清缓存）在异步回调中安全访问，
 * 不依赖 useAuth() 的 Nuxt 上下文。
 */
export const verifiedUser = ref<AuthUser | null>(null)

/** 显示态占位：来自可读写 cookie，仅用于 hydration 期间显示用户名，不作授权依据 */
const displayUser = ref<AuthUser | null>(null)

/** me() 是否已完成探测：false 表示尚未向服务端确认过会话（此时允许 cookie 占位） */
const authChecked = ref(false)

/** 并发 refreshAuth() 去重：多个守卫/调用方同时验证时共享同一个 me() 请求 */
let refreshInflight: Promise<AuthUser | null> | null = null

/**
 * 写盘辅助：手动 document.cookie 写入 pkw_user。
 *
 * 为何不用 useCookie 写：useCookie 的 secure 选项在创建时定型为静态布尔值，
 * 无法接受 Ref/computed 做运行时判定。为了让 Secure 严格跟随页面运行时协议，
 * 写盘统一收敛到本函数手动拼接。
 *
 * 序列化格式与 cookie-es/useCookie 一致：value 经 encodeURIComponent(JSON.stringify)，
 * 属性固定 SameSite=Lax; Path=/; Max-Age=86400，Secure 仅在 https 页面附加。
 *
 * 引入动机（见文件头契约）：Secure 必须匹配后端 session cookie 的传输条件。
 * 前后端同源经同一 Nginx 反代，HTTPS 页面下 session cookie 才能带 Secure；
 * 此函数按 window.location.protocol 运行时判定，替代构建期 import.meta.dev，
 * 消除"构建期值 vs 部署期 COOKIE_SECURE"的错配。
 */
function writeAuthCookie(user: AuthUser): void {
  if (typeof document === 'undefined') return
  const value = encodeURIComponent(JSON.stringify(user))
  // 仅 HTTPS 页面才标记 Secure；HTTP（dev localhost / 内网部署）不写，
  // 否则浏览器会拒存此 cookie，与后端 session cookie 的传输条件错配。
  const secure = typeof window !== 'undefined' && window.location.protocol === 'https:' ? '; Secure' : ''
  document.cookie =
    `${AUTH_COOKIE_NAME}=${value}; Path=/; Max-Age=${AUTH_COOKIE_MAX_AGE}; SameSite=Lax${secure}`
}

/** 通过 document.cookie 强制清除 pkw_user（不依赖 useCookie/Nuxt 上下文） */
function expireAuthCookie(): void {
  if (typeof document !== 'undefined') {
    // 清除不附加 Secure（secure 只影响写/匹配存储，与过期语义无关），
    // Path 必须与写入时一致才能命中并删除同一条目。
    document.cookie = `${AUTH_COOKIE_NAME}=; Path=/; Max-Age=0; SameSite=Lax`
  }
}

/**
 * 模块级清理：清空权威态/显示态/探测标记并清除 pkw_user cookie。
 * 供 useApi.ts 的 handleUnauthorized() 在异步回调中调用（无 Nuxt 上下文）。
 * authChecked 置为 true 表示"会话已被服务端判定为无效"，禁止 cookie 占位再次生效。
 */
export function clearAuthState(): void {
  verifiedUser.value = null
  displayUser.value = null
  authChecked.value = true
  expireAuthCookie()
}

/**
 * useAuth — 全局认证状态 composable
 *
 * 行为：
 * - verifiedUser（模块级）为唯一权威认证态，由 login/refreshAuth 写入
 * - displayUser + pkw_user cookie 仅作刷新期间的显示占位（username 渲染）
 * - isAuthenticated/isSystemAdmin 优先依据 verifiedUser；仅在 authChecked=false
 *   （尚未调用过 me()）时临时回退到 cookie 占位，受保护路由守卫必须先 refreshAuth()
 */
export function useAuth() {
  // useCookie 仅作读取/hydration 源：readonly + watch:false，禁用其对 document.cookie
  // 的写盘副作用（含 scope dispose 时的 flush），实际写盘统一走 writeAuthCookie。
  // 不传 secure：该选项只影响 useCookie 自身序列化写盘（此处已禁用），读取
  // document.cookie 不受 secure 限制——浏览器对 https 页面写入的 Secure cookie 在
  // 同源下仍可读，故读取端无需也无需关心 secure 标志。
  const userCookie = useCookie<AuthUser | null>(AUTH_COOKIE_NAME, {
    readonly: true,
    watch: false,
    // decode 与默认一致（JSON.parse），保证读到的是对象而非字符串
    decode: (val) => (val ? (JSON.parse(decodeURIComponent(val)) as AuthUser) : (val as null))
  })

  // cookie → displayUser 的 hydration：仅用于页面刷新后服务端验证完成前的占位显示。
  if (!displayUser.value && userCookie.value) {
    displayUser.value = userCookie.value
  }

  // 授权判据：已验证的权威态。仅当 me() 尚未探测（authChecked=false）时，
  // 允许用可伪造的 cookie 占位做"看起来像已登录"的显示态，受保护路由会先 refreshAuth 确认真伪。
  const effectiveUser = computed<AuthUser | null>(() =>
    verifiedUser.value ?? (authChecked.value ? null : displayUser.value)
  )
  const isAuthenticated = computed(() => effectiveUser.value !== null)
  const isSystemAdmin = computed(() => effectiveUser.value?.system_role === 'system_admin')
  const currentUser = computed(() => effectiveUser.value)

  /** 设置认证状态（login 成功后调用）。登录响应即权威来源，同时写 verifiedUser。 */
  function setAuth(loginResponse: LoginResponse) {
    const user: AuthUser = {
      id: loginResponse.user.id,
      username: loginResponse.user.username,
      system_role: loginResponse.user.system_role
    }
    verifiedUser.value = user
    displayUser.value = user
    authChecked.value = true
    writeAuthCookie(user)
  }

  /** 用 /auth/me 的权威响应刷新当前用户状态（verifiedUser + cookie 占位）。 */
  function setCurrentUser(current: CurrentUserResponse) {
    const user: AuthUser = {
      id: current.id,
      username: current.username,
      email: current.email,
      system_role: current.system_role,
      workspace_create_perm: current.workspace_create_perm
    }
    verifiedUser.value = user
    displayUser.value = user
    authChecked.value = true
    writeAuthCookie(user)
  }

  /** 清理认证状态（logout 或 401 时调用） */
  function clearAuth() {
    clearAuthState()
  }

  /**
   * refreshAuth — 通过 GET /api/auth/me 做服务端权威验证。
   *
   * 契约：成功 → setCurrentUser + verifiedUser；401（apiFetch 已触发
   * handleUnauthorized/clearAuthState）→ verifiedUser=null；
   * 其它错误（网络/5xx）只标记 authChecked，不伪造登录也不强清显示态。
   * 并发调用共享同一个 in-flight 请求。
   */
  async function refreshAuth(): Promise<AuthUser | null> {
    if (refreshInflight) {
      return refreshInflight
    }
    refreshInflight = (async () => {
      try {
        const me = await apiFetch<CurrentUserResponse>('/auth/me')
        setCurrentUser(me)
        return verifiedUser.value
      } catch (err) {
        const status = (err as { status?: number }).status
        if (status === 401) {
          // apiFetch 的 401 分支已调用 clearAuthState；这里显式兜底保证状态一致。
          clearAuthState()
        } else {
          // 网络错误/5xx：不能证明未登录，只标记已探测，由守卫决定如何处置。
          authChecked.value = true
        }
        return null
      } finally {
        refreshInflight = null
      }
    })()
    return refreshInflight
  }

  return {
    user: effectiveUser,
    currentUser,
    isAuthenticated,
    isSystemAdmin,
    /** 服务端权威态（模块级 ref），供路由守卫与 API 层共享判断 */
    verifiedUser,
    /** me() 是否已完成至少一次探测 */
    authChecked,
    setAuth,
    setCurrentUser,
    clearAuth,
    refreshAuth
  }
}
