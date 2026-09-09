// rbac.go 实现 Workspace RBAC 权限检查中间件和辅助函数。
//
// 引入动机：design/04-WEB-API.md §Security 要求 "ACL 后端强制"——
// 每个 workspace-scoped handler 必须在进入领域逻辑前通过后端 member/RBAC 检查；
// 绝不相信请求中声明的 user、role 或 workspace scope。
// design/00-MASTER.md §Workspace 禁止跨 Workspace 访问。
//
// 本文件提供：
//   - WorkspaceContext：存储已验证的 workspace ID 和成员角色，供 handler 使用
//   - RequireWorkspacePermission：基于服务端 membership 判断权限的中间件
//   - 辅助函数：从 context 提取 workspace context、检查系统角色
package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"partitura/server/internal/auth"
	"partitura/server/internal/queryutil"
	"io"
)

// workspaceCtxKey 是 request context 中存储 WorkspaceContext 的键类型。
// 引入动机：使用自定义类型避免 context key 冲突。
type workspaceCtxKey struct{}

// wsCtxKey 是包级 context key 实例。
var wsCtxKey = workspaceCtxKey{}

// WorkspaceContext 存储已验证的 workspace 上下文信息。
// 引入动机：handler 需要从 context 获取已验证的 workspace ID 和当前用户角色，
// 这些信息由 RequireWorkspacePermission 中间件从服务端查询并注入。
type WorkspaceContext struct {
	// WorkspaceID 是经服务端验证存在的 workspace UUID。
	WorkspaceID string

	// MemberRole 是当前用户在该 workspace 中的角色（从服务端查询，非客户端声明）。
	// 空字符串表示非成员（但 RequireWorkspacePermission 已拒绝非成员请求）。
	MemberRole string
}

// WithWorkspaceContext 将 WorkspaceContext 放入 request context。
// 引入动机：RequireWorkspacePermission 验证通过后需要将上下文传递给 handler。
func WithWorkspaceContext(ctx context.Context, wsc *WorkspaceContext) context.Context {
	return context.WithValue(ctx, wsCtxKey, wsc)
}

// WorkspaceContextFromContext 从 request context 中提取 WorkspaceContext。
// 引入动机：handler 需要获取已验证的 workspace 上下文以执行业务逻辑。
// 如果 context 中没有 WorkspaceContext，返回 nil。
func WorkspaceContextFromContext(ctx context.Context) *WorkspaceContext {
	v := ctx.Value(wsCtxKey)
	if v == nil {
		return nil
	}
	wsc, ok := v.(*WorkspaceContext)
	if !ok {
		return nil
	}
	return wsc
}

// RequireWorkspacePermission 返回一个中间件，验证当前已认证用户在指定 workspace 中
// 拥有指定权限。
//
// 引入动机：design/04-WEB-API.md §Security 要求 ACL 后端强制。
// 此中间件从 URL 路径参数提取 workspace ID，从 auth.Identity 获取用户 ID，
// 然后从服务端 repository 查询成员关系和角色，判断是否有权执行操作。
// 绝不接受客户端声明的角色。
//
// 安全行为：
//   - 非 workspace 成员返回 404（不泄露 workspace 存在性）
//   - 成员但权限不足返回 403
//   - workspace 不存在返回 404
//   - 未认证返回 401（由前置 RequireAuth 处理）
//
// 参数：
//   - repo：workspace 数据访问接口
//   - permission：所需权限（如 PermSettings, PermMemberManage 等）
//   - paramKey：URL 路径参数中 workspace ID 的键名（默认 "id"）
func RequireWorkspacePermission(repo Repository, permission, paramKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 从 auth context 获取已认证身份
			id := auth.IdentityFromContext(r.Context())
			if id == nil {
				// 前置 RequireAuth 应已处理此情况，此处防御性检查
				writeWorkspaceError(w, http.StatusUnauthorized, "未认证")
				return
			}

			// 从 URL 路径提取 workspace ID
			workspaceID := r.PathValue(paramKey)
			if workspaceID == "" {
				writeWorkspaceError(w, http.StatusBadRequest, "缺少 workspace ID")
				return
			}

			// 验证 workspace 存在
			ws, err := repo.GetWorkspaceByID(r.Context(), workspaceID)
			if err != nil {
				if err == sql.ErrNoRows {
					// workspace 不存在——返回 404，不泄露存在性
					writeWorkspaceError(w, http.StatusNotFound, "workspace 不存在")
					return
				}
				slog.Error("查询 workspace 失败", "error", err, "workspace_id", workspaceID)
				writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
				return
			}

			// 查询当前用户在该 workspace 中的角色
			role, err := repo.GetMemberRole(r.Context(), workspaceID, id.UserID)
			if err != nil {
				if err == sql.ErrNoRows {
					// 非成员——返回 404，不泄露 workspace 存在性
					// 选择 404 而非 403：非成员不应知道 workspace 存在
					writeWorkspaceError(w, http.StatusNotFound, "workspace 不存在")
					return
				}
				slog.Error("查询成员角色失败", "error", err, "workspace_id", workspaceID, "user_id", id.UserID)
				writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
				return
			}

			// 检查权限
			if !HasPermission(role, permission) {
				slog.Info("workspace 权限不足",
					"workspace_id", workspaceID,
					"user_id", id.UserID,
					"role", role,
					"required_permission", permission,
				)
				writeWorkspaceError(w, http.StatusForbidden, "权限不足")
				return
			}

			// 权限验证通过——注入 WorkspaceContext 并继续
			wsc := &WorkspaceContext{
				WorkspaceID: ws.ID,
				MemberRole:  role,
			}
			r = r.WithContext(WithWorkspaceContext(r.Context(), wsc))
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSystemAdmin 返回一个中间件，验证当前已认证用户是 system_admin。
// 引入动机：admin API 仅 system_admin 可访问。
func RequireSystemAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := auth.IdentityFromContext(r.Context())
		if id == nil {
			writeWorkspaceError(w, http.StatusUnauthorized, "未认证")
			return
		}
		if id.SystemRole != SystemRoleAdmin {
			writeWorkspaceError(w, http.StatusForbidden, "需要系统管理员权限")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeWorkspaceError 写入统一 JSON 错误响应。
// 引入动机：workspace 模块中间件拒绝请求时使用统一格式。
// 安全要求：使用 encoding/json 编码消息，防止注入。
func writeWorkspaceError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		slog.Error("写入错误响应失败", "error", err)
	}
}

// writeWorkspaceJSON 写入 JSON 响应。
// 引入动机：workspace handler 共用此工具函数序列化响应体。
func writeWorkspaceJSON(w http.ResponseWriter, status int, body interface{}) {
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

// parsePagination 从 URL 查询参数解析分页参数。
// 引入动机：design/06-IMPLEMENTATION.md §工程要求 "所有列表和搜索分页"。
// 默认 limit=20, offset=0；limit 最大 100。
//
// 委托至 queryutil.ParsePagination，采用 Fail Fast 策略：
// 畸形 RawQuery（含 '?'）直接返回错误并记录日志，不做静默清洁。
//
// 参数：
//   - r：HTTP 请求
//
// 返回 limit, offset 和错误（非法参数时）。
func parsePagination(r *http.Request) (int, int, error) {
	return queryutil.ParsePagination(r)
}

// decodeJSONStrict 严格解码 JSON 请求体。
// 引入动机：所有 handler 需要拒绝畸形 JSON、未知字段、多 JSON 值。
//
// 参数：
//   - r：HTTP 请求
//   - dst：解码目标
//
// 返回错误（任何解析问题）。
func decodeJSONStrict(r *http.Request, dst interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	// 检查是否存在额外的 JSON 值（如 `{}{}`）
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体包含多个 JSON 值")
	}
	return nil
}
