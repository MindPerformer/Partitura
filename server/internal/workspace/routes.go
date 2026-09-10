// routes.go 实现 workspace 和 admin 模块的路由注册。
//
// 引入动机：将路由注册集中在一处，使 main.go 只需调用 RegisterRoutes 即可挂载全部端点。
// 路由按端点功能分组，middleware 按需挂载。
//
// middleware 链（请求进入顺序）：
//   - AuthMiddleware：解析 cookie session 或 bearer token，设置 Identity
//   - RequireAuth：拒绝未认证请求
//   - RequireCSRF：验证 cookie session 认证的状态变更请求的 CSRF token
//   - RequireWorkspacePermission / RequireSystemAdmin：workspace/admin 级权限检查
//
// 关键约束：AuthMiddleware 必须在 RequireAuth 之前执行，
// 否则 RequireAuth 在 Identity 尚未注入 context 时永远返回 401。
package workspace

import (
	"net/http"

	"partitura/server/internal/auth"
)

// RegisterRoutes 在给定的 mux 上注册 workspace 模块的全部 HTTP 路由。
//
// 引入动机：main.go 调用此函数完成 workspace 路由注册，保持入口文件简洁。
//
// 路由清单：
//   - GET    /api/workspaces                   — 已认证，返回当前用户成员的 workspace 列表
//   - POST   /api/workspaces                   — 已认证 + workspace:create，创建 workspace
//   - GET    /api/workspaces/{id}              — member，返回 workspace 详情
//   - PUT    /api/workspaces/{id}              — workspace admin+，更新 workspace
//   - POST   /api/workspaces/{id}/archive      — workspace admin+，归档 workspace
//   - GET    /api/workspaces/{id}/stats        — member，返回工作台聚合统计
//   - GET    /api/workspaces/{id}/members      — member，返回成员列表
//   - POST   /api/workspaces/{id}/members      — workspace admin+，添加成员
//   - PUT    /api/workspaces/{id}/members/{userId} — workspace admin+，修改成员角色
//   - DELETE /api/workspaces/{id}/members/{userId} — workspace admin+，移除成员
//
// 参数：
//   - mux：HTTP 路由器
//   - handler：workspace handler
//   - adminHandler：admin handler
//   - authRepo：auth 数据访问接口（用于 AuthMiddleware）
//   - authCfg：认证配置
func RegisterRoutes(
	mux *http.ServeMux,
	handler *Handler,
	adminHandler *AdminHandler,
	authRepo auth.Repository,
	authCfg auth.AuthConfig,
) {
	// --- workspace 列表/创建（不需要 workspace-scoped 权限检查） ---
	// GET /api/workspaces — 已认证即可
	mux.Handle("GET /api/workspaces",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				http.HandlerFunc(handler.ListWorkspaces),
			),
		),
	)

	// POST /api/workspaces — 已认证 + CSRF（状态变更）
	mux.Handle("POST /api/workspaces",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					http.HandlerFunc(handler.CreateWorkspace),
				),
			),
		),
	)

	// --- workspace scoped 端点（需要 RequireWorkspacePermission） ---

	// GET /api/workspaces/{id} — member（PermRead 是最低权限）
	mux.Handle("GET /api/workspaces/{id}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireWorkspacePermission(handler.repo, PermRead, "id")(
					http.HandlerFunc(handler.GetWorkspace),
				),
			),
		),
	)

	// PUT /api/workspaces/{id} — workspace admin+（PermSettings）
	mux.Handle("PUT /api/workspaces/{id}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					RequireWorkspacePermission(handler.repo, PermSettings, "id")(
						http.HandlerFunc(handler.UpdateWorkspace),
					),
				),
			),
		),
	)

	// POST /api/workspaces/{id}/archive — workspace admin+（PermArchive）
	mux.Handle("POST /api/workspaces/{id}/archive",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					RequireWorkspacePermission(handler.repo, PermArchive, "id")(
						http.HandlerFunc(handler.ArchiveWorkspace),
					),
				),
			),
		),
	)

	// GET /api/workspaces/{id}/stats — member（PermRead 是最低权限）
	// 引入动机：Phase6 WP2 工作台首页需要聚合统计信息。
	mux.Handle("GET /api/workspaces/{id}/stats",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireWorkspacePermission(handler.repo, PermRead, "id")(
					http.HandlerFunc(handler.GetWorkspaceStats),
				),
			),
		),
	)

	// GET /api/workspaces/{id}/me/membership — member（PermRead 是最低权限）
	// 引入动机：Web 需要直接获取当前用户角色，避免反查 members 第一页推断。
	mux.Handle("GET /api/workspaces/{id}/me/membership",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireWorkspacePermission(handler.repo, PermRead, "id")(
					http.HandlerFunc(handler.GetMyMembership),
				),
			),
		),
	)

	// GET /api/workspaces/{id}/members — member（PermRead 是最低权限）
	mux.Handle("GET /api/workspaces/{id}/members",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireWorkspacePermission(handler.repo, PermRead, "id")(
					http.HandlerFunc(handler.ListMembers),
				),
			),
		),
	)

	// GET /api/workspaces/{id}/members/candidates — workspace admin+（PermMemberManage）
	mux.Handle("GET /api/workspaces/{id}/members/candidates",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireWorkspacePermission(handler.repo, PermMemberManage, "id")(
					http.HandlerFunc(handler.ListMemberCandidates),
				),
			),
		),
	)

	// POST /api/workspaces/{id}/members — workspace admin+（PermMemberManage）
	mux.Handle("POST /api/workspaces/{id}/members",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					RequireWorkspacePermission(handler.repo, PermMemberManage, "id")(
						http.HandlerFunc(handler.AddMember),
					),
				),
			),
		),
	)

	// PUT /api/workspaces/{id}/members/{userId} — workspace admin+（PermMemberManage）
	mux.Handle("PUT /api/workspaces/{id}/members/{userId}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					RequireWorkspacePermission(handler.repo, PermMemberManage, "id")(
						http.HandlerFunc(handler.UpdateMemberRole),
					),
				),
			),
		),
	)

	// DELETE /api/workspaces/{id}/members/{userId} — workspace admin+（PermMemberManage）
	mux.Handle("DELETE /api/workspaces/{id}/members/{userId}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					RequireWorkspacePermission(handler.repo, PermMemberManage, "id")(
						http.HandlerFunc(handler.RemoveMember),
					),
				),
			),
		),
	)

	// --- Admin 端点（仅 system_admin） ---

	// GET /api/admin/users — system_admin
	mux.Handle("GET /api/admin/users",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireSystemAdmin(
					http.HandlerFunc(adminHandler.ListUsers),
				),
			),
		),
	)

	// POST /api/admin/users — system_admin + CSRF，创建普通用户
	// 引入动机：计划要求仅 system_admin 可创建普通用户，复用 CSRF 和 session 认证。
	mux.Handle("POST /api/admin/users",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					RequireSystemAdmin(
						http.HandlerFunc(adminHandler.CreateUser),
					),
				),
			),
		),
	)

	// PUT /api/admin/users/{id} — system_admin + CSRF
	mux.Handle("PUT /api/admin/users/{id}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					RequireSystemAdmin(
						http.HandlerFunc(adminHandler.UpdateUser),
					),
				),
			),
		),
	)

	// GET /api/admin/workspaces — system_admin
	mux.Handle("GET /api/admin/workspaces",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireSystemAdmin(
					http.HandlerFunc(adminHandler.ListAllWorkspaces),
				),
			),
		),
	)

	// GET /api/admin/audit — system_admin
	mux.Handle("GET /api/admin/audit",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				RequireSystemAdmin(
					http.HandlerFunc(adminHandler.ListAuditLogs),
				),
			),
		),
	)
}
