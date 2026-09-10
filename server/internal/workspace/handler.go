// handler.go 实现 workspace 模块的 HTTP handler。
//
// 引入动机：design/04-WEB-API.md §Workspace 要求 list/get/create/update/archive/members
// 等 API。handler 只负责 HTTP 输入输出（解析请求、写入响应），
// 领域规则、权限判断、数据访问分别由 rbac.go 和 repository.go 处理。
//
// 端点清单：
//   - GET    /api/workspaces             — 已认证，返回当前用户成员的 workspace 列表
//   - POST   /api/workspaces             — 已认证 + workspace:create，创建 workspace
//   - GET    /api/workspaces/{id}        — member，返回 workspace 详情
//   - PUT    /api/workspaces/{id}        — workspace admin+，更新 workspace
//   - POST   /api/workspaces/{id}/archive — workspace admin+，归档 workspace
//   - GET    /api/workspaces/{id}/members — member，返回成员列表
//   - POST   /api/workspaces/{id}/members — workspace admin+，添加成员
//   - PUT    /api/workspaces/{id}/members/{userId} — workspace admin+，修改成员角色
//   - DELETE /api/workspaces/{id}/members/{userId} — workspace admin+，移除成员
package workspace

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"partitura/server/internal/audit"
	"partitura/server/internal/auth"
	httpmw "partitura/server/internal/http"
)

// Handler 是 workspace 模块的 HTTP handler 集合。
// 引入动机：将 workspace 相关的 HTTP handler 集中在一个结构体中，
// 通过依赖注入接收 repository 和 audit repository，保持 handler 轻量。
type Handler struct {
	repo        Repository
	auditRepo   audit.Repository
	initializer SpecialDocumentsInitializer
	jobEnq      TxJobEnqueuer
}

// NewHandler 创建 workspace handler。
// 引入动机：main.go 通过此构造函数注入 repository 和 audit repository。
func NewHandler(repo Repository, auditRepo audit.Repository) *Handler {
	return &Handler{repo: repo, auditRepo: auditRepo}
}

// NewHandlerWithInitializer 创建 workspace handler 并注入特殊文件初始化器。
// 引入动机：Phase 2 需要在 workspace 创建时原子地初始化 PROJECT.md 和 AGENTS.md。
// 初始化器由 document 模块提供，通过此构造函数注入到 workspace handler。
// C1：jobEnq 非空时，workspace 创建事务中会为特殊文档 enqueue index job。
func NewHandlerWithInitializer(repo Repository, auditRepo audit.Repository, initializer SpecialDocumentsInitializer, jobEnq TxJobEnqueuer) *Handler {
	return &Handler{repo: repo, auditRepo: auditRepo, initializer: initializer, jobEnq: jobEnq}
}

// --- 请求/响应类型定义 ---

// workspaceResponse 是返回给客户端的 workspace JSON 结构。
// 引入动机：统一 workspace API 响应格式，不暴露内部实现细节。
type workspaceResponse struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	DisplayName           string `json:"display_name"`
	Description           string `json:"description"`
	Status                string `json:"status"`
	RevisionRetentionDays int    `json:"revision_retention_days"`
	RevisionMaxCount      int    `json:"revision_max_count"`
	MaxDocumentSizeBytes  int    `json:"max_document_size_bytes"`
	CreatedBy             string `json:"created_by"`
}

// memberResponse 是返回给客户端的成员 JSON 结构。
type memberResponse struct {
	ID       string `json:"id"`
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
}

// listWorkspacesResponse 是 workspace 列表响应。
type listWorkspacesResponse struct {
	Workspaces []workspaceResponse `json:"workspaces"`
	Total      int                 `json:"total"`
	Limit      int                 `json:"limit"`
	Offset     int                 `json:"offset"`
}

// listMembersResponse 是成员列表响应。
type listMembersResponse struct {
	Members []memberResponse `json:"members"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
}

// memberCandidatesResponse 是成员搜索候选响应。
type memberCandidatesResponse struct {
	Users []memberResponse `json:"users"`
}

// createWorkspaceRequest 是创建 workspace 的请求体。
type createWorkspaceRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// updateWorkspaceRequest 是更新 workspace 的请求体。
// 引入动机：Phase6 要求 workspace admin 可更新 display_name/description 和 settings，
// 所有字段使用指针以实现 optional 部分更新，服务端校验范围与 CSRF。
type updateWorkspaceRequest struct {
	DisplayName           *string `json:"display_name"`
	Description           *string `json:"description"`
	RevisionRetentionDays *int    `json:"revision_retention_days"`
	RevisionMaxCount      *int    `json:"revision_max_count"`
	MaxDocumentSizeBytes  *int    `json:"max_document_size_bytes"`
}

// addMemberRequest 是添加成员的请求体。
// 引入动机：前端不应要求用户处理内部 UUID，服务端根据用户名解析目标用户。
type addMemberRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// updateMemberRoleRequest 是修改成员角色的请求体。
type updateMemberRoleRequest struct {
	Role string `json:"role"`
}

// toWorkspaceResponse 将 Workspace 结构体转换为 API 响应。
func toWorkspaceResponse(ws *Workspace) workspaceResponse {
	return workspaceResponse{
		ID:                    ws.ID,
		Name:                  ws.Name,
		DisplayName:           ws.DisplayName,
		Description:           ws.Description,
		Status:                ws.Status,
		RevisionRetentionDays: ws.RevisionRetentionDays,
		RevisionMaxCount:      ws.RevisionMaxCount,
		MaxDocumentSizeBytes:  ws.MaxDocumentSizeBytes,
		CreatedBy:             ws.CreatedBy,
	}
}

// toMemberResponse 将 Member 结构体转换为 API 响应。
func toMemberResponse(m *Member) memberResponse {
	return memberResponse{
		ID:       m.ID,
		UserID:   m.UserID,
		Username: m.Username,
		Email:    m.Email,
		Role:     m.Role,
	}
}

// --- Handler 方法 ---

// ListWorkspaces 处理 GET /api/workspaces。
// 引入动机：design/04-WEB-API.md §Workspace 要求 list endpoint。
// 仅返回当前用户作为成员的 workspace，必须分页。
func (h *Handler) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeWorkspaceError(w, http.StatusUnauthorized, "未认证")
		return
	}

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.repo.ListWorkspacesByUser(r.Context(), id.UserID, limit, offset)
	if err != nil {
		slog.Error("查询用户 workspace 列表失败", "error", err, "user_id", id.UserID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := listWorkspacesResponse{
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

// CreateWorkspace 处理 POST /api/workspaces。
// 引入动机：design/04-WEB-API.md §Workspace 要求 create endpoint。
// 需要 workspace:create 权限；创建 workspace 并在同一事务创建 owner membership
// 和 PROJECT.md / AGENTS.md 特殊文件（Phase 2）。
func (h *Handler) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeWorkspaceError(w, http.StatusUnauthorized, "未认证")
		return
	}

	// 检查 workspace:create 权限
	if id.SystemRole != SystemRoleAdmin && !id.WorkspaceCreatePerm {
		h.recordAudit(r, id.UserID, "", "workspace.create.denied", "workspace", "", map[string]string{"reason": "missing workspace:create permission"})
		writeWorkspaceError(w, http.StatusForbidden, "缺少 workspace:create 权限")
		return
	}

	var req createWorkspaceRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.Name == "" {
		writeWorkspaceError(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if req.DisplayName == "" {
		writeWorkspaceError(w, http.StatusBadRequest, "display_name 不能为空")
		return
	}

	var ws *Workspace
	var err error
	if h.initializer != nil {
		ws, err = h.repo.CreateWorkspaceWithInitializer(r.Context(), req.Name, req.DisplayName, req.Description, id.UserID, h.initializer, h.jobEnq)
	} else {
		ws, err = h.repo.CreateWorkspace(r.Context(), req.Name, req.DisplayName, req.Description, id.UserID)
	}
	if err != nil {
		if isDuplicateKeyError(err) {
			writeWorkspaceError(w, http.StatusConflict, "workspace 名称已存在")
			return
		}
		slog.Error("创建 workspace 失败", "error", err, "user_id", id.UserID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 审计日志
	h.recordAudit(r, id.UserID, ws.ID, "workspace.create", "workspace", ws.ID, map[string]string{
		"name":         ws.Name,
		"display_name": ws.DisplayName,
	})

	writeWorkspaceJSON(w, http.StatusCreated, toWorkspaceResponse(ws))
}

// GetWorkspace 处理 GET /api/workspaces/{id}。
// 引入动机：design/04-WEB-API.md §Workspace 要求 get endpoint。
// 非成员不能通过猜测 UUID 获取——RequireWorkspacePermission 中间件已处理。
// 此 handler 在权限验证通过后返回 workspace 详情。
func (h *Handler) GetWorkspace(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	ws, err := h.repo.GetWorkspaceByID(r.Context(), wsc.WorkspaceID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "workspace 不存在")
			return
		}
		slog.Error("查询 workspace 失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	writeWorkspaceJSON(w, http.StatusOK, toWorkspaceResponse(ws))
}

// GetMyMembership 处理 GET /api/workspaces/{id}/me/membership。
// 引入动机：Web 需要直接获取当前用户在该 workspace 中的角色，
// 避免反查 members 第一页推断本人角色，避免 membership 分页缺失导致误判。
// 权限：member——RequireWorkspacePermission 已验证 MemberRole 非空。
func (h *Handler) GetMyMembership(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil || wsc.MemberRole == "" {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace membership 上下文缺失")
		return
	}

	writeWorkspaceJSON(w, http.StatusOK, map[string]string{
		"workspace_id": wsc.WorkspaceID,
		"role":         wsc.MemberRole,
	})
}

// UpdateWorkspace 处理 PUT /api/workspaces/{id}。
// 引入动机：design/04-WEB-API.md §Workspace 要求 update endpoint。
// Phase6 扩展：支持 workspace admin 安全更新 display_name、description 与三项 settings。
// 所有字段 optional，服务端校验范围；需要 workspace admin+ 权限（PermSettings）
// 和 CSRF（RequireWorkspacePermission 已处理权限，Auth 模块负责 CSRF）。
func (h *Handler) UpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeWorkspaceError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req updateWorkspaceRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	// 至少一个字段显式提供；否则 400。
	if req.DisplayName == nil && req.Description == nil &&
		req.RevisionRetentionDays == nil && req.RevisionMaxCount == nil && req.MaxDocumentSizeBytes == nil {
		writeWorkspaceError(w, http.StatusBadRequest, "请求体中没有可更新字段")
		return
	}

	if req.DisplayName != nil {
		*req.DisplayName = strings.TrimSpace(*req.DisplayName)
		if *req.DisplayName == "" {
			writeWorkspaceError(w, http.StatusBadRequest, "display_name 不能为空")
			return
		}
	}

	if req.RevisionRetentionDays != nil {
		if *req.RevisionRetentionDays < MinRevisionRetentionDays || *req.RevisionRetentionDays > MaxRevisionRetentionDays {
			writeWorkspaceError(w, http.StatusBadRequest, fmt.Sprintf("revision_retention_days 必须在 %d 到 %d 之间", MinRevisionRetentionDays, MaxRevisionRetentionDays))
			return
		}
	}
	if req.RevisionMaxCount != nil {
		if *req.RevisionMaxCount < MinRevisionMaxCount || *req.RevisionMaxCount > MaxRevisionMaxCount {
			writeWorkspaceError(w, http.StatusBadRequest, fmt.Sprintf("revision_max_count 必须在 %d 到 %d 之间", MinRevisionMaxCount, MaxRevisionMaxCount))
			return
		}
	}
	if req.MaxDocumentSizeBytes != nil {
		if *req.MaxDocumentSizeBytes < MinDocumentSizeBytes || *req.MaxDocumentSizeBytes > MaxDocumentSizeBytes {
			writeWorkspaceError(w, http.StatusBadRequest, fmt.Sprintf("max_document_size_bytes 必须在 %d 到 %d 之间", MinDocumentSizeBytes, MaxDocumentSizeBytes))
			return
		}
	}

	opts := UpdateWorkspaceSettingsOptions{
		DisplayName:           req.DisplayName,
		Description:           req.Description,
		RevisionRetentionDays: req.RevisionRetentionDays,
		RevisionMaxCount:      req.RevisionMaxCount,
		MaxDocumentSizeBytes:  req.MaxDocumentSizeBytes,
	}

	ws, err := h.repo.UpdateWorkspaceSettings(r.Context(), wsc.WorkspaceID, opts)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "workspace 不存在")
			return
		}
		slog.Error("更新 workspace 设置失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 审计 detail 只记录变更字段与业务允许的值，绝不包含敏感信息。
	detail := map[string]string{}
	if req.DisplayName != nil {
		detail["display_name"] = *req.DisplayName
	}
	if req.Description != nil {
		detail["description"] = *req.Description
	}
	if req.RevisionRetentionDays != nil {
		detail["revision_retention_days"] = fmt.Sprintf("%d", *req.RevisionRetentionDays)
	}
	if req.RevisionMaxCount != nil {
		detail["revision_max_count"] = fmt.Sprintf("%d", *req.RevisionMaxCount)
	}
	if req.MaxDocumentSizeBytes != nil {
		detail["max_document_size_bytes"] = fmt.Sprintf("%d", *req.MaxDocumentSizeBytes)
	}

	h.recordAudit(r, id.UserID, ws.ID, "workspace.update", "workspace", ws.ID, detail)

	writeWorkspaceJSON(w, http.StatusOK, toWorkspaceResponse(ws))
}

// ArchiveWorkspace 处理 POST /api/workspaces/{id}/archive。
// 引入动机：design/04-WEB-API.md §Workspace 要求 archive endpoint。
// 需要 workspace admin+ 权限（PermArchive）——RequireWorkspacePermission 已处理。
func (h *Handler) ArchiveWorkspace(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	ws, err := h.repo.ArchiveWorkspace(r.Context(), wsc.WorkspaceID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "workspace 不存在")
			return
		}
		slog.Error("归档 workspace 失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordAudit(r, id.UserID, ws.ID, "workspace.archive", "workspace", ws.ID, nil)

	writeWorkspaceJSON(w, http.StatusOK, toWorkspaceResponse(ws))
}

// GetWorkspaceStats 处理 GET /api/workspaces/{id}/stats。
// 引入动机：Phase6 WP2 工作台首页需要聚合 workspace 文档/成员/近期 revision 统计。
// 需要 member 权限——RequireWorkspacePermission 使用 PermRead 验证。
func (h *Handler) GetWorkspaceStats(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	stats, err := h.repo.GetWorkspaceStats(r.Context(), wsc.WorkspaceID)
	if err != nil {
		slog.Error("查询 workspace 统计失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	writeWorkspaceJSON(w, http.StatusOK, map[string]interface{}{
		"stats": stats,
	})
}

// ListMembers 处理 GET /api/workspaces/{id}/members。
// 引入动机：design/04-WEB-API.md §Workspace 要求 members endpoint。
// 需要 member 权限——RequireWorkspacePermission 使用 PermRead（最低权限）验证。
func (h *Handler) ListMembers(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	limit, offset, err := parsePagination(r)
	if err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.repo.ListMembers(r.Context(), wsc.WorkspaceID, limit, offset)
	if err != nil {
		slog.Error("查询成员列表失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := listMembersResponse{
		Members: make([]memberResponse, 0, len(result.Members)),
		Total:   result.Total,
		Limit:   limit,
		Offset:  offset,
	}
	for i := range result.Members {
		resp.Members = append(resp.Members, toMemberResponse(&result.Members[i]))
	}

	writeWorkspaceJSON(w, http.StatusOK, resp)
}

// ListMemberCandidates 处理 GET /api/workspaces/{id}/members/candidates。
// 只返回尚未加入当前 workspace 的非敏感用户候选。
func (h *Handler) ListMemberCandidates(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeWorkspaceError(w, http.StatusBadRequest, "q 不能为空")
		return
	}
	limit, _, err := parsePagination(r)
	if err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, err.Error())
		return
	}
	candidates, err := h.repo.ListMemberCandidates(r.Context(), wsc.WorkspaceID, query, limit)
	if err != nil {
		slog.Error("查询成员候选失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}
	resp := memberCandidatesResponse{Users: make([]memberResponse, 0, len(candidates))}
	for i := range candidates {
		resp.Users = append(resp.Users, toMemberResponse(&candidates[i]))
	}
	writeWorkspaceJSON(w, http.StatusOK, resp)
}

// AddMember 处理 POST /api/workspaces/{id}/members。
// 引入动机：design/04-WEB-API.md §Workspace 要求 members endpoint。
// 需要 workspace admin+ 权限（PermMemberManage）——RequireWorkspacePermission 已处理。
// 拒绝重复成员和非法角色。
func (h *Handler) AddMember(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	var req addMemberRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" {
		writeWorkspaceError(w, http.StatusBadRequest, "username 不能为空")
		return
	}
	if !IsValidRole(req.Role) {
		writeWorkspaceError(w, http.StatusBadRequest, "非法角色")
		return
	}
	// 不允许通过添加成员直接赋予 owner 角色——owner 只能通过创建 workspace 或所有权转移产生
	if req.Role == RoleOwner {
		writeWorkspaceError(w, http.StatusBadRequest, "不能直接添加 owner 角色")
		return
	}

	// 按用户名解析目标用户，内部只使用服务端查询得到的用户 ID。
	targetUser, err := h.repo.GetUserByUsername(r.Context(), req.Username)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "目标用户不存在")
			return
		}
		slog.Error("查询目标用户失败", "error", err, "target_username", req.Username)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	member, err := h.repo.AddMember(r.Context(), wsc.WorkspaceID, targetUser.UserID, req.Role)
	if err != nil {
		if isDuplicateKeyError(err) {
			writeWorkspaceError(w, http.StatusConflict, "用户已是该 workspace 成员")
			return
		}
		slog.Error("添加成员失败", "error", err, "workspace_id", wsc.WorkspaceID, "target_user_id", targetUser.UserID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordAudit(r, id.UserID, wsc.WorkspaceID, "workspace.member.add", "workspace_member", member.ID, map[string]string{
		"target_user_id":  targetUser.UserID,
		"target_username": targetUser.Username,
		"role":            req.Role,
	})

	writeWorkspaceJSON(w, http.StatusCreated, toMemberResponse(member))
}

// UpdateMemberRole 处理 PUT /api/workspaces/{id}/members/{userId}。
// 引入动机：design/04-WEB-API.md §Workspace 要求 member role update endpoint。
// 需要 workspace admin+ 权限（PermMemberManage）——RequireWorkspacePermission 已处理。
// 必须维护 owner 不变量：任一 active workspace 至少存在一名 owner。
// 不允许通过此操作获得 owner 角色——owner 只能通过创建 workspace 或明确的 transfer 行为产生。
func (h *Handler) UpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())
	targetUserID := r.PathValue("userId")
	if targetUserID == "" {
		writeWorkspaceError(w, http.StatusBadRequest, "缺少目标用户 ID")
		return
	}

	var req updateMemberRoleRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeWorkspaceError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if !IsValidRole(req.Role) {
		writeWorkspaceError(w, http.StatusBadRequest, "非法角色")
		return
	}
	// 不允许通过修改角色获得 owner 角色
	if req.Role == RoleOwner {
		writeWorkspaceError(w, http.StatusBadRequest, "不能通过修改角色设置 owner——所有权转移需 owner 明确执行")
		return
	}

	// 查询当前角色以检查 owner 不变量
	currentRole, err := h.repo.GetMemberRole(r.Context(), wsc.WorkspaceID, targetUserID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "成员不存在")
			return
		}
		slog.Error("查询当前成员角色失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 如果当前是 owner 且要降级，检查是否是最后一名 owner
	if currentRole == RoleOwner && req.Role != RoleOwner {
		ownerCount, err := h.repo.CountOwners(r.Context(), wsc.WorkspaceID)
		if err != nil {
			slog.Error("查询 owner 数量失败", "error", err)
			writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
			return
		}
		if ownerCount <= 1 {
			writeWorkspaceError(w, http.StatusBadRequest, "不能降级最后一名 owner——workspace 至少需要一名 owner")
			return
		}
	}

	member, err := h.repo.UpdateMemberRole(r.Context(), wsc.WorkspaceID, targetUserID, req.Role)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "成员不存在")
			return
		}
		slog.Error("更新成员角色失败", "error", err, "workspace_id", wsc.WorkspaceID, "target_user_id", targetUserID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordAudit(r, id.UserID, wsc.WorkspaceID, "workspace.member.role_update", "workspace_member", member.ID, map[string]string{
		"target_user_id": targetUserID,
		"old_role":       currentRole,
		"new_role":       req.Role,
	})

	writeWorkspaceJSON(w, http.StatusOK, toMemberResponse(member))
}

// RemoveMember 处理 DELETE /api/workspaces/{id}/members/{userId}。
// 引入动机：design/04-WEB-API.md §Workspace 要求 member removal endpoint。
// 需要 workspace admin+ 权限（PermMemberManage）——RequireWorkspacePermission 已处理。
// 必须维护 owner 不变量。
func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	wsc := WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeWorkspaceError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())
	targetUserID := r.PathValue("userId")
	if targetUserID == "" {
		writeWorkspaceError(w, http.StatusBadRequest, "缺少目标用户 ID")
		return
	}

	// 查询当前角色以检查 owner 不变量
	currentRole, err := h.repo.GetMemberRole(r.Context(), wsc.WorkspaceID, targetUserID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "成员不存在")
			return
		}
		slog.Error("查询当前成员角色失败", "error", err)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 如果要移除的是 owner，检查是否是最后一名 owner
	if currentRole == RoleOwner {
		ownerCount, err := h.repo.CountOwners(r.Context(), wsc.WorkspaceID)
		if err != nil {
			slog.Error("查询 owner 数量失败", "error", err)
			writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
			return
		}
		if ownerCount <= 1 {
			writeWorkspaceError(w, http.StatusBadRequest, "不能移除最后一名 owner——workspace 至少需要一名 owner")
			return
		}
	}

	if err := h.repo.RemoveMember(r.Context(), wsc.WorkspaceID, targetUserID); err != nil {
		if err == sql.ErrNoRows {
			writeWorkspaceError(w, http.StatusNotFound, "成员不存在")
			return
		}
		slog.Error("移除成员失败", "error", err, "workspace_id", wsc.WorkspaceID, "target_user_id", targetUserID)
		writeWorkspaceError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordAudit(r, id.UserID, wsc.WorkspaceID, "workspace.member.remove", "workspace_member", "", map[string]string{
		"target_user_id": targetUserID,
		"old_role":       currentRole,
	})

	writeWorkspaceJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- 辅助函数 ---

// recordAudit 记录审计日志。
// 引入动机：每个安全敏感/状态变更操作需要产生审计记录。
// 如果审计日志写入失败，仅记录 slog 但不影响主操作结果（审计失败不应阻塞业务）。
//
// request ID 优先从 request context 获取（由全局 RequestIDMiddleware 注入），
// 回退到 X-Request-ID header（覆盖未被全局 middleware 包装的调用路径），
// 确保审计记录始终有 correlation ID。
func (h *Handler) recordAudit(r *http.Request, userID, workspaceID, action, resourceType, resourceID string, detail interface{}) {
	if h.auditRepo == nil {
		return
	}

	var detailJSON json.RawMessage
	if detail != nil {
		data, err := json.Marshal(detail)
		if err != nil {
			slog.Error("序列化审计 detail 失败", "error", err, "action", action)
			return
		}
		detailJSON = data
	}

	requestID := httpmw.RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = r.Header.Get("X-Request-ID")
	}
	if err := h.auditRepo.Record(r.Context(), userID, workspaceID, action, resourceType, resourceID, detailJSON, requestID); err != nil {
		slog.Error("写入审计日志失败", "error", err, "action", action)
	}
}

// isDuplicateKeyError 判断是否为唯一约束冲突错误。
// 引入动机：创建 workspace、添加成员时需要区分重复键和其他错误。
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	// 检查 pgconn 错误
	var pgErr interface{ Code() string }
	if errors.As(err, &pgErr) {
		return pgErr.Code() == "23505"
	}
	// 字符串匹配作为后备（也覆盖 mock 测试错误）
	errStr := err.Error()
	return strings.Contains(errStr, "duplicate key") || strings.Contains(errStr, "already exists") || strings.Contains(errStr, "already member")
}
