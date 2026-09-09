// Package httpmw 实现全局 HTTP middleware：结构化请求日志。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 1 要求 HTTP 路由组装包含
// middleware (auth, request_id, slog)。design/05-OPERATIONS.md §Logging 要求
// 使用 log/slog structured logging，字段包含 request_id、user_id、latency 等。
//
// request ID 相关功能已拆分至 internal/requestid 包，以消除
// internal/auth → internal/http → internal/auth 的导入环。
// 此包仅保留 LoggingMiddleware，它同时依赖 auth（获取已认证身份）
// 和 requestid（获取 request ID），不构成环。
//
// 安全约束：
//   - 不记录请求 query 中的敏感值（仅记录 r.URL.Path，不记录 RawQuery）
//   - 不记录密码、cookie、token、请求 body
package httpmw

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"partitura/server/internal/auth"
	"partitura/server/internal/requestid"
)

// statusRecorder 包装 http.ResponseWriter 以捕获响应状态码。
// 引入动机：LoggingMiddleware 需要在请求结束后记录 HTTP 状态码，
// 标准 http.ResponseWriter 不暴露已写入的状态码，需要包装。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader 拦截状态码写入并记录。
func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// LoggingMiddleware 在每个 HTTP 请求结束后用 slog 记录结构化请求日志。
//
// 记录字段：
//   - method：HTTP 方法
//   - path：请求路径（仅 r.URL.Path，不记录 query 中的敏感值）
//   - status：HTTP 状态码
//   - duration：请求处理耗时
//   - request_id：从 context 获取的 request ID
//   - user_id：如果上下文中有已认证身份则记录（安全字段）
//
// 安全约束：
//   - 禁止记录密码、cookie、csrf/access/refresh token 或请求 body
//   - 不记录 r.URL.RawQuery（query 参数可能包含敏感值）
//
// 引入动机：design/05-OPERATIONS.md §Logging 要求 log/slog structured logging，
// 字段包含 request_id、user_id、latency。design/06-IMPLEMENTATION.md Phase 1 要求 slog middleware。
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sr, r)

		duration := time.Since(start)
		requestID := requestid.RequestIDFromContext(r.Context())

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"duration", duration.String(),
			"request_id", requestID,
		}

		// 如果上下文中有已认证身份，安全记录 user_id。
		// 引入动机：design/05-OPERATIONS.md §Logging 要求 user_id 字段。
		// 通过 auth.IdentityFromContext 获取已认证身份中的 UserID，
		// 不记录原始 cookie/token 等凭据。
		if id := auth.IdentityFromContext(r.Context()); id != nil {
			attrs = append(attrs, "user_id", id.UserID)
		}

		slog.Info("HTTP 请求完成", attrs...)
	})
}

// 以下导出函数和变量为向后兼容保留，委托至 requestid 包。
// 引入动机：避免所有调用方同时修改导入路径，降低变更风险。
// 新代码应直接使用 requestid 包。

// RequestIDFromContext 从 request context 中提取 request ID。
// 委托至 requestid.RequestIDFromContext。
func RequestIDFromContext(ctx context.Context) string {
	return requestid.RequestIDFromContext(ctx)
}

// RequestIDMiddleware 为每个 HTTP 请求注入 request ID。
// 委托至 requestid.Middleware。
func RequestIDMiddleware(next http.Handler) http.Handler {
	return requestid.Middleware(next)
}
