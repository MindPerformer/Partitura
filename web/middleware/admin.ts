// middleware/admin.ts — system_admin 路由守卫
//
// 引入动机：管理员页面不能只依赖菜单隐藏；普通用户直接访问 /admin/*
// 时也必须在路由层被拒绝，避免页面加载管理员 API。
//
// 授权依据：仅信服务端权威态 verifiedUser（不可被客户端 cookie 伪造）。
// 链式顺序 ['auth', 'admin'] 下本守卫通常在 auth 之后执行，但为防御性
// 编程这里独立保证：无 verifiedUser 先 await refreshAuth()，仍无 → /login，
// 非 system_admin → /。

export default defineNuxtRouteMiddleware((to) => {
  const { verifiedUser, authChecked, isSystemAdmin, refreshAuth } = useAuth()

  function deny(): ReturnType<typeof navigateTo> {
    // 权威态仍为空：未完成认证 → 登录页；已认证但非管理员 → 首页。
    if (!verifiedUser.value) {
      return navigateTo(`/login?redirect=${encodeURIComponent(to.fullPath)}`)
    }
    return navigateTo('/')
  }

  if (verifiedUser.value) {
    return isSystemAdmin.value ? undefined : deny()
  }

  if (authChecked.value) {
    // me() 已完成且权威态为空：未认证。
    return navigateTo(`/login?redirect=${encodeURIComponent(to.fullPath)}`)
  }

  return (async () => {
    await refreshAuth()
    if (!verifiedUser.value) {
      return navigateTo(`/login?redirect=${encodeURIComponent(to.fullPath)}`)
    }
    if (!isSystemAdmin.value) {
      return navigateTo('/')
    }
  })()
})
