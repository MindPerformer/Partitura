// admin_handler.go 实现 system_admin API 的 HTTP handler。
//
// 引入动机：design/04-WEB-API.md §Admin 要求 users、workspace permissions、audit 等 API。
// 所有 admin API 严格限 system_admin 访问——RequireSystemAdmin 中间件已处理。
//
// 端点清单：
//   - GET  /api/admin/users              — system_admin，分页用户列表
//   - POST /api/admin/users              — system_admin + CSRF，创建普通用户
//   - PUT  /api/admin/users/{id}         — system_admin，更新用户系统角色和 workspace:create 权限
//   - GET  /api/admin/workspaces         — system_admin，分页全部 workspace 列表
//   - GET  /api/admin/audit              — system_admin，分页审计日志列表
package workspace

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"partitura/server/internal/audit"
	"partitura/server/internal/auth"
	httpmw "partitura/server/internal/http"
)

// AdminHandler 是系统管理员 API 的 HTTP handler 集合。
// 引入动机：将 admin 相关的 HTTP handler 集中在一个结构体中，
// 复用 workspace 模块的 repository，并注入 audit repository 供审计日志查询。
//
// authRepo 和 authCfg 用于创建用户时调用 auth.Repository.CreateUser 和 auth.HashPassword，
// 确保 Argon2id 密码哈希与现有认证流程一致。
type AdminHandler struct {
	repo      Repository
	auditRepo audit.Repository
	authRepo  auth.Repository
	authCfg   auth.AuthConfig
}

// adminListUsersResponse 是用户列表响应。
type adminListUsersResponse struct {
	Users  []adminUserResponse `json:"users"`
	Total  int                 `json:"total"`
	Limit  int                 `json:"limit"`
	Offset int                 `json:"offset"`
}

// adminUserResponse 是返回给客户端的用户 JSON 结构（含系统信息）。
type adminUserResponse struct {
	ID                  string `json:"id"`
	Username            string `json:"username"`
	Email               string `json:"email"`
	SystemRole          string `json:"system_role"`
	WorkspaceCreatePerm bool   `json:"workspace_create_perm"`
}

// adminListWorkspacesResponse 是全部 workspace 列表响应。
type adminListWorkspacesResponse struct {
	Workspaces []workspaceResponse `json:"workspaces"`
	Total      int                 `json:"total"`
	Limit      int                 `json:"limit"`
	Offset     int                 `json:"offset"`
}

// adminListAuditResponse 是审计日志列表响应。
type adminListAuditResponse struct {
	Entries []auditEntryResponse `json:"entries"`
	Total   int                  `json:"total"`
	Limit   int                  `json:"limit"`
	Offset  int                  `json:"offset"`
}

// auditEntryResponse 是返回给客户端的审计日志 JSON 结构。
type auditEntryResponse struct {
	ID           int64           `json:"id"`
	UserID       string          `json:"user_id"`
	WorkspaceID  string          `json:"workspace_id"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	Detail       json.RawMessage `json:"detail"`
	RequestID    string          `json:"request_id"`
	CreatedAt    string          `json:"created_at"`
}

// adminUpdateUserRequest 是更新用户系统信息的请求体。
// 使用指针类型区分"未提供"和"零值"。
type adminUpdateUserRequest struct {
	SystemRole          *string `json:"system_role"`
	WorkspaceCreatePerm *bool   `json:"workspace_create_perm"`
}

// NewAdminHandler 创建 admin handler。
// 引入动机：main.go 通过此构造函数注入 repository 和 audit repository。
func NewAdminHandler(repo Repository, auditRepo audit.Repository) *AdminHandler {
	return &AdminHandler{repo: repo, auditRepo: auditRepo}
}

// NewAdminHandlerWithAuth 创建 admin handler 并注入 auth repository 和配置。
// 引入动机：创建用户 API 需要调用 auth.Repository.CreateUser 和 auth.HashPassword，
// 必须注入 auth 依赖以保证密码哈希与登录流程一致。
func NewAdminHandlerWithAuth(repo Repository, auditRepo audit.Repository, authRepo auth.Repository, authCfg auth.AuthConfig) *AdminHandler {
	return &AdminHandler{repo: repo, auditRepo: auditRepo, authRepo: authRepo, authCfg: authCfg}
}

// adminCreateUserRequest 是创建用户的请求体。
// 引入动机：system_admin 通过 POST /api/admin/users 创建普通用户，
// 请求仅接受用户名、邮箱和初始密码，以及可选的受限 system_role 和 workspace_create_perm。
// 默认安全值为 system_role=user、workspace_create_perm=false。
type adminCreateUserRequest struct {
	Username            string `json:"username"`
	Email               string `json:"email"`
	Password            string `json:"password"`
	SystemRole          string `json:"system_role"`
	WorkspaceCreatePerm bool   `json:"workspace_create_perm"`
}

// adminCreateUserResponse 是创建用户成功的响应体。
type adminCreateUserResponse struct {
	ID                  string `json:"id"`
	Username            string `json:"username"`
	Email               string `json:"email"`
	SystemRole          string `json:"system_role"`
	WorkspaceCreatePerm bool   `json:"workspace_create_perm"`
}

// CreateUser 处理 POST /api/admin/users。
//
// 动机/职责：仅 system_admin 可创建普通用户，复用 Argon2id 密码哈希和 auth.Repository.CreateUser。
//
// 安全约束：
//   - 仅 system_admin 可访问（RequireSystemAdmin 中间件已处理）
//   - 需要 CSRF token（RequireCSRF 中间件已处理）
//   - 请求体严格 JSON 解码，拒绝未知字段
//   - 密码不记录在审计日志中，审计仅记录用户名、邮箱、角色和权限
//   - 用户名/邮箱唯一性冲突返回 409，不泄露密码
//   - 默认 system_role=user、workspace_create_perm=false
//   - 不允许通过此端点创建 system_admin（防止权限提升）
func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if h.authRepo == nil {
		slog.Error("auth repository 未注入，无法创建用户")
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	var req adminCreateUserRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	// 输入校验
	if req.Username == "" || len(req.Username) > 100 {
		writeWorkspaceError(w, http.StatusBadRequest, "用户名不能为空且不超过 100 字符")
		return
	}
	if req.Email == "" || len(req.Email) > 255 {
		writeWorkspaceError(w, http.StatusBadRequest, "邮箱不能为空且不超过 255 字符")
		return
	}
	if !strings.Contains(req.Email, "@") {
		writeWorkspaceError(w, http.StatusBadRequest, "邮箱格式不正确")
		return
	}
	if len(req.Password) < 8 {
		writeWorkspaceError(w, http.StatusBadRequest, "密码长度至少 8 个字符")
		return
	}

	// 安全约束：此端点不允许创建 system_admin，防止权限提升
	systemRole := req.SystemRole
	if systemRole == "" {
		systemRole = SystemRoleUser
	}
	if systemRole != SystemRoleUser {
		writeWorkspaceError(w, http.StatusBadRequest, "此端点仅允许创建普通用户")
		return
	}

	// 密码哈希
	passwordHash, err := auth.HashPassword(req.Password, h.authCfg)
	if err != nil {
		slog.Error("创建用户时密码哈希失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 创建用户
	userID, err := h.authRepo.CreateUser(r.Context(), req.Username, req.Email, passwordHash, systemRole, req.WorkspaceCreatePerm)
	if err != nil {
		// 检查是否为唯一约束冲突
		if isDuplicateKeyErr(err) {
			writeWorkspaceError(w, http.StatusConflict, "用户名或邮箱已存在")
			return
		}
		slog.Error("创建用户失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 审计日志：记录创建用户事件，严禁包含密码
	h.recordCreateUserAudit(r, userID, req.Username, req.Email, systemRole, req.WorkspaceCreatePerm)

	resp := adminCreateUserResponse{
		ID:                  userID,
		Username:            req.Username,
		Email:               req.Email,
		SystemRole:          systemRole,
		WorkspaceCreatePerm: req.WorkspaceCreatePerm,
	}

	writeWorkspaceJSON(w, http.StatusCreated, resp)
}

// recordCreateUserAudit 记录管理员创建用户的审计事件。
// 引入动机：创建用户是安全敏感操作，必须可追溯。
// 审计 detail 仅包含新用户 ID、用户名、邮箱、角色和权限，严禁包含密码或完整请求体。
func (h *AdminHandler) recordCreateUserAudit(r *http.Request, userID, username, email, systemRole string, workspaceCreatePerm bool) {
	if h.auditRepo == nil {
		return
	}

	id := auth.IdentityFromContext(r.Context())
	actorID := ""
	if id != nil {
		actorID = id.UserID
	}

	// detail 严禁包含密码、session、token 或 CSRF
	detail := map[string]interface{}{
		"target_user_id":        userID,
		"target_username":       username,
		"target_email":           email,
		"system_role":           systemRole,
		"workspace_create_perm": workspaceCreatePerm,
	}

	detailJSON, err := json.Marshal(detail)
	if err != nil {
		slog.Error("序列化 admin.user.create 审计 detail 失败", "error", err)
		return
	}

	requestID := httpmw.RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = r.Header.Get("X-Request-ID")
	}
	if err := h.auditRepo.Record(r.Context(), actorID, "", "admin.user.create", "user", userID, detailJSON, requestID); err != nil {
		slog.Error("写入 admin.user.create 审计日志失败", "error", err, "target_user_id", userID)
	}
}

// isDuplicateKeyErr 判断错误是否为唯一约束冲突。
// 引入动机：auth.Repository.CreateUser 的唯一约束冲突需要映射为 409 Conflict，
// 而非 500 Internal Server Error。
// 兼容 PG pgconn.PGConstraintViolation 和 mock 实现的错误消息。
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "唯一") ||
		strings.Contains(msg, "已存在") ||
		strings.Contains(msg, "unique constraint")
}

// ListUsers 处理 GET /api/admin/users。
// 引入动机：design/04-WEB-API.md §Admin 要求 users endpoint。
// 仅 system_admin 可访问——RequireSystemAdmin 中间件已处理。
// 使用单条参数化 SQL 一次性返回用户基础信息、系统角色和 workspace:create 权限，避免 N+1 查询。
func (h *AdminHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.repo.ListAllUsersWithSystemInfo(r.Context(), limit, offset)
	if err != nil {
		slog.Error("查询用户列表（含系统信息）失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	users := make([]adminUserResponse, 0, len(result.Users))
	for _, u := range result.Users {
		users = append(users, adminUserResponse{
			ID:                  u.UserID,
			Username:            u.Username,
			Email:               u.Email,
			SystemRole:          u.SystemRole,
			WorkspaceCreatePerm: u.WorkspaceCreatePerm,
		})
	}

	resp := adminListUsersResponse{
		Users:  users,
		Total:  result.Total,
		Limit:  limit,
		Offset: offset,
	}

	writeWorkspaceJSON(w, http.StatusOK, resp)
}

// UpdateUser 处理 PUT /api/admin/users/{id}。
// 引入动机：design/04-WEB-API.md §Admin 要求更新用户 system_role 和 workspace_create_perm。
// 仅 system_admin 可访问。请求不能自赋 system_admin。
func (h *AdminHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	targetUserID := r.PathValue("id")
	if targetUserID == "" {
		writeWorkspaceError(w, http.StatusBadRequest, "缺少用户 ID")
		return
	}

	var req adminUpdateUserRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	// 验证 system_role 如果提供了
	if req.SystemRole != nil {
		if *req.SystemRole != SystemRoleAdmin && *req.SystemRole != SystemRoleUser {
			writeWorkspaceError(w, http.StatusBadRequest, "非法 system_role")
			return
		}
	}

	// 执行更新
	if req.SystemRole != nil {
		if err := h.repo.UpdateUserSystemRole(r.Context(), targetUserID, *req.SystemRole); err != nil {
			if err == sql.ErrNoRows {
				writeWorkspaceError(w, http.StatusNotFound, "用户不存在")
				return
			}
			slog.Error("更新用户系统角色失败", "error", err, "target_user_id", targetUserID)
			writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
			return
		}
	}

	if req.WorkspaceCreatePerm != nil {
		if err := h.repo.UpdateUserWorkspaceCreatePerm(r.Context(), targetUserID, *req.WorkspaceCreatePerm); err != nil {
			if err == sql.ErrNoRows {
				writeWorkspaceError(w, http.StatusNotFound, "用户不存在")
				return
			}
			slog.Error("更新用户 workspace:create 权限失败", "error", err, "target_user_id", targetUserID)
			writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
			return
		}
	}

	// 返回更新后的用户信息
	systemRole, wsCreatePerm, err := h.repo.GetUserSystemInfo(r.Context(), targetUserID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "用户不存在")
			return
		}
		slog.Error("查询更新后用户信息失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	targetUser, err := h.repo.GetUserByID(r.Context(), targetUserID)
	if err != nil {
		slog.Error("查询更新后用户基本信息失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 记录审计日志：管理员更新用户安全敏感状态是安全变更，必须可追溯。
	// detail 仅包含变更后的角色/权限和目标用户标识，严禁记录密码、session、token 或 CSRF。
	h.recordAdminAudit(r, targetUserID, targetUser.Username, systemRole, wsCreatePerm)

	resp := adminUserResponse{
		ID:                  targetUserID,
		Username:            targetUser.Username,
		Email:               targetUser.Email,
		SystemRole:          systemRole,
		WorkspaceCreatePerm: wsCreatePerm,
	}

	writeWorkspaceJSON(w, http.StatusOK, resp)
}

// recordAdminAudit 记录管理员更新用户安全敏感状态的审计事件。
// 引入动机：管理员修改用户 system_role 或 workspace:create 权限是安全敏感状态变更，
// design/00-MASTER.md §核心数据原则要求 audit 记录所有安全敏感操作。
// 审计写失败不回滚已成功的业务变更，但必须通过 slog 记录错误。
//
// 参数：
//   - r：HTTP 请求（用于提取 actor identity 和 request_id）
//   - targetUserID：被修改的目标用户 ID
//   - targetUsername：被修改的目标用户名
//   - systemRole：变更后的系统角色
//   - workspaceCreatePerm：变更后的 workspace:create 权限
func (h *AdminHandler) recordAdminAudit(r *http.Request, targetUserID, targetUsername, systemRole string, workspaceCreatePerm bool) {
	if h.auditRepo == nil {
		return
	}

	id := auth.IdentityFromContext(r.Context())
	actorID := ""
	if id != nil {
		actorID = id.UserID
	}

	// detail 仅记录变更后的角色/权限和目标用户标识，严禁包含密码、session、token 或 CSRF。
	detail := map[string]interface{}{
		"target_user_id":        targetUserID,
		"target_username":       targetUsername,
		"system_role":           systemRole,
		"workspace_create_perm": workspaceCreatePerm,
	}

	detailJSON, err := json.Marshal(detail)
	if err != nil {
		slog.Error("序列化 admin.user.update 审计 detail 失败", "error", err)
		return
	}

	requestID := httpmw.RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = r.Header.Get("X-Request-ID")
	}
	if err := h.auditRepo.Record(r.Context(), actorID, "", "admin.user.update", "user", targetUserID, detailJSON, requestID); err != nil {
		slog.Error("写入 admin.user.update 审计日志失败", "error", err, "target_user_id", targetUserID)
	}
}
// ListAllWorkspaces 处理 GET /api/admin/workspaces。
// 引入动机：design/04-WEB-API.md §Admin 要求 workspace permissions 管理，需要列出全部 workspace 供管理员查看。
// 仅 system_admin 可访问——RequireSystemAdmin 中间件已处理。
// 返回全部 workspace 不受成员关系限制，支持分页。
func (h *AdminHandler) ListAllWorkspaces(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.repo.ListAllWorkspaces(r.Context(), limit, offset)
	if err != nil {
		slog.Error("查询全部 workspace 列表失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := adminListWorkspacesResponse{
		Workspaces: make([]workspaceResponse, 0, len(result.Workspaces)),
		Total:      result.Total,
		Limit:      limit,
		Offset:     offset,
	}
	for i := range result.Workspaces {
		resp.Workspaces = append(resp.Workspaces, toWorkspaceResponse(&result.Workspaces[i]))
	}

	writeWorkspaceJSON(w, http.StatusOK, resp)
}

// ListAuditLogs 处理 GET /api/admin/audit。
// 引入动机：design/04-WEB-API.md §Admin 列出 audit endpoint。
// 仅 system_admin 可读取审计日志。
func (h *AdminHandler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	if h.auditRepo == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "审计模块未配置")
		return
	}

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.auditRepo.List(r.Context(), limit, offset)
	if err != nil {
		slog.Error("查询审计日志失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := adminListAuditResponse{
		Entries: make([]auditEntryResponse, 0, len(result.Entries)),
		Total:   result.Total,
		Limit:   limit,
		Offset:  offset,
	}
	for _, e := range result.Entries {
		resp.Entries = append(resp.Entries, auditEntryResponse{
			ID:           e.ID,
			UserID:       e.UserID,
			WorkspaceID:  e.WorkspaceID,
			Action:       e.Action,
			ResourceType: e.ResourceType,
			ResourceID:   e.ResourceID,
			Detail:       e.Detail,
			RequestID:    e.RequestID,
			CreatedAt:    e.CreatedAt,
		})
	}

	writeWorkspaceJSON(w, http.StatusOK, resp)
}
