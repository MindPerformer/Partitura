// handler.go 实现 document 模块的 HTTP handler。
//
// 引入动机：design/04-WEB-API.md §Document 要求 list/outline/read/section/lines/
// create/patch/replace/move/archive/history/revision/sources 等 API。
// handler 只负责 HTTP 输入输出（解析请求、写入响应），
// 领域规则（path validation、Markdown 解析、content hash、revision）、
// 数据访问分别由 path.go、outline.go、hash.go、repository.go 处理。
//
// 权限链：
//   - AuthMiddleware → RequireAuth → CSRF（状态变更）→ RequireWorkspacePermission → handler
//   - viewer: read/search/history/outline/section/lines/revision/sources(list)
//   - editor: viewer + create/update/move/replace/patch/sources(add)
//   - admin: editor + archive/restore/sources(delete)
//   - owner: admin + purge
package document

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"partitura/server/internal/audit"
	"partitura/server/internal/auth"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/queryutil"
	"partitura/server/internal/workspace"
)

// TxJobEnqueuer 定义在 PG 事务内 enqueue job 的接口。
// 引入动机：C1 要求 document/revision 与 index job 在同一 PG 事务提交或回滚。
// 使用接口而非直接依赖 job 包，避免循环导入。
// job.PGRepository 已实现 EnqueueInTx 方法，满足此接口。
type TxJobEnqueuer interface {
	// EnqueueInTx 在给定事务中 enqueue 任务。
	// 引入动机：保证文档写入和 job 入队的原子性——事务提交后 job 可见，
	// 事务回滚则 job 也不存在。
	EnqueueInTx(ctx context.Context, tx *sql.Tx, jobType string, payload map[string]interface{}) (string, error)
}

// Handler 是 document 模块的 HTTP handler 集合。
// 引入动机：将 document 相关的 HTTP handler 集中在一个结构体中，
// 通过依赖注入接收 repository 和 audit repository，保持 handler 轻量。
type Handler struct {
	repo      Repository
	auditRepo audit.Repository
	jobEnq    TxJobEnqueuer
}

// NewHandler 创建 document handler。
// 引入动机：main.go 通过此构造函数注入 repository 和 audit repository。
// jobEnq 可为 nil（当 job worker 未启用时），此时文档写入不会触发索引 job。
func NewHandler(repo Repository, auditRepo audit.Repository, jobEnq TxJobEnqueuer) *Handler {
	return &Handler{repo: repo, auditRepo: auditRepo, jobEnq: jobEnq}
}

// enqueueIndexJobInTx 构建一个事务内 enqueue 回调。
// 引入动机：C1 要求 document 写操作在同一 PG 事务中 enqueue index job。
// 返回的回调在 Repository 方法的事务内、提交前被调用。
// 如果 jobEnq 为 nil，返回 nil 表示不 enqueue。
func (h *Handler) enqueueIndexJobInTx() TxJobEnqueuer {
	return h.jobEnq
}

// enqueueIndexJobPayload 构建 index_document job 的 payload。
func enqueueIndexJobPayload(documentID string) map[string]interface{} {
	return map[string]interface{}{
		"document_id": documentID,
	}
}

// --- 请求/响应类型定义 ---

// documentResponse 是返回给客户端的文档 JSON 结构。
// 引入动机：统一 document API 响应格式，不暴露内部实现细节。
type documentResponse struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	Path            string `json:"path"`
	Title           string `json:"title"`
	Type            string `json:"type,omitempty"`
	Status          string `json:"status"`
	ContentMarkdown string `json:"content_markdown"`
	ContentHash     string `json:"content_hash"`
	RevisionNumber  int    `json:"revision_number"`
	IsSpecial       bool   `json:"is_special"`
	CreatedBy       string `json:"created_by"`
	UpdatedBy       string `json:"updated_by"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

// documentListItemResponse 是文档列表项的 JSON 结构（不含正文）。
// 引入动机：list API 不返回全文，减少响应体积。
type documentListItemResponse struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	Title          string `json:"title"`
	Type           string `json:"type,omitempty"`
	Status         string `json:"status"`
	ContentHash    string `json:"content_hash"`
	RevisionNumber int    `json:"revision_number"`
	IsSpecial      bool   `json:"is_special"`
	UpdatedBy      string `json:"updated_by"`
	UpdatedAt      string `json:"updated_at"`
}

// listDocumentsResponse 是文档列表响应。
type listDocumentsResponse struct {
	Documents []documentListItemResponse `json:"documents"`
	Total     int                        `json:"total"`
	Limit     int                        `json:"limit"`
	Offset    int                        `json:"offset"`
}

// outlineResponse 是 outline API 的响应。
type outlineResponse struct {
	Path    string    `json:"path"`
	Outline []Heading `json:"outline"`
}

// sectionResponse 是 section read API 的响应。
type sectionResponse struct {
	Path        string   `json:"path"`
	SectionPath []string `json:"section_path"`
	Content     string   `json:"content"`
	StartLine   int      `json:"start_line"`
	EndLine     int      `json:"end_line"`
}

// linesResponse 是 lines read API 的响应。
type linesResponse struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

// createDocumentRequest 是创建文档的请求体。
type createDocumentRequest struct {
	Path            string `json:"path"`
	Title           string `json:"title"`
	Type            string `json:"type"`
	ContentMarkdown string `json:"content_markdown"`
}

// replaceDocumentRequest 是替换文档全文的请求体。
type replaceDocumentRequest struct {
	Title           string `json:"title"`
	Type            string `json:"type"`
	ContentMarkdown string `json:"content_markdown"`
	ExpectedRevision int   `json:"expected_revision"`
	ExpectedHash    string `json:"expected_hash"`
}

// patchDocumentRequest 是 patch 文档的请求体。
// 引入动机：design/02-MCP.md §document_patch 要求 MCP 在本地生成 candidate content 和 hash，
// 然后上传完整 candidate 给 server。server 不再接收 old_text/new_text，
// 只做乐观并发校验（expected_revision/expected_hash）和 candidate_hash 完整性校验。
type patchDocumentRequest struct {
	// ContentMarkdown 是 MCP 本地 patch 后的完整候选内容。
	ContentMarkdown string `json:"content_markdown"`
	// CandidateHash 是 MCP 本地计算的 candidate content SHA-256 十六进制摘要。
	// server 必须验证 CandidateHash == SHA256(ContentMarkdown)，不一致返回 400。
	CandidateHash   string `json:"candidate_hash"`
	ExpectedRevision int   `json:"expected_revision"`
	ExpectedHash    string `json:"expected_hash"`
}

// moveDocumentRequest 是移动文档的请求体。
type moveDocumentRequest struct {
	NewPath         string `json:"new_path"`
	ExpectedRevision int   `json:"expected_revision"`
	ExpectedHash    string `json:"expected_hash"`
}

// archiveDocumentRequest 是归档/恢复文档的请求体。
type archiveDocumentRequest struct {
	ExpectedRevision int    `json:"expected_revision"`
	ExpectedHash     string `json:"expected_hash"`
}

// revisionResponse 是单个版本的 JSON 结构。
type revisionResponse struct {
	ID              string `json:"id"`
	DocumentID      string `json:"document_id"`
	RevisionNumber  int    `json:"revision_number"`
	Path            string `json:"path"`
	Title           string `json:"title"`
	ContentMarkdown string `json:"content_markdown"`
	ContentHash     string `json:"content_hash"`
	Status          string `json:"status"`
	CreatedBy       string `json:"created_by"`
	CreatedAt       string `json:"created_at"`
}

// listRevisionsResponse 是版本列表响应。
type listRevisionsResponse struct {
	Revisions []revisionResponse `json:"revisions"`
	Total     int                `json:"total"`
	Limit     int                `json:"limit"`
	Offset    int                `json:"offset"`
}

// sourceResponse 是单个来源的 JSON 结构。
type sourceResponse struct {
	ID                  string `json:"id"`
	DocumentID          string `json:"document_id"`
	SourceType          string `json:"source_type"`
	Value               string `json:"value"`
	Title               string `json:"title,omitempty"`
	RetrievedAt         string `json:"retrieved_at,omitempty"`
	ContentHash         string `json:"content_hash,omitempty"`
	RefreshIntervalDays int    `json:"refresh_interval_days,omitempty"`
	SourceDocumentID    string `json:"source_document_id,omitempty"`
	CreatedBy           string `json:"created_by"`
	CreatedAt           string `json:"created_at"`
}

// listSourcesResponse 是来源列表响应。
type listSourcesResponse struct {
	Sources []sourceResponse `json:"sources"`
	Total   int              `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
}

// addSourceRequest 是添加来源的请求体。
type addSourceRequest struct {
	SourceType         string `json:"source_type"`
	Value              string `json:"value"`
	Title              string `json:"title"`
	RetrievedAt        string `json:"retrieved_at"`
	ContentHash        string `json:"content_hash"`
	RefreshIntervalDays int    `json:"refresh_interval_days"`
	SourceDocumentID   string `json:"source_document_id"`
}

// --- 辅助函数 ---

// toDocumentResponse 将 Document 结构体转换为 API 响应。
func toDocumentResponse(doc *Document) documentResponse {
	return documentResponse{
		ID:              doc.ID,
		WorkspaceID:     doc.WorkspaceID,
		Path:            doc.Path,
		Title:           doc.Title,
		Type:            doc.Type,
		Status:          doc.Status,
		ContentMarkdown: doc.ContentMarkdown,
		ContentHash:     doc.ContentHash,
		RevisionNumber:  doc.RevisionNumber,
		IsSpecial:       doc.IsSpecial,
		CreatedBy:       doc.CreatedBy,
		UpdatedBy:       doc.UpdatedBy,
		CreatedAt:       doc.CreatedAt,
		UpdatedAt:       doc.UpdatedAt,
	}
}

// toDocumentListItemResponse 将 Document 转换为列表项响应（不含正文）。
func toDocumentListItemResponse(doc *Document) documentListItemResponse {
	return documentListItemResponse{
		ID:             doc.ID,
		Path:           doc.Path,
		Title:          doc.Title,
		Type:           doc.Type,
		Status:         doc.Status,
		ContentHash:    doc.ContentHash,
		RevisionNumber: doc.RevisionNumber,
		IsSpecial:      doc.IsSpecial,
		UpdatedBy:      doc.UpdatedBy,
		UpdatedAt:      doc.UpdatedAt,
	}
}

// toRevisionResponse 将 Revision 转换为 API 响应。
func toRevisionResponse(rev *Revision) revisionResponse {
	return revisionResponse{
		ID:              rev.ID,
		DocumentID:      rev.DocumentID,
		RevisionNumber:  rev.RevisionNumber,
		Path:            rev.Path,
		Title:           rev.Title,
		ContentMarkdown: rev.ContentMarkdown,
		ContentHash:     rev.ContentHash,
		Status:          rev.Status,
		CreatedBy:       rev.CreatedBy,
		CreatedAt:       rev.CreatedAt,
	}
}

// toSourceResponse 将 Source 转换为 API 响应。
func toSourceResponse(src *Source) sourceResponse {
	return sourceResponse{
		ID:                  src.ID,
		DocumentID:          src.DocumentID,
		SourceType:          src.SourceType,
		Value:               src.Value,
		Title:               src.Title,
		RetrievedAt:         src.RetrievedAt,
		ContentHash:         src.ContentHash,
		RefreshIntervalDays: src.RefreshIntervalDays,
		SourceDocumentID:    src.SourceDocumentID,
		CreatedBy:           src.CreatedBy,
		CreatedAt:           src.CreatedAt,
	}
}

// writeDocError 写入统一 JSON 错误响应。
func writeDocError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		slog.Error("写入错误响应失败", "error", err)
	}
}

// writeDocJSON 写入 JSON 响应。
func writeDocJSON(w http.ResponseWriter, status int, body interface{}) {
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

// decodeDocJSONStrict 严格解码 JSON 请求体。
// 引入动机：拒绝畸形 JSON、未知字段、多 JSON 值。
func decodeDocJSONStrict(r *http.Request, dst interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体包含多个 JSON 值")
	}
	return nil
}

// parseDocPagination 从 URL 查询参数解析分页参数。
// 引入动机：与 workspace 模块一致的分页逻辑。
//
// 委托至 queryutil.ParsePagination，采用 Fail Fast 策略：
// 畸形 RawQuery（含 '?'）直接返回错误并记录日志，不做静默清洁。
func parseDocPagination(r *http.Request) (int, int, error) {
	return queryutil.ParsePagination(r)
}

// parseIntDocStrict 严格解析整数字符串。
// 引入动机：document handler 中 ReadLines 和 GetRevision 使用此函数解析
// start/end/revision 等非分页整型参数，不经过分页路径，仍需保留。
func parseIntDocStrict(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空字符串")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("包含非数字字符: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// recordDocAudit 记录审计日志。
// 引入动机：每个文档状态变更和永久删除需要审计记录。
// detail 不能含 Markdown 全文、password、cookie、csrf/access/refresh token。
func (h *Handler) recordDocAudit(r *http.Request, userID, workspaceID, action, resourceType, resourceID string, detail interface{}) {
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
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr interface{ Code() string }
	if errors.As(err, &pgErr) {
		return pgErr.Code() == "23505"
	}
	errStr := err.Error()
	return strings.Contains(errStr, "duplicate key") || strings.Contains(errStr, "already exists")
}

// --- Handler 方法 ---

// ListDocuments 处理 GET /api/workspaces/{wid}/documents。
// 引入动机：design/04-WEB-API.md §Document 要求 list endpoint。
// 默认隐藏 archived，可通过 include_archived=true 包含。
// 可按 status/type 过滤，分页。
func (h *Handler) ListDocuments(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	limit, offset, err := parseDocPagination(r)
	if err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	q := r.URL.Query()
	statusFilter := q.Get("status")
	typeFilter := q.Get("type")
	includeArchived := q.Get("include_archived") == "true"

	if statusFilter != "" && !isValidDocumentStatus(statusFilter) {
		writeDocError(w, http.StatusBadRequest, "非法 status 过滤值")
		return
	}
	if typeFilter != "" && !isValidDocumentType(typeFilter) {
		writeDocError(w, http.StatusBadRequest, "非法 type 过滤值")
		return
	}

	result, err := h.repo.ListDocuments(r.Context(), wsc.WorkspaceID, statusFilter, typeFilter, includeArchived, limit, offset)
	if err != nil {
		slog.Error("查询文档列表失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := listDocumentsResponse{
		Documents: make([]documentListItemResponse, 0, len(result.Documents)),
		Total:     result.Total,
		Limit:     limit,
		Offset:    offset,
	}
	for i := range result.Documents {
		resp.Documents = append(resp.Documents, toDocumentListItemResponse(&result.Documents[i]))
	}

	writeDocJSON(w, http.StatusOK, resp)
}

// GetOutline 处理 GET /api/workspaces/{wid}/documents/outline。
// 引入动机：design/04-WEB-API.md §Document 要求 outline endpoint。
// 解析 Markdown ATX headings 为层级树，返回 heading、level、line numbers、section_path。
func (h *Handler) GetOutline(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// archived 文档默认拒绝读取
	if doc.Status == StatusArchived {
		writeDocError(w, http.StatusNotFound, "文档不存在")
		return
	}

	outline := ParseOutline(doc.ContentMarkdown)
	writeDocJSON(w, http.StatusOK, outlineResponse{
		Path:    doc.Path,
		Outline: outline,
	})
}

// ReadDocument 处理 GET /api/workspaces/{wid}/documents。
// 引入动机：design/04-WEB-API.md §Document 要求 read endpoint。
// 通过 path 查询参数读取全文（含 revision, hash）。
func (h *Handler) ReadDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// archived 文档默认拒绝读取
	if doc.Status == StatusArchived {
		writeDocError(w, http.StatusNotFound, "文档不存在")
		return
	}

	writeDocJSON(w, http.StatusOK, toDocumentResponse(doc))
}

// ReadSection 处理 GET /api/workspaces/{wid}/documents/section。
// 引入动机：design/04-WEB-API.md §Document 要求 read section endpoint。
// 通过 path + section_path 参数读取指定 section。
func (h *Handler) ReadSection(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	sectionPathStr := r.URL.Query().Get("section_path")
	if sectionPathStr == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 section_path 参数")
		return
	}

	// 解析 section_path（逗号分隔或 JSON 数组格式）
	sectionPath := parseSectionPath(sectionPathStr)
	if len(sectionPath) == 0 {
		writeDocError(w, http.StatusBadRequest, "section_path 不能为空")
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	if doc.Status == StatusArchived {
		writeDocError(w, http.StatusNotFound, "文档不存在")
		return
	}

	content, ok := ReadSection(doc.ContentMarkdown, sectionPath)
	if !ok {
		writeDocError(w, http.StatusNotFound, "section 未找到")
		return
	}

	// 查找 heading 以获取行号
	headings := ParseOutline(doc.ContentMarkdown)
	var startLine, endLine int
	for _, h := range headings {
		if pathEqual(h.SectionPath, sectionPath) {
			startLine = h.StartLine
			endLine = h.EndLine
			break
		}
	}

	writeDocJSON(w, http.StatusOK, sectionResponse{
		Path:        doc.Path,
		SectionPath: sectionPath,
		Content:     content,
		StartLine:   startLine,
		EndLine:     endLine,
	})
}

// ReadLines 处理 GET /api/workspaces/{wid}/documents/lines。
// 引入动机：design/04-WEB-API.md §Document 要求 read lines endpoint。
// 通过 path + start + end 参数读取指定行范围，限制最大行数 MaxLineReadCount。
func (h *Handler) ReadLines(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")
	if startStr == "" || endStr == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 start 或 end 参数")
		return
	}

	startLine, err := parseIntDocStrict(startStr)
	if err != nil {
		writeDocError(w, http.StatusBadRequest, "start 参数非法")
		return
	}
	endLine, err := parseIntDocStrict(endStr)
	if err != nil {
		writeDocError(w, http.StatusBadRequest, "end 参数非法")
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	if doc.Status == StatusArchived {
		writeDocError(w, http.StatusNotFound, "文档不存在")
		return
	}

	content, err := ReadLines(doc.ContentMarkdown, startLine, endLine, MaxLineReadCount)
	if err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	actualEnd := endLine
	lines := strings.Split(doc.ContentMarkdown, "\n")
	if actualEnd > len(lines) {
		actualEnd = len(lines)
	}

	writeDocJSON(w, http.StatusOK, linesResponse{
		Path:      doc.Path,
		StartLine: startLine,
		EndLine:   actualEnd,
		Content:   content,
	})
}

// CreateDocument 处理 POST /api/workspaces/{wid}/documents。
// 引入动机：design/04-WEB-API.md §Document 要求 create endpoint。
// 需要 editor+ 权限。创建不需要 expected_revision/expected_hash。
func (h *Handler) CreateDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	var req createDocumentRequest
	if err := decodeDocJSONStrict(r, &req); err != nil {
		writeDocError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.Path == "" {
		writeDocError(w, http.StatusBadRequest, "path 不能为空")
		return
	}
	if err := ValidatePath(req.Path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Title == "" {
		writeDocError(w, http.StatusBadRequest, "title 不能为空")
		return
	}
	if req.Type != "" && !isValidDocumentType(req.Type) {
		writeDocError(w, http.StatusBadRequest, "非法 type")
		return
	}
	if req.ContentMarkdown == "" {
		writeDocError(w, http.StatusBadRequest, "content_markdown 不能为空")
		return
	}

	// 检查 content 大小限制
	maxSize, err := h.repo.GetWorkspaceMaxDocumentSize(r.Context(), wsc.WorkspaceID)
	if err != nil {
		slog.Error("查询 workspace 文档大小限制失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if len([]byte(req.ContentMarkdown)) > maxSize {
		writeDocError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("文档内容超过最大限制 %d 字节", maxSize))
		return
	}

	// 不允许通过 API 创建特殊文件（特殊文件在 workspace 初始化时自动创建）
	if IsSpecialFilePath(req.Path) {
		writeDocError(w, http.StatusBadRequest, "不能通过 API 创建特殊文件")
		return
	}

	contentHash := ComputeContentHash(req.ContentMarkdown)

	doc, err := h.repo.CreateDocument(r.Context(), wsc.WorkspaceID, req.Path, req.Title, req.Type, req.ContentMarkdown, contentHash, false, id.UserID, h.enqueueIndexJobInTx())
	if err != nil {
		if isDuplicateKeyError(err) {
			writeDocError(w, http.StatusConflict, "路径已存在")
			return
		}
		slog.Error("创建文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", req.Path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.create", "document", doc.ID, map[string]string{
		"path":  req.Path,
		"title": req.Title,
	})

	writeDocJSON(w, http.StatusCreated, toDocumentResponse(doc))
}

// ReplaceDocument 处理 PUT /api/workspaces/{wid}/documents。
// 引入动机：design/04-WEB-API.md §Document 要求 replace endpoint。
// 需要 editor+ 权限。必须携带 expected_revision + expected_hash，不匹配返回 409。
func (h *Handler) ReplaceDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req replaceDocumentRequest
	if err := decodeDocJSONStrict(r, &req); err != nil {
		writeDocError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.ContentMarkdown == "" {
		writeDocError(w, http.StatusBadRequest, "content_markdown 不能为空")
		return
	}
	if req.Title == "" {
		writeDocError(w, http.StatusBadRequest, "title 不能为空")
		return
	}
	if req.Type != "" && !isValidDocumentType(req.Type) {
		writeDocError(w, http.StatusBadRequest, "非法 type")
		return
	}

	// 检查 content 大小限制
	maxSize, err := h.repo.GetWorkspaceMaxDocumentSize(r.Context(), wsc.WorkspaceID)
	if err != nil {
		slog.Error("查询 workspace 文档大小限制失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if len([]byte(req.ContentMarkdown)) > maxSize {
		writeDocError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("文档内容超过最大限制 %d 字节", maxSize))
		return
	}

	newHash := ComputeContentHash(req.ContentMarkdown)

	// 原子性替换：在单一事务中同时更新 content、hash、title、type，
	// 并写入恰好一条完整 revision snapshot。
	// 引入动机：之前依次调用 UpdateDocumentContent 和 UpdateDocumentMetadata，
	// 导致两条 revision、两事务，中间可被并发修改产生半成品。
	doc, err := h.repo.ReplaceDocumentFull(r.Context(), wsc.WorkspaceID, path, req.ContentMarkdown, newHash, req.Title, req.Type, req.ExpectedRevision, req.ExpectedHash, id.UserID, h.enqueueIndexJobInTx())
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			writeDocError(w, http.StatusConflict, "版本冲突：expected_revision/expected_hash 不匹配")
			return
		}
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("替换文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.replace", "document", doc.ID, map[string]string{
		"path":            path,
		"revision_number": fmt.Sprintf("%d", doc.RevisionNumber),
	})

	writeDocJSON(w, http.StatusOK, toDocumentResponse(doc))
}

// PatchDocument 处理 PATCH /api/workspaces/{wid}/documents。
// 引入动机：design/02-MCP.md §document_patch 和 design/04-WEB-API.md §Document 要求 patch endpoint。
//
// server 端职责（严格按 design）：
//   - 验证 expected_revision/expected_hash 与当前文档一致（乐观并发控制）
//   - 验证 candidate_hash == SHA256(content_markdown)（完整性校验）
//   - 一致则接受 candidate content 作为新版本
//   - 不一致返回 409
//
// MCP 端职责（在本地完成）：
//   - 获取 base revision/content
//   - 验证 old_text 唯一匹配
//   - 本地应用 old_text→new_text 生成 candidate
//   - 计算 candidate SHA-256 hash
//   - 上传 candidate content + hash + expected_revision/expected_hash
//
// 禁止 silent overwrite。
func (h *Handler) PatchDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req patchDocumentRequest
	if err := decodeDocJSONStrict(r, &req); err != nil {
		writeDocError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.ContentMarkdown == "" {
		writeDocError(w, http.StatusBadRequest, "content_markdown 不能为空")
		return
	}
	if req.CandidateHash == "" {
		writeDocError(w, http.StatusBadRequest, "candidate_hash 不能为空")
		return
	}

	// 完整性校验：candidate_hash 必须等于 content_markdown 的 SHA-256
	actualHash := ComputeContentHash(req.ContentMarkdown)
	if actualHash != req.CandidateHash {
		writeDocError(w, http.StatusBadRequest, "candidate_hash 与 content_markdown 不一致")
		return
	}

	// 检查 content 大小限制
	maxSize, err := h.repo.GetWorkspaceMaxDocumentSize(r.Context(), wsc.WorkspaceID)
	if err != nil {
		slog.Error("查询 workspace 文档大小限制失败", "error", err, "workspace_id", wsc.WorkspaceID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if len([]byte(req.ContentMarkdown)) > maxSize {
		writeDocError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("修改后文档内容超过最大限制 %d 字节", maxSize))
		return
	}

	// 原子性更新：使用 candidate content 和 candidate hash，
	// 通过 expected_revision/expected_hash 做乐观并发控制。
	updatedDoc, err := h.repo.UpdateDocumentContent(r.Context(), wsc.WorkspaceID, path, req.ContentMarkdown, req.CandidateHash, req.ExpectedRevision, req.ExpectedHash, id.UserID, h.enqueueIndexJobInTx())
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			writeDocError(w, http.StatusConflict, "版本冲突：expected_revision/expected_hash 不匹配")
			return
		}
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("patch 文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.patch", "document", updatedDoc.ID, map[string]string{
		"path":            path,
		"revision_number": fmt.Sprintf("%d", updatedDoc.RevisionNumber),
	})

	writeDocJSON(w, http.StatusOK, toDocumentResponse(updatedDoc))
}

// MoveDocument 处理 POST /api/workspaces/{wid}/documents/move。
// 引入动机：design/04-WEB-API.md §Document 要求 move endpoint。
// 需要 editor+ 权限。必须携带 expected_revision + expected_hash。
// 通过 query 参数 path 获取源路径，body 中传 new_path。
func (h *Handler) MoveDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	sourcePath := r.URL.Query().Get("path")
	if sourcePath == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(sourcePath); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req moveDocumentRequest
	if err := decodeDocJSONStrict(r, &req); err != nil {
		writeDocError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.NewPath == "" {
		writeDocError(w, http.StatusBadRequest, "new_path 不能为空")
		return
	}
	if err := ValidatePath(req.NewPath); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}
	if sourcePath == req.NewPath {
		writeDocError(w, http.StatusBadRequest, "源路径和目标路径相同")
		return
	}

	// 不允许通过 move 创建特殊文件路径（除非源本身就是特殊文件）
	if IsSpecialFilePath(req.NewPath) {
		writeDocError(w, http.StatusBadRequest, "不能移动到特殊文件路径")
		return
	}

	// 不允许移动特殊文件（PROJECT.md / AGENTS.md）到其他路径。
	// 引入动机：design/00-MASTER.md §特殊文件 要求每个 workspace 固定存在
	// PROJECT.md 和 AGENTS.md，移动它们会破坏这一不变量。
	// 必须在真正移动前拒绝，使用 HTTP 400。
	if IsSpecialFilePath(sourcePath) {
		writeDocError(w, http.StatusBadRequest, "不能移动特殊文件")
		return
	}

	// 查询源文档以验证 archived 状态
	existing, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, sourcePath)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询源文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", sourcePath)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if existing.Status == StatusArchived {
		writeDocError(w, http.StatusNotFound, "文档不存在")
		return
	}

	// 检查目标路径是否已存在
	_, err = h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, req.NewPath)
	if err == nil {
		writeDocError(w, http.StatusConflict, "目标路径已存在")
		return
	}
	if err != sql.ErrNoRows {
		slog.Error("查询目标路径失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", req.NewPath)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	doc, err := h.repo.MoveDocument(r.Context(), wsc.WorkspaceID, sourcePath, req.NewPath, req.ExpectedRevision, req.ExpectedHash, id.UserID, h.enqueueIndexJobInTx())
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			writeDocError(w, http.StatusConflict, "版本冲突：expected_revision/expected_hash 不匹配")
			return
		}
		if isDuplicateKeyError(err) {
			writeDocError(w, http.StatusConflict, "目标路径已存在")
			return
		}
		slog.Error("移动文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", sourcePath)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.move", "document", doc.ID, map[string]string{
		"old_path": sourcePath,
		"new_path": req.NewPath,
	})

	writeDocJSON(w, http.StatusOK, toDocumentResponse(doc))
}

// ArchiveDocument 处理 POST /api/workspaces/{wid}/documents/archive。
// 引入动机：design/03-DOCUMENTS.md §Archive 要求默认删除操作是 archive。
// 需要 admin+ 权限。必须携带 expected_revision + expected_hash。
func (h *Handler) ArchiveDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req archiveDocumentRequest
	if err := decodeDocJSONStrict(r, &req); err != nil {
		writeDocError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	doc, err := h.repo.ArchiveDocument(r.Context(), wsc.WorkspaceID, path, req.ExpectedRevision, req.ExpectedHash, id.UserID, h.enqueueIndexJobInTx())
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			writeDocError(w, http.StatusConflict, "版本冲突：expected_revision/expected_hash 不匹配")
			return
		}
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("归档文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.archive", "document", doc.ID, map[string]string{
		"path":            path,
		"revision_number": fmt.Sprintf("%d", doc.RevisionNumber),
	})

	writeDocJSON(w, http.StatusOK, toDocumentResponse(doc))
}

// RestoreDocument 处理 POST /api/workspaces/{wid}/documents/restore。
// 引入动机：design/03-DOCUMENTS.md §Archive 要求 archived document 可恢复。
// 需要 admin+ 权限。必须携带 expected_revision + expected_hash。
func (h *Handler) RestoreDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req archiveDocumentRequest
	if err := decodeDocJSONStrict(r, &req); err != nil {
		writeDocError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	doc, err := h.repo.RestoreDocument(r.Context(), wsc.WorkspaceID, path, req.ExpectedRevision, req.ExpectedHash, id.UserID, h.enqueueIndexJobInTx())
	if err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			writeDocError(w, http.StatusConflict, "版本冲突：expected_revision/expected_hash 不匹配")
			return
		}
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("恢复文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.restore", "document", doc.ID, map[string]string{
		"path":            path,
		"revision_number": fmt.Sprintf("%d", doc.RevisionNumber),
	})

	writeDocJSON(w, http.StatusOK, toDocumentResponse(doc))
}

// PurgeDocument 处理 POST /api/workspaces/{wid}/documents/purge。
// 引入动机：design/03-DOCUMENTS.md §Archive 要求 purge 仅 owner 可执行。
// 永久删除文档及其全部 revision 和 source。
func (h *Handler) PurgeDocument(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	// 权限检查：仅 owner 可执行 purge
	if wsc.MemberRole != workspace.RoleOwner {
		writeDocError(w, http.StatusForbidden, "永久删除需要 owner 权限")
		return
	}

	docID := r.PathValue("docId")
	if docID == "" {
		// 也支持通过 query 参数 path 获取
		path := r.URL.Query().Get("path")
		if path == "" {
			writeDocError(w, http.StatusBadRequest, "缺少 docId 或 path 参数")
			return
		}
		if err := ValidatePath(path); err != nil {
			writeDocError(w, http.StatusBadRequest, err.Error())
			return
		}
		doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
		if err != nil {
			if err == sql.ErrNoRows {
				writeDocError(w, http.StatusNotFound, "文档不存在")
				return
			}
			slog.Error("查询待删除文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
			writeDocError(w, http.StatusInternalServerError, "内部错误")
			return
		}
		docID = doc.ID
	}

	// 先查询文档元数据用于审计
	doc, err := h.repo.GetDocumentByID(r.Context(), wsc.WorkspaceID, docID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询待删除文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "doc_id", docID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	if err := h.repo.PurgeDocument(r.Context(), wsc.WorkspaceID, docID); err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("永久删除文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "doc_id", docID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.purge", "document", docID, map[string]string{
		"path":  doc.Path,
		"title": doc.Title,
	})

	writeDocJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListHistory 处理 GET /api/workspaces/{wid}/documents/history。
// 引入动机：design/04-WEB-API.md §Document 要求 history endpoint。
// 返回指定文档的版本列表，分页。
func (h *Handler) ListHistory(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit, offset, err := parseDocPagination(r)
	if err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	result, err := h.repo.ListRevisions(r.Context(), wsc.WorkspaceID, doc.ID, limit, offset)
	if err != nil {
		slog.Error("查询版本历史失败", "error", err, "workspace_id", wsc.WorkspaceID, "doc_id", doc.ID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := listRevisionsResponse{
		Revisions: make([]revisionResponse, 0, len(result.Revisions)),
		Total:     result.Total,
		Limit:     limit,
		Offset:    offset,
	}
	for i := range result.Revisions {
		resp.Revisions = append(resp.Revisions, toRevisionResponse(&result.Revisions[i]))
	}

	writeDocJSON(w, http.StatusOK, resp)
}

// GetRevision 处理 GET /api/workspaces/{wid}/documents/revision。
// 引入动机：design/04-WEB-API.md §Document 要求 revision endpoint。
// 返回指定文档的指定版本内容。
func (h *Handler) GetRevision(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	revisionStr := r.URL.Query().Get("revision")
	if revisionStr == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 revision 参数")
		return
	}
	revisionNum, err := parseIntDocStrict(revisionStr)
	if err != nil || revisionNum < 1 {
		writeDocError(w, http.StatusBadRequest, "revision 参数非法")
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	rev, err := h.repo.GetRevision(r.Context(), wsc.WorkspaceID, doc.ID, revisionNum)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "版本不存在")
			return
		}
		slog.Error("查询版本失败", "error", err, "workspace_id", wsc.WorkspaceID, "doc_id", doc.ID, "revision", revisionNum)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	writeDocJSON(w, http.StatusOK, toRevisionResponse(rev))
}

// ListSources 处理 GET /api/workspaces/{wid}/documents/sources。
// 引入动机：design/04-WEB-API.md §Document 要求 sources endpoint。
// 返回指定文档的来源列表，分页。
func (h *Handler) ListSources(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit, offset, err := parseDocPagination(r)
	if err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	if doc.Status == StatusArchived {
		writeDocError(w, http.StatusNotFound, "文档不存在")
		return
	}

	result, err := h.repo.ListSources(r.Context(), wsc.WorkspaceID, doc.ID, limit, offset)
	if err != nil {
		slog.Error("查询来源列表失败", "error", err, "workspace_id", wsc.WorkspaceID, "doc_id", doc.ID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := listSourcesResponse{
		Sources: make([]sourceResponse, 0, len(result.Sources)),
		Total:   result.Total,
		Limit:   limit,
		Offset:  offset,
	}
	for i := range result.Sources {
		resp.Sources = append(resp.Sources, toSourceResponse(&result.Sources[i]))
	}

	writeDocJSON(w, http.StatusOK, resp)
}

// AddSource 处理 POST /api/workspaces/{wid}/documents/sources。
// 引入动机：design/03-DOCUMENTS.md §Source 要求 Document-level provenance。
// 需要 editor+ 权限。source 必须是 workspace 内 document（如果指定 source_document_id）。
func (h *Handler) AddSource(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	path := r.URL.Query().Get("path")
	if path == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if err := ValidatePath(path); err != nil {
		writeDocError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req addSourceRequest
	if err := decodeDocJSONStrict(r, &req); err != nil {
		writeDocError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if !isValidSourceType(req.SourceType) {
		writeDocError(w, http.StatusBadRequest, "非法 source_type")
		return
	}
	if req.Value == "" {
		writeDocError(w, http.StatusBadRequest, "value 不能为空")
		return
	}

	doc, err := h.repo.GetDocumentByPath(r.Context(), wsc.WorkspaceID, path)
	if err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "文档不存在")
			return
		}
		slog.Error("查询文档失败", "error", err, "workspace_id", wsc.WorkspaceID, "path", path)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}
	if doc.Status == StatusArchived {
		writeDocError(w, http.StatusNotFound, "文档不存在")
		return
	}

	// 如果指定了 source_document_id，验证它是同一 workspace 内的 document
	if req.SourceDocumentID != "" {
		_, err := h.repo.GetDocumentByID(r.Context(), wsc.WorkspaceID, req.SourceDocumentID)
		if err != nil {
			if err == sql.ErrNoRows {
				writeDocError(w, http.StatusBadRequest, "source_document_id 不属于当前 workspace 或不存在")
				return
			}
			slog.Error("验证 source document 失败", "error", err, "source_doc_id", req.SourceDocumentID)
			writeDocError(w, http.StatusInternalServerError, "内部错误")
			return
		}
	}

	src, err := h.repo.AddSource(r.Context(), wsc.WorkspaceID, doc.ID, req.SourceType, req.Value, req.Title, req.RetrievedAt, req.ContentHash, req.RefreshIntervalDays, req.SourceDocumentID, id.UserID)
	if err != nil {
		slog.Error("添加来源失败", "error", err, "workspace_id", wsc.WorkspaceID, "doc_id", doc.ID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.source.add", "source", src.ID, map[string]string{
		"document_id": doc.ID,
		"source_type": req.SourceType,
	})

	writeDocJSON(w, http.StatusCreated, toSourceResponse(src))
}

// DeleteSource 处理 DELETE /api/workspaces/{wid}/documents/sources/{sourceId}。
// 引入动机：design/04-WEB-API.md §Document 要求 source delete endpoint。
// 需要 admin+ 权限。
func (h *Handler) DeleteSource(w http.ResponseWriter, r *http.Request) {
	wsc := workspace.WorkspaceContextFromContext(r.Context())
	if wsc == nil {
		writeDocError(w, http.StatusInternalServerError, "workspace 上下文缺失")
		return
	}

	id := auth.IdentityFromContext(r.Context())

	sourceID := r.PathValue("sourceId")
	if sourceID == "" {
		writeDocError(w, http.StatusBadRequest, "缺少 sourceId 参数")
		return
	}

	if err := h.repo.DeleteSource(r.Context(), wsc.WorkspaceID, sourceID); err != nil {
		if err == sql.ErrNoRows {
			writeDocError(w, http.StatusNotFound, "来源不存在")
			return
		}
		slog.Error("删除来源失败", "error", err, "workspace_id", wsc.WorkspaceID, "source_id", sourceID)
		writeDocError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	h.recordDocAudit(r, id.UserID, wsc.WorkspaceID, "document.source.delete", "source", sourceID, nil)

	writeDocJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// parseSectionPath 解析 section_path 参数。
// 引入动机：section_path 可能以逗号分隔或 JSON 数组格式传入。
// 支持两种格式：逗号分隔（"Architecture,Components"）或 JSON 数组（'["Architecture","Components"]'）。
func parseSectionPath(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	// 尝试 JSON 数组格式
	if strings.HasPrefix(s, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			return arr
		}
	}

	// 逗号分隔格式
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
