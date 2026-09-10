// composables/useAuth.ts — 认证状态管理
//
// 引入动机：design/04-WEB-API.md §Security 要求 secure cookie 认证。
// server 没有 /api/auth/me 端点，login 响应提供 user 信息。
// 认证状态基于 cookie session，前端通过 login 获取用户信息，
// 通过 API 401 响应判断 session 过期。
//
// 安全：
// - 不将 password/session/access/refresh token 存入 localStorage/sessionStorage/URL/日志
// - 用户显示信息（id/username/system_role）非敏感数据，使用 Nuxt cookie 持久化供 SSR hydration
// - 认证状态通过 API 请求结果验证，不依赖前端可篡改的状态

import type { CurrentUserResponse, LoginResponse } from '~/types/api'

/** 认证用户信息（非敏感显示数据） */
export interface AuthUser {
  id: string
  username: string
  email?: string
  system_role: string
  workspace_create_perm?: boolean
}

const AUTH_COOKIE_NAME = 'pkw_user'

/**
 * useAuth — 全局认证状态 composable
 *
 * 行为：
 * - 使用 Nuxt useState 保持响应式认证状态
 * - 使用 Nuxt useCookie 持久化用户显示信息（非敏感）
 * - login 成功后设置状态，logout/401 后清理
 * - isAuthenticated 判断基于 user 状态存在性
 * - 系统管理员判断基于 system_role === 'system_admin'
 */
export function useAuth() {
  const userState = useState<AuthUser | null>('auth_user', () => null)
  const userCookie = useCookie<AuthUser | null>(AUTH_COOKIE_NAME, {
    maxAge: 86400,
    sameSite: 'lax',
    path: '/'
  })

  // 从 cookie 恢复用户信息（SSR hydration）
  if (!userState.value && userCookie.value) {
    userState.value = userCookie.value
  }

  const isAuthenticated = computed(() => userState.value !== null)
  const isSystemAdmin = computed(() => userState.value?.system_role === 'system_admin')
  const currentUser = computed(() => userState.value)

  /** 设置认证状态（login 成功后调用） */
  function setAuth(loginResponse: LoginResponse) {
    const user: AuthUser = {
      id: loginResponse.user.id,
      username: loginResponse.user.username,
      system_role: loginResponse.user.system_role
    }
    userState.value = user
    userCookie.value = user
  }

  /** 用 /auth/me 的权威响应刷新当前用户显示状态。 */
  function setCurrentUser(current: CurrentUserResponse) {
    const user: AuthUser = {
      id: current.id,
      username: current.username,
      email: current.email,
      system_role: current.system_role,
      workspace_create_perm: current.workspace_create_perm
    }
    userState.value = user
    userCookie.value = user
  }

  /** 清理认证状态（logout 或 401 时调用） */
  function clearAuth() {
    userState.value = null
    userCookie.value = null
  }

  return {
    user: userState,
    currentUser,
    isAuthenticated,
    isSystemAdmin,
    setAuth,
    setCurrentUser,
    clearAuth
  }
}
