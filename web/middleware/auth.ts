// middleware/auth.ts — 路由守卫
//
// 引入动机：design/04-WEB-API.md §Security 要求阻止未登录访问项目页面。
// login 和 bootstrap 页面不需要认证，其他所有页面需要认证。
//
// 行为：
// - 检查 useAuth().isAuthenticated
// - /login 和 /bootstrap 是公开页面，不需要认证
// - 未认证时重定向到 /login，携带 redirect 参数
// - 已认证但访问 /login 时重定向到首页

export default defineNuxtRouteMiddleware((to) => {
  const { isAuthenticated } = useAuth()

  // login 和 bootstrap 页面是公开的
  // 引入动机：bootstrap 仅在空数据库时可用，创建首个管理员后自动转入登录流程。
  // 不需要认证——首次使用时系统中尚无用户，无法认证。
  if (to.path === '/login' || to.path === '/bootstrap') {
    if (to.path === '/login' && isAuthenticated.value) {
      return navigateTo('/')
    }
    return
  }

  // 其他页面需要认证
  if (!isAuthenticated.value) {
    return navigateTo({
      path: '/login',
      query: { redirect: to.fullPath }
    })
  }
})
