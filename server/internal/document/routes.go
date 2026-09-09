// routes.go 实现 document 模块的路由注册。
//
// 引入动机：将 document 路由注册集中在一处，使 main.go 只需调用 RegisterRoutes 即可挂载全部端点。
//
// 路由清单：
//   - GET    /api/workspaces/{wid}/documents                   — viewer+，文档列表（分页）
//   - GET    /api/workspaces/{wid}/documents/outline           — viewer+，返回 heading tree
//   - GET    /api/workspaces/{wid}/documents/read              — viewer+，读取全文
//   - GET    /api/workspaces/{wid}/documents/section           — viewer+，读取指定 section
//   - GET    /api/workspaces/{wid}/documents/lines             — viewer+，读取行范围
//   - POST   /api/workspaces/{wid}/documents                   — editor+，创建文档
//   - PUT    /api/workspaces/{wid}/documents                   — editor+，替换全文
//   - PATCH  /api/workspaces/{wid}/documents                   — editor+，patch 文档
//   - POST   /api/workspaces/{wid}/documents/move              — editor+，移动文档
//   - POST   /api/workspaces/{wid}/documents/archive           — admin+，归档文档
//   - POST   /api/workspaces/{wid}/documents/restore           — admin+，恢复文档
//   - POST   /api/workspaces/{wid}/documents/purge             — owner，永久删除
//   - GET    /api/workspaces/{wid}/documents/history           — viewer+，版本列表
//   - GET    /api/workspaces/{wid}/documents/revision          — viewer+，指定版本
//   - GET    /api/workspaces/{wid}/documents/sources           — viewer+，来源列表
//   - POST   /api/workspaces/{wid}/documents/sources           — editor+，添加来源
//   - DELETE /api/workspaces/{wid}/documents/sources/{sourceId} — admin+，删除来源
//
// middleware 链（请求进入顺序）：
//   - AuthMiddleware：解析 cookie session 或 bearer token
//   - RequireAuth：拒绝未认证请求
//   - RequireCSRF：验证 cookie session 认证的状态变更请求的 CSRF token
//   - RequireWorkspacePermission：workspace 级权限检查
package document

import (
	"net/http"

	"partitura/server/internal/auth"
	"partitura/server/internal/workspace"
)

// RegisterRoutes 在给定的 mux 上注册 document 模块的全部 HTTP 路由。
//
// 引入动机：main.go 调用此函数完成 document 路由注册，保持入口文件简洁。
//
// 参数：
//   - mux：HTTP 路由器
//   - handler：document handler
//   - wsRepo：workspace 数据访问接口（用于 RequireWorkspacePermission）
//   - authRepo：auth 数据访问接口（用于 AuthMiddleware）
//   - authCfg：认证配置
func RegisterRoutes(
	mux *http.ServeMux,
	handler *Handler,
	wsRepo workspace.Repository,
	authRepo auth.Repository,
	authCfg auth.AuthConfig,
) {
	// --- 只读端点（viewer+，PermRead） ---

	// GET /api/workspaces/{wid}/documents — 文档列表
	mux.Handle("GET /api/workspaces/{wid}/documents",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermRead, "wid")(
					http.HandlerFunc(handler.ListDocuments),
				),
			),
		),
	)

	// GET /api/workspaces/{wid}/documents/outline — heading tree
	mux.Handle("GET /api/workspaces/{wid}/documents/outline",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermRead, "wid")(
					http.HandlerFunc(handler.GetOutline),
				),
			),
		),
	)

	// GET /api/workspaces/{wid}/documents/read — 读取全文
	mux.Handle("GET /api/workspaces/{wid}/documents/read",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermRead, "wid")(
					http.HandlerFunc(handler.ReadDocument),
				),
			),
		),
	)

	// GET /api/workspaces/{wid}/documents/section — 读取指定 section
	mux.Handle("GET /api/workspaces/{wid}/documents/section",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermRead, "wid")(
					http.HandlerFunc(handler.ReadSection),
				),
			),
		),
	)

	// GET /api/workspaces/{wid}/documents/lines — 读取行范围
	mux.Handle("GET /api/workspaces/{wid}/documents/lines",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermRead, "wid")(
					http.HandlerFunc(handler.ReadLines),
				),
			),
		),
	)

	// GET /api/workspaces/{wid}/documents/history — 版本列表
	mux.Handle("GET /api/workspaces/{wid}/documents/history",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermHistory, "wid")(
					http.HandlerFunc(handler.ListHistory),
				),
			),
		),
	)

	// GET /api/workspaces/{wid}/documents/revision — 指定版本
	mux.Handle("GET /api/workspaces/{wid}/documents/revision",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermHistory, "wid")(
					http.HandlerFunc(handler.GetRevision),
				),
			),
		),
	)

	// GET /api/workspaces/{wid}/documents/sources — 来源列表
	mux.Handle("GET /api/workspaces/{wid}/documents/sources",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireWorkspacePermission(wsRepo, workspace.PermRead, "wid")(
					http.HandlerFunc(handler.ListSources),
				),
			),
		),
	)

	// --- 写入端点（editor+，PermCreate/PermUpdate） ---

	// POST /api/workspaces/{wid}/documents — 创建文档
	mux.Handle("POST /api/workspaces/{wid}/documents",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermCreate, "wid")(
						http.HandlerFunc(handler.CreateDocument),
					),
				),
			),
		),
	)

	// PUT /api/workspaces/{wid}/documents — 替换全文
	mux.Handle("PUT /api/workspaces/{wid}/documents",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermUpdate, "wid")(
						http.HandlerFunc(handler.ReplaceDocument),
					),
				),
			),
		),
	)

	// PATCH /api/workspaces/{wid}/documents — patch 文档
	mux.Handle("PATCH /api/workspaces/{wid}/documents",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermUpdate, "wid")(
						http.HandlerFunc(handler.PatchDocument),
					),
				),
			),
		),
	)

	// POST /api/workspaces/{wid}/documents/move — 移动文档
	mux.Handle("POST /api/workspaces/{wid}/documents/move",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermMove, "wid")(
						http.HandlerFunc(handler.MoveDocument),
					),
				),
			),
		),
	)

	// POST /api/workspaces/{wid}/documents/sources — 添加来源
	mux.Handle("POST /api/workspaces/{wid}/documents/sources",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermUpdate, "wid")(
						http.HandlerFunc(handler.AddSource),
					),
				),
			),
		),
	)

	// --- 管理端点（admin+，PermArchive） ---

	// POST /api/workspaces/{wid}/documents/archive — 归档文档
	mux.Handle("POST /api/workspaces/{wid}/documents/archive",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermArchive, "wid")(
						http.HandlerFunc(handler.ArchiveDocument),
					),
				),
			),
		),
	)

	// POST /api/workspaces/{wid}/documents/restore — 恢复文档
	mux.Handle("POST /api/workspaces/{wid}/documents/restore",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermArchive, "wid")(
						http.HandlerFunc(handler.RestoreDocument),
					),
				),
			),
		),
	)

	// DELETE /api/workspaces/{wid}/documents/sources/{sourceId} — 删除来源
	mux.Handle("DELETE /api/workspaces/{wid}/documents/sources/{sourceId}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermArchive, "wid")(
						http.HandlerFunc(handler.DeleteSource),
					),
				),
			),
		),
	)

	// --- Owner 端点 ---

	// POST /api/workspaces/{wid}/documents/purge — 永久删除（owner only）
	// RequireWorkspacePermission(PermPurge) 确保只有 owner 能通过
	mux.Handle("POST /api/workspaces/{wid}/documents/purge",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireWorkspacePermission(wsRepo, workspace.PermPurge, "wid")(
						http.HandlerFunc(handler.PurgeDocument),
					),
				),
			),
		),
	)
}
