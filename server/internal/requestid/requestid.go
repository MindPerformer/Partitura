// Package requestid 实现 HTTP request ID 的生成、注入和提取。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 1 要求 request_id middleware，
// design/05-OPERATIONS.md §Logging 要求所有请求带 request_id。
//
// 此包从 internal/http 拆分出来，以消除 internal/auth → internal/http → internal/auth
// 的导入环。auth 包需要 RequestIDFromContext 来在审计日志中记录 request ID，
// 但不能导入 http 包（http 包的 LoggingMiddleware 需要 auth.IdentityFromContext）。
// 将 request ID 相关功能独立到此包后，auth 和 http 都可以安全导入此包。
//
// 提供的功能：
//   - RequestIDMiddleware：为每个请求生成或保留 request ID，放入 context 并回写响应头
//   - RequestIDFromContext：从 request context 中提取 request ID
//
// 安全约束：
//   - request ID 使用 crypto/rand 生成，不可预测
//   - 入站 request ID 需通过长度和字符限制验证，防止注入
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
)

// HeaderName 是 request ID 的 HTTP 头名称。
// 引入动机：design/05-OPERATIONS.md §Logging 要求 request_id 作为日志字段，
// 同时需要通过 HTTP 头传递 correlation ID 供客户端和上游代理使用。
const HeaderName = "X-Request-ID"

// MaxLength 是入站 request ID 的最大允许长度。
// 引入动机：防止过长的 header 值导致内存浪费或日志膨胀，
// 128 字符足以容纳 UUID、ULID 等常见 correlation ID 格式。
const MaxLength = 128

// ctxKey 是 request context 中存储 request ID 的键类型。
// 引入动机：使用自定义类型避免 context key 冲突。
type ctxKey struct{}

// RequestIDFromContext 从 request context 中提取 request ID。
// 引入动机：handler 和审计记录需要从 context 获取全局 middleware 注入的 request ID，
// 保证每条请求都有 correlation ID，不依赖客户端是否传入了 X-Request-ID header。
//
// 如果 context 中没有 request ID（如未经过全局 middleware 的单元测试调用），
// 返回空字符串。
func RequestIDFromContext(ctx context.Context) string {
	v := ctx.Value(ctxKey{})
	if v == nil {
		return ""
	}
	id, ok := v.(string)
	if !ok {
		return ""
	}
	return id
}

// generate 使用 crypto/rand 生成不可预测的 16 字节十六进制 request ID。
// 引入动机：当入站请求缺少 X-Request-ID 或传入值非法时，
// 服务器必须自行生成不可预测的 correlation ID，防止猜测和碰撞。
func generate() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read 在正常系统环境下不会失败。
		// 如果失败，说明系统熵源异常，属于致命错误。
		slog.Error("生成 request ID 失败：crypto/rand 不可用", "error", err)
		// 仍需返回一个值以维持服务可用性，但此路径不应在正常环境中出现。
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// IsValid 验证入站 request ID 是否满足安全限制。
// 引入动机：入站 X-Request-ID header 由客户端控制，必须验证长度和字符集，
// 防止注入恶意字符串到日志或审计记录中。
//
// 规则：
//   - 长度在 1 到 MaxLength 之间
//   - 仅允许字母、数字、连字符（-）、下划线（_）
func IsValid(id string) bool {
	if len(id) == 0 || len(id) > MaxLength {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Middleware 为每个 HTTP 请求注入 request ID。
//
// 行为：
//  1. 检查入站 X-Request-ID header——非空且通过安全验证则保留
//  2. 缺失或非法时由服务器用 crypto/rand 生成不可预测 request ID
//  3. 将 request ID 放入 request context
//  4. 响应始终通过 X-Request-ID 头返回 request ID
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 1 要求 request_id middleware，
// design/05-OPERATIONS.md §Logging 要求所有请求带 request_id。
// 保证每条请求（无论客户端是否传入）都有 correlation ID。
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(HeaderName)
		if !IsValid(requestID) {
			requestID = generate()
		}

		// 将 request ID 放入 context 供后续 handler 和审计使用
		ctx := context.WithValue(r.Context(), ctxKey{}, requestID)
		r = r.WithContext(ctx)

		// 响应始终返回 X-Request-ID
		w.Header().Set(HeaderName, requestID)

		next.ServeHTTP(w, r)
	})
}
