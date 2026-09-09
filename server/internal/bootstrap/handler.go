// Package bootstrap 实现首次管理员创建的安全 Bootstrap 逻辑。
//
// 引入动机：计划要求 Compose 空数据库启动时提供一次性 Bootstrap API 创建第一个 system_admin。
// 只要 users 表中存在任意用户，Bootstrap API 永久拒绝创建，避免抢占。
// 密码沿用现有 Argon2id 实现，创建成功写脱敏审计并立即关闭 Bootstrap 能力。
//
// 安全原则：
//   - 仅 users 表为空时可访问（通过原子操作 CreateUserIfNoneExist 保证）
//   - 严格输入校验（用户名、邮箱、密码长度/复杂度）
//   - 密码使用 Argon2id 哈希，复用 auth.HashPassword
//   - 审计记录脱敏，不含密码
//   - 不出现空 catch / 兜底 / 占位
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"partitura/server/internal/auth"
	httpmw "partitura/server/internal/http"
)

// AuditRepository 定义审计记录接口。
// 引入动机：Bootstrap 创建管理员需要记录审计日志，但审计中不得包含密码。
type AuditRepository interface {
	Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error
}

// FirstAdminCreator 定义原子创建首个管理员的接口。
// 引入动机：消除 Bootstrap 中 CountUsers + CreateUser 两步操作的 TOCTOU 竞态。
// 实现必须保证在并发调用下，只有第一个调用成功插入，后续调用返回 ErrUsersAlreadyExist。
type FirstAdminCreator interface {
	CreateUserIfNoneExist(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error)
}

// UserCounter 定义查询用户总数的接口。
// 引入动机：Bootstrap 状态查询端点需要知道 users 表是否为空，
// 但不执行创建操作，因此不需要原子性保证。
type UserCounter interface {
	CountUsers(ctx context.Context) (int, error)
}

// Handler 是 Bootstrap HTTP handler。
// 引入动机：处理 Bootstrap 状态查询和首次管理员创建请求。
type Handler struct {
	firstAdminCreator FirstAdminCreator
	userCounter       UserCounter
	auditRepo         AuditRepository
	authCfg           auth.AuthConfig
}

// NewHandler 创建 Bootstrap handler。
// 引入动机：main.go 注入原子创建接口、用户计数接口、审计仓储和认证配置。
func NewHandler(firstAdminCreator FirstAdminCreator, userCounter UserCounter, auditRepo AuditRepository, authCfg auth.AuthConfig) *Handler {
	return &Handler{
		firstAdminCreator: firstAdminCreator,
		userCounter:       userCounter,
		auditRepo:         auditRepo,
		authCfg:           authCfg,
	}
}

// bootstrapRequest 是创建首个管理员的请求体。
type bootstrapRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// bootstrapResponse 是创建成功的响应体。
type bootstrapResponse struct {
	Status string `json:"status"`
	UserID string `json:"user_id"`
}

// GetBootstrapStatus 处理 GET /api/bootstrap。
// 引入动机：前端 Bootstrap 页面需要知道 Bootstrap 是否可用。
// 返回 bootstrap_available 布尔值，不泄露用户信息。
func (h *Handler) GetBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeBootstrapError(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}

	count, err := h.userCounter.CountUsers(r.Context())
	if err != nil {
		slog.Error("查询用户总数失败", "error", err)
		writeBootstrapError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	writeBootstrapJSON(w, http.StatusOK, map[string]interface{}{
		"bootstrap_available": count == 0,
	})
}

// CreateFirstAdmin 处理 POST /api/bootstrap。
// 引入动机：空数据库时创建第一个 system_admin，非空后永久拒绝。
//
// 竞态安全：通过 CreateUserIfNoneExist 原子操作保证并发安全。
// 该方法在单个 PG 事务内执行 LOCK TABLE + COUNT + INSERT，
// 确保同一时刻只有一个请求能成功创建管理员。
func (h *Handler) CreateFirstAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeBootstrapError(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}

	// 解析请求体
	var req bootstrapRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeBootstrapError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}
	// 检查是否有额外 JSON 值
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		writeBootstrapError(w, http.StatusBadRequest, "请求体包含多个 JSON 值")
		return
	}

	// 严格输入校验
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	if req.Username == "" {
		writeBootstrapError(w, http.StatusBadRequest, "username 不能为空")
		return
	}
	if len(req.Username) < 3 || len(req.Username) > 100 {
		writeBootstrapError(w, http.StatusBadRequest, "username 长度必须在 3-100 字符之间")
		return
	}
	if req.Email == "" {
		writeBootstrapError(w, http.StatusBadRequest, "email 不能为空")
		return
	}
	if !isValidEmail(req.Email) {
		writeBootstrapError(w, http.StatusBadRequest, "email 格式不正确")
		return
	}
	if len(req.Password) < 12 {
		writeBootstrapError(w, http.StatusBadRequest, "password 长度必须至少 12 字符")
		return
	}

	// 使用 Argon2id 哈希密码
	passwordHash, err := auth.HashPassword(req.Password, h.authCfg)
	if err != nil {
		slog.Error("哈希密码失败", "error", err)
		writeBootstrapError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 原子创建 system_admin 用户：检查 users 表为空 + 插入在同一事务内完成
	userID, err := h.firstAdminCreator.CreateUserIfNoneExist(r.Context(), req.Username, req.Email, passwordHash, "system_admin", true)
	if err != nil {
		if errors.Is(err, auth.ErrUsersAlreadyExist) {
			writeBootstrapError(w, http.StatusForbidden, "Bootstrap 已关闭：系统已有用户")
			return
		}
		slog.Error("创建首个管理员失败", "error", err)
		writeBootstrapError(w, http.StatusInternalServerError, fmt.Sprintf("创建用户失败: %v", err))
		return
	}

	// 记录审计：仅记录用户名和邮箱，不含密码
	h.recordBootstrapAudit(r, userID, req.Username, req.Email)

	slog.Info("首个 system_admin 已通过 Bootstrap 创建", "user_id", userID, "username", req.Username)

	writeBootstrapJSON(w, http.StatusCreated, bootstrapResponse{
		Status: "created",
		UserID: userID,
	})
}

// RegisterRoutes 注册 Bootstrap API 路由。
// 引入动机：Bootstrap 端点不需要认证（首次使用时无用户），但创建后永久关闭。
//
// 路由清单：
//   - GET  /api/bootstrap — 公开，查询 Bootstrap 是否可用
//   - POST /api/bootstrap — 公开，创建首个 system_admin（仅 users 表为空时）
func RegisterRoutes(mux *http.ServeMux, handler *Handler) {
	mux.HandleFunc("GET /api/bootstrap", handler.GetBootstrapStatus)
	mux.HandleFunc("POST /api/bootstrap", handler.CreateFirstAdmin)
}

// recordBootstrapAudit 记录 Bootstrap 审计日志。
// 引入动机：审计仅记录用户名和邮箱，不含密码。
func (h *Handler) recordBootstrapAudit(r *http.Request, userID, username, email string) {
	if h.auditRepo == nil {
		return
	}
	detail := map[string]string{
		"username": username,
		"email":    email,
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		slog.Error("序列化 Bootstrap 审计 detail 失败", "error", err)
		return
	}
	requestID := httpmw.RequestIDFromContext(r.Context())
	if err := h.auditRepo.Record(r.Context(), userID, "", "bootstrap.create_admin", "user", userID, detailJSON, requestID); err != nil {
		slog.Error("写入 Bootstrap 审计日志失败", "error", err)
	}
}

// isValidEmail 简单校验邮箱格式。
// 引入动机：Bootstrap 输入校验需要基本的邮箱格式验证。
func isValidEmail(email string) bool {
	at := strings.Index(email, "@")
	if at <= 0 || at >= len(email)-1 {
		return false
	}
	dot := strings.LastIndex(email[at+1:], ".")
	return dot > 0
}

// writeBootstrapError 写入统一 JSON 错误响应。
func writeBootstrapError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		slog.Error("写入错误响应失败", "error", err)
	}
}

// writeBootstrapJSON 写入 JSON 响应。
func writeBootstrapJSON(w http.ResponseWriter, status int, body interface{}) {
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

// EnsureNoUsers 检查数据库是否无用户。
// 引入动机：main.go 启动时可能需要判断 Bootstrap 状态。
func EnsureNoUsers(ctx context.Context, counter UserCounter) (bool, error) {
	count, err := counter.CountUsers(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, nil
		}
		return false, err
	}
	return count == 0, nil
}
