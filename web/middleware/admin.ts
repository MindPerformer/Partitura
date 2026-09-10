// middleware/admin.ts — system_admin 路由守卫
//
// 引入动机：管理员页面不能只依赖菜单隐藏；普通用户直接访问 /admin/*
// 时也必须在路由层被拒绝，避免页面加载管理员 API。

export default defineNuxtRouteMiddleware((to) => {
  const { isAuthenticated, isSystemAdmin } = useAuth()

  if (!isAuthenticated.value) {
    return navigateTo({
      path: '/login',
      query: { redirect: to.fullPath }
    })
  }

  if (!isSystemAdmin.value) {
    return navigateTo('/')
  }
})
