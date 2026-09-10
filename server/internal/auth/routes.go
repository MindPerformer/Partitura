// routes.go 实现 auth 模块的路由注册。
//
// 引入动机：将路由注册集中在一处，使 main.go 只需调用 RegisterRoutes 即可挂载全部 auth 端点。
// 路由按端点功能分组，middleware 按需挂载。
package auth

import (
	"net/http"
)

// RegisterRoutes 在给定的 mux 上注册 auth 模块的全部 HTTP 路由。
//
// 引入动机：main.go 调用此函数完成 auth 路由注册，保持入口文件简洁。
//
// 路由清单：
//   - POST /api/auth/login            — 公开，凭据认证
//   - POST /api/auth/logout           — 需认证 + CSRF
//   - POST /api/auth/device/authorize — 需认证 + CSRF
//   - POST /api/auth/refresh          — 公开（需 refresh token）
//   - POST /api/auth/revoke           — 需认证 + CSRF
//   - GET  /api/auth/device/sessions  — 需认证，列出当前用户设备会话
//
// middleware 链（请求进入顺序）：
//   - AuthMiddleware：解析 cookie session 或 bearer token，设置 Identity
//   - RequireAuth：拒绝未认证请求（依赖 AuthMiddleware 已注入 Identity）
//   - RequireCSRF：验证 cookie session 认证的状态变更请求的 CSRF token
//
// 关键约束：AuthMiddleware 必须在 RequireAuth 之前执行，
// 否则 RequireAuth 在 Identity 尚未注入 context 时永远返回 401。
//
// 参数：
//   - mux：HTTP 路由器
//   - handler：auth handler
//   - repo：数据访问接口
//   - cfg：认证配置
func RegisterRoutes(mux *http.ServeMux, handler *Handler, repo Repository, cfg AuthConfig) {
	// 公开端点（不需要认证）
	// login 基于凭据认证，refresh 基于 refresh token 认证——两者均不依赖 cookie session，
	// 不应被施加 CSRF 验证。
	mux.HandleFunc("POST /api/auth/login", handler.Login)
	mux.HandleFunc("POST /api/auth/refresh", handler.Refresh)

	// 需要认证 + CSRF 的端点
	// middleware 执行顺序严格为：AuthMiddleware → RequireAuth → RequireCSRF → handler
	// AuthMiddleware 先解析凭据并注入 Identity，RequireAuth 才能正确判断认证状态，
	// RequireCSRF 最后对 cookie session 认证的状态变更请求验证 CSRF token。
	protected := AuthMiddleware(repo, cfg)(RequireAuth(RequireCSRF(repo, cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/logout":
			handler.Logout(w, r)
		case "/api/auth/device/authorize":
			handler.DeviceAuthorize(w, r)
		case "/api/auth/revoke":
			handler.Revoke(w, r)
		default:
			writeError(w, http.StatusNotFound, "未找到路由")
		}
	}))))

	mux.Handle("POST /api/auth/logout", protected)
	mux.Handle("POST /api/auth/device/authorize", protected)
	mux.Handle("POST /api/auth/revoke", protected)

	// 设备会话列表只需要认证，不涉及状态变更，因此不需要 CSRF。
	mux.Handle("GET /api/auth/device/sessions",
		AuthMiddleware(repo, cfg)(RequireAuth(http.HandlerFunc(handler.ListDeviceSessions))),
	)
}
