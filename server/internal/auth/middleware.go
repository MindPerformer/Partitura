// middleware.go 实现认证 HTTP middleware：cookie session 认证、bearer token 认证、CSRF 验证。
//
// 引入动机：design/04-WEB-API.md §Security 要求 secure cookies、CSRF protection、
// ACL 后端强制。middleware 层负责从请求中提取认证凭据、验证有效性、
// 将已认证身份放入 request context 供后续 handler 使用。
//
// 认证方式：
//   - Cookie session：从 cookie 中提取 session token，验证 session 有效性
//   - Bearer token：从 Authorization 头提取 access token，验证 device session 有效性
//
// CSRF 验证仅用于 cookie session 认证的状态变更请求（POST/PUT/PATCH/DELETE）。
// Bearer token 认证不需要 CSRF（token 通过 Authorization 头传递，不受 CSRF 影响）。
package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// IdentityFromContext 从 request context 中提取已认证身份。
// 引入动机：handler 需要从 context 获取 Identity 以执行业务逻辑。
// 如果 context 中没有 Identity，返回 nil（表示未认证）。
func IdentityFromContext(ctx context.Context) *Identity {
	v := ctx.Value(ctxKey)
	if v == nil {
		return nil
	}
	id, ok := v.(*Identity)
	if !ok {
		return nil
	}
	return id
}

// WithIdentity 将 Identity 放入 request context。
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey, id)
}

// AuthMiddleware 同时支持 cookie session 和 bearer token 认证。
//
// 认证顺序：
//  1. 检查 Authorization 头是否有 Bearer token——有则验证 bearer
//  2. 否则检查 cookie 中是否有 session token——有则验证 cookie session
//  3. 如果都没有，不设置 Identity（匿名请求）
//
// 此 middleware 不拒绝未认证请求——拒绝逻辑由 RequireAuth middleware 处理。
// 引入动机：某些路由需要同时支持认证和匿名访问（如 login），由具体 handler 决定。
//
// 参数：
//   - repo：数据访问接口
//   - cfg：认证配置（用于获取 cookie 名称）
func AuthMiddleware(repo Repository, cfg AuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 优先检查 Bearer token
			authHeader := r.Header.Get("Authorization")
			if authHeader != "" {
				if strings.HasPrefix(authHeader, "Bearer ") {
					token := strings.TrimPrefix(authHeader, "Bearer ")
					token = strings.TrimSpace(token)
					if token != "" {
						id, err := ValidateAccessToken(r.Context(), repo, token)
						if err != nil {
							// Bearer token 无效——不设置 Identity，继续处理
							// 不在这里拒绝，因为可能是 cookie session 请求
							slog.Debug("bearer token 验证失败", "error", err)
						} else {
							r = r.WithContext(WithIdentity(r.Context(), id))
						}
					}
				}
			}

			// 如果 bearer 没有成功，检查 cookie session
			if IdentityFromContext(r.Context()) == nil {
				cookie, err := r.Cookie(cfg.CookieName)
				if err == nil && cookie.Value != "" {
					id, err := ValidateSession(r.Context(), repo, cookie.Value)
					if err != nil {
						slog.Debug("cookie session 验证失败", "error", err)
					} else {
						r = r.WithContext(WithIdentity(r.Context(), id))
					}
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuth 要求请求已认证，否则返回 401。
// 引入动机：需要认证的端点（如 device authorize、revoke）使用此 middleware。
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id == nil {
			writeError(w, http.StatusUnauthorized, "未认证")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireCSRF 要求 cookie session 认证的状态变更请求携带正确的 CSRF token。
//
// 验证逻辑：
//  1. 仅对状态变更方法（POST/PUT/PATCH/DELETE）生效
//  2. 从 cookie 中提取 session token
//  3. 从请求头中提取 CSRF token
//  4. 验证 CSRF token 与 session 关联的 CSRF token 匹配
//
// 引入动机：design/04-WEB-API.md §Security 要求 CSRF protection。
// 使用 double-submit 模式：CSRF token 通过非 HttpOnly cookie 提供给前端，
// 前端 JS 读取后以 header 回传，middleware 验证 header 中的 token 与 DB 中存储的哈希匹配。
func RequireCSRF(repo Repository, cfg AuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 仅状态变更方法需要 CSRF 验证
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}

			// Bearer token 认证不需要 CSRF
			id := IdentityFromContext(r.Context())
			if id != nil && id.AuthMethod == AuthMethodBearer {
				next.ServeHTTP(w, r)
				return
			}

			// Cookie session 认证需要 CSRF
			cookie, err := r.Cookie(cfg.CookieName)
			if err != nil || cookie.Value == "" {
				writeError(w, http.StatusForbidden, "缺少 session cookie")
				return
			}

			csrfToken := r.Header.Get(cfg.CSRFHeaderName)
			if csrfToken == "" {
				writeError(w, http.StatusForbidden, "缺少 CSRF token")
				return
			}

			if err := ValidateCSRF(r.Context(), repo, cookie.Value, csrfToken); err != nil {
				slog.Info("CSRF 验证失败", "error", err)
				writeError(w, http.StatusForbidden, "CSRF token 无效")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// writeError 写入统一 JSON 错误响应。
// 引入动机：所有 middleware 拒绝请求时使用统一格式。
// 安全要求：使用 encoding/json 编码消息，防止消息中包含引号、反斜杠等
// 特殊字符时产生非法 JSON 或注入风险。
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		slog.Error("写入错误响应失败", "error", err)
	}
}

// writeJSON 写入 JSON 响应。
// 引入动机：handler 共用此工具函数序列化响应体。
func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("序列化 JSON 响应失败", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"内部错误"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
