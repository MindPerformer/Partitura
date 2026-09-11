// middleware/auth.ts — 路由守卫
//
// 引入动机：design/04-WEB-API.md §Security 要求阻止未登录访问项目页面。
// login 和 bootstrap 页面不需要认证，其他所有页面需要认证。
//
// 行为：
// - /login 和 /bootstrap 是公开页面，不需要认证
// - 其他页面：若 verifiedUser 为空则先 await refreshAuth() 做服务端权威验证；
//   仍未通过则重定向 /login?redirect=<fullPath>（encodeURIComponent）
// - 已认证但访问 /login 时重定向到首页

export default defineNuxtRouteMiddleware((to) => {
  const { verifiedUser, authChecked, isAuthenticated, refreshAuth } = useAuth()

  // login 和 bootstrap 页面是公开的
  // 引入动机：bootstrap 仅在空数据库时可用，创建首个管理员后自动转入登录流程。
  // 不需要认证——首次使用时系统中尚无用户，无法认证。
  if (to.path === '/login' || to.path === '/bootstrap') {
    if (to.path === '/login' && isAuthenticated.value) {
      return navigateTo('/')
    }
    return
  }

  // 同步快路径：服务端权威态已确认，直接放行（保持返回值非 Promise 语义）。
  if (verifiedUser.value) {
    return
  }

  // me() 已完成且权威态为空：服务端已判定会话无效/不存在，直接拒绝，
  // 避免每次导航都重复打 /auth/me。
  if (authChecked.value) {
    return navigateTo(`/login?redirect=${encodeURIComponent(to.fullPath)}`)
  }

  // 尚未探测：发起服务端验证，成功后放行，否则重定向登录页。
  return (async () => {
    await refreshAuth()
    if (!verifiedUser.value) {
      return navigateTo(`/login?redirect=${encodeURIComponent(to.fullPath)}`)
    }
  })()
})
