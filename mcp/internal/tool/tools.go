// Package tool 实现所有 MCP 工具的注册和执行逻辑。
//
// 引入动机：design/02-MCP.md §Document Tools 和 §Workspace 要求完整实现所有 MCP 工具。
// 每个工具通过 HTTPS REST 调用 server API，严格 schema/输入验证，
// 权限错误传播和结构化结果。
//
// 工具清单：
//   - workspace_list, workspace_current, switch_workspace, workspace_bootstrap
//   - document_list, document_outline, document_read
//   - document_create, document_patch, document_replace, upload_document_file,
//     document_move, document_archive, document_history, document_revision
//   - knowledge_search, source_attach
//
// 安全原则：
//   - 所有 project tools 在未 switch_workspace 时拒绝
//   - workspace ID 由内部注入，调用方不可控制
//   - 写请求必须携带 expected_revision + expected_hash
//   - document_patch 本地算法严格：唯一匹配、409 一次 rebase、否则返回 conflict
package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"partitura/mcp/internal/cache"
	"partitura/mcp/internal/client"
	"partitura/mcp/internal/protocol"
	"partitura/mcp/internal/workspace"
)

// Registry 是所有 MCP 工具的注册中心。
// 引入动机：集中管理工具注册，供 MCP server 启动时调用。
type Registry struct {
	// cli 是 REST 客户端。
	cli *client.Client

	// wsState 是 workspace 状态管理器。
	wsState *workspace.State

	// cache 是进程内 TTL 缓存。
	cache *cache.Cache

	// defaultSearchMode 是默认搜索模式。
	defaultSearchMode string

	// searchResultLimit 是默认搜索结果数量上限。
	searchResultLimit int
}

// NewRegistry 创建工具注册中心。
// 引入动机：MCP server 启动时创建 registry，注入所有依赖。
func NewRegistry(cli *client.Client, wsState *workspace.State, cache *cache.Cache, defaultSearchMode string, searchResultLimit int) *Registry {
	return &Registry{
		cli:               cli,
		wsState:           wsState,
		cache:             cache,
		defaultSearchMode: defaultSearchMode,
		searchResultLimit: searchResultLimit,
	}
}

// RegisterAll 注册所有 MCP 工具到 server。
// 引入动机：MCP server 启动前调用此方法注册全部工具。
func (r *Registry) RegisterAll(server *protocol.Server) {
	// --- Workspace 工具 ---
	server.RegisterTool(r.workspaceListTool(), r.handleWorkspaceList)
	server.RegisterTool(r.workspaceCurrentTool(), r.handleWorkspaceCurrent)
	server.RegisterTool(r.switchWorkspaceTool(), r.handleSwitchWorkspace)
	server.RegisterTool(r.workspaceBootstrapTool(), r.handleWorkspaceBootstrap)

	// --- Document Read 工具 ---
	server.RegisterTool(r.documentListTool(), r.handleDocumentList)
	server.RegisterTool(r.documentOutlineTool(), r.handleDocumentOutline)
	server.RegisterTool(r.documentReadTool(), r.handleDocumentRead)
	server.RegisterTool(r.documentHistoryTool(), r.handleDocumentHistory)
	server.RegisterTool(r.documentRevisionTool(), r.handleDocumentRevision)

	// --- Document Write 工具 ---
	server.RegisterTool(r.documentCreateTool(), r.handleDocumentCreate)
	server.RegisterTool(r.documentPatchTool(), r.handleDocumentPatch)
	server.RegisterTool(r.documentReplaceTool(), r.handleDocumentReplace)
	server.RegisterTool(r.uploadDocumentFileTool(), r.handleUploadDocumentFile)
	server.RegisterTool(r.documentMoveTool(), r.handleDocumentMove)
	server.RegisterTool(r.documentArchiveTool(), r.handleDocumentArchive)

	// --- Search 工具 ---
	server.RegisterTool(r.knowledgeSearchTool(), r.handleKnowledgeSearch)

	// --- Source 工具 ---
	server.RegisterTool(r.sourceAttachTool(), r.handleSourceAttach)
}

// callToolForTest 是测试专用方法，按名称调用工具并返回结果。
// 引入动机：测试需要直接调用工具 handler 而不经过 MCP 协议层。
func (r *Registry) callToolForTest(name string, args json.RawMessage) (*protocol.ToolResult, error) {
	handlers := map[string]func(json.RawMessage) (*protocol.ToolResult, error){
		"workspace_list":       r.handleWorkspaceList,
		"workspace_current":    r.handleWorkspaceCurrent,
		"switch_workspace":     r.handleSwitchWorkspace,
		"workspace_bootstrap":  r.handleWorkspaceBootstrap,
		"document_list":        r.handleDocumentList,
		"document_outline":     r.handleDocumentOutline,
		"document_read":        r.handleDocumentRead,
		"document_history":     r.handleDocumentHistory,
		"document_revision":    r.handleDocumentRevision,
		"document_create":      r.handleDocumentCreate,
		"document_patch":       r.handleDocumentPatch,
		"document_replace":     r.handleDocumentReplace,
		"upload_document_file": r.handleUploadDocumentFile,
		"document_move":        r.handleDocumentMove,
		"document_archive":     r.handleDocumentArchive,
		"knowledge_search":     r.handleKnowledgeSearch,
		"source_attach":        r.handleSourceAttach,
	}
	handler, ok := handlers[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	return handler(args)
}

// --- 辅助函数 ---

// textResult 创建纯文本工具结果。
func textResult(text string) *protocol.ToolResult {
	return &protocol.ToolResult{
		Content: []protocol.ContentItem{
			{Type: "text", Text: text},
		},
	}
}

// errorResult 创建错误工具结果。
func errorResult(msg string) *protocol.ToolResult {
	return &protocol.ToolResult{
		Content: []protocol.ContentItem{
			{Type: "text", Text: msg},
		},
		IsError: true,
	}
}

// jsonResult 创建 JSON 文本工具结果。
func jsonResult(data interface{}) (*protocol.ToolResult, error) {
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize result: %w", err)
	}
	return textResult(string(jsonBytes)), nil
}

// stripContentMarkdown 递归移除响应中的 content_markdown 字段。
// 引入动机：服务端文档响应携带完整正文，写操作把全文回传给 Agent 会占用大量上下文，
// 而写操作的结果通常只需要 revision/hash 等元数据。
// 递归处理是为了同时覆盖 patch 的 rebase 结果这类嵌套结构。
func stripContentMarkdown(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		delete(v, "content_markdown")
		for key, item := range v {
			v[key] = stripContentMarkdown(item)
		}
		return v
	case []interface{}:
		for i, item := range v {
			v[i] = stripContentMarkdown(item)
		}
		return v
	default:
		return value
	}
}

// writeResult 构建写操作的工具结果。
// 引入动机：写操作默认不回传 content_markdown，避免把整篇文档重新灌回 Agent 上下文；
// 只有调用方显式传 verbose=true 时才返回完整响应。
func writeResult(result interface{}, verbose bool) (*protocol.ToolResult, error) {
	if !verbose {
		result = stripContentMarkdown(result)
	}
	return jsonResult(result)
}

// computeHash 计算 SHA-256 十六进制摘要。
// 引入动机：document_patch 需要本地计算 hash 用于 expected_hash。
func computeHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// parseArgs 将原始 JSON 参数解析到目标结构体。
func parseArgs(raw json.RawMessage, dst interface{}) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// cacheKey 构建 workspace-scoped 缓存键。
func cacheKey(wsID, key string) string {
	return wsID + ":" + key
}

// --- Workspace 工具 ---

func (r *Registry) workspaceListTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "workspace_list",
		Description: "List all workspaces the current user can access. switch_workspace is not required first.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func (r *Registry) handleWorkspaceList(args json.RawMessage) (*protocol.ToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result struct {
		Workspaces []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			Status      string `json:"status"`
		} `json:"workspaces"`
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}

	if err := r.cli.Get(ctx, "/api/workspaces?limit=100", &result); err != nil {
		return errorResult(fmt.Sprintf("failed to list workspaces: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) workspaceCurrentTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "workspace_current",
		Description: "Return the status of the active workspace.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func (r *Registry) handleWorkspaceCurrent(args json.RawMessage) (*protocol.ToolResult, error) {
	if !r.wsState.IsActive() {
		return textResult("No active workspace. Use switch_workspace first."), nil
	}

	info := map[string]string{
		"workspace_id":   r.wsState.ID(),
		"workspace_name": r.wsState.Name(),
	}
	return jsonResult(info)
}

func (r *Registry) switchWorkspaceTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "switch_workspace",
		Description: "Switch the active workspace. All project tools then operate on that workspace. Requires permission on the workspace.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"workspace_id": map[string]interface{}{
					"type":        "string",
					"description": "Workspace UUID to switch to",
				},
			},
			"required": []string{"workspace_id"},
		},
	}
}

func (r *Registry) handleSwitchWorkspace(args json.RawMessage) (*protocol.ToolResult, error) {
	var params struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.WorkspaceID == "" {
		return errorResult("workspace_id is required"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 验证用户有该 workspace 权限——调用 GET /api/workspaces/{id}
	// 如果用户不是成员，server 返回 404
	var ws struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		Status      string `json:"status"`
	}

	if err := r.cli.Get(ctx, "/api/workspaces/"+params.WorkspaceID, &ws); err != nil {
		if apiErr, ok := err.(*client.APIError); ok && apiErr.IsNotFound() {
			return errorResult("workspace not found or permission denied"), nil
		}
		return errorResult(fmt.Sprintf("failed to fetch workspace: %v", err)), nil
	}

	// 设置 active workspace
	r.wsState.Switch(ws.ID, ws.DisplayName)

	// 清除前一个 workspace 的缓存
	r.cache.Clear()

	// 检查 PROJECT.md 和 AGENTS.md 是否存在
	projectExists := r.checkDocumentExists(ctx, ws.ID, "PROJECT.md")
	agentsExists := r.checkDocumentExists(ctx, ws.ID, "AGENTS.md")

	result := map[string]interface{}{
		"workspace_id":   ws.ID,
		"workspace_name": ws.DisplayName,
		"project_md":     map[string]bool{"exists": projectExists},
		"agents_md":      map[string]bool{"exists": agentsExists},
	}

	return jsonResult(result)
}

// checkDocumentExists 检查文档是否存在（不返回全文）。
func (r *Registry) checkDocumentExists(ctx context.Context, wsID, path string) bool {
	url := fmt.Sprintf("/api/workspaces/%s/documents/read?path=%s", wsID, path)
	var result map[string]interface{}
	err := r.cli.Get(ctx, url, &result)
	return err == nil
}

func (r *Registry) workspaceBootstrapTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "workspace_bootstrap",
		Description: "Return an overview of the active workspace: the essential parts of PROJECT.md and AGENTS.md, key entry points, and outlines of documents to read first. Bounded in size; full files are not returned.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func (r *Registry) handleWorkspaceBootstrap(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	wsID := r.wsState.ID()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 读取 PROJECT.md（限制前 100 行以控制上下文大小）
	projectContent := ""
	projectURL := fmt.Sprintf("/api/workspaces/%s/documents/read?path=PROJECT.md", wsID)
	var projectDoc map[string]interface{}
	if err := r.cli.Get(ctx, projectURL, &projectDoc); err == nil {
		if content, ok := projectDoc["content_markdown"].(string); ok {
			lines := strings.Split(content, "\n")
			if len(lines) > 100 {
				projectContent = strings.Join(lines[:100], "\n") + "\n... (truncated; use document_read for more)"
			} else {
				projectContent = content
			}
		}
	}

	// 读取 AGENTS.md（限制前 100 行）
	agentsContent := ""
	agentsURL := fmt.Sprintf("/api/workspaces/%s/documents/read?path=AGENTS.md", wsID)
	var agentsDoc map[string]interface{}
	if err := r.cli.Get(ctx, agentsURL, &agentsDoc); err == nil {
		if content, ok := agentsDoc["content_markdown"].(string); ok {
			lines := strings.Split(content, "\n")
			if len(lines) > 100 {
				agentsContent = strings.Join(lines[:100], "\n") + "\n... (truncated; use document_read for more)"
			} else {
				agentsContent = content
			}
		}
	}

	// 获取文档列表以推荐先阅读
	listURL := fmt.Sprintf("/api/workspaces/%s/documents?limit=10", wsID)
	var listResult struct {
		Documents []struct {
			Path  string `json:"path"`
			Title string `json:"title"`
			Type  string `json:"type"`
		} `json:"documents"`
	}
	_ = r.cli.Get(ctx, listURL, &listResult)

	recommended := make([]map[string]string, 0, len(listResult.Documents))
	for _, doc := range listResult.Documents {
		if doc.Path != "PROJECT.md" && doc.Path != "AGENTS.md" {
			recommended = append(recommended, map[string]string{
				"path":  doc.Path,
				"title": doc.Title,
				"type":  doc.Type,
			})
		}
	}

	result := map[string]interface{}{
		"project_md":          projectContent,
		"agents_md":           agentsContent,
		"recommended_reading": recommended,
	}

	return jsonResult(result)
}

// --- Document Read 工具 ---

func (r *Registry) documentListTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_list",
		Description: "List documents in the active workspace. Archived documents are hidden by default.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Page size (default 20, max 100)",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "Pagination offset",
				},
				"status": map[string]interface{}{
					"type":        "string",
					"description": "Filter by status: active/draft/archived",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Filter by document type",
				},
				"include_archived": map[string]interface{}{
					"type":        "boolean",
					"description": "Include archived documents",
				},
			},
		},
	}
}

func (r *Registry) handleDocumentList(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Limit           int    `json:"limit"`
		Offset          int    `json:"offset"`
		Status          string `json:"status"`
		Type            string `json:"type"`
		IncludeArchived bool   `json:"include_archived"`
	}
	_ = parseArgs(args, &params)

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents?limit=%d&offset=%d", r.wsState.ID(), limit, params.Offset)
	if params.Status != "" {
		url += "&status=" + params.Status
	}
	if params.Type != "" {
		url += "&type=" + params.Type
	}
	if params.IncludeArchived {
		url += "&include_archived=true"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("failed to list documents: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) documentOutlineTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_outline",
		Description: "Return the Markdown heading tree of a document (heading, level, start_line, end_line, section_path).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path (e.g. architecture/overview.md)",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (r *Registry) handleDocumentOutline(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path string `json:"path"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}

	// 检查缓存
	cacheKey := cacheKey(r.wsState.ID(), "outline:"+params.Path)
	if cached, ok := r.cache.Get(cacheKey); ok {
		return jsonResult(cached)
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/outline?path=%s", r.wsState.ID(), params.Path)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("failed to fetch document outline: %v", err)), nil
	}

	r.cache.Set(cacheKey, result)
	return jsonResult(result)
}

func (r *Registry) documentReadTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_read",
		Description: "Read a document. With no extra params returns metadata+full content; pass section_path OR start_line+end_line to read only a slice. For long documents call document_outline first to get section_path/line ranges.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path",
				},
				"section_path": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "Section path array from document_outline (e.g. [\"Architecture\", \"Components\"])",
				},
				"start_line": map[string]interface{}{
					"type":        "integer",
					"description": "Start line (1-based, inclusive); must be given with end_line",
				},
				"end_line": map[string]interface{}{
					"type":        "integer",
					"description": "End line (inclusive); max 500 lines per call",
				},
			},
			"required": []string{"path"},
		},
	}
}

// handleDocumentRead 合并全文/段读/行读三种模式。
// 引入动机：三个读工具合并为一个 document_read，减少 tools/list 体积和 Agent 选工具的步骤。
// 参数互斥规则 fail-fast：section_path 与 start_line/end_line 不可同时给出；start/end 必须成对出现。
func (r *Registry) handleDocumentRead(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path        string   `json:"path"`
		SectionPath []string `json:"section_path"`
		StartLine   *int     `json:"start_line"`
		EndLine     *int     `json:"end_line"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}

	hasSection := len(params.SectionPath) > 0
	hasStart := params.StartLine != nil
	hasEnd := params.EndLine != nil

	if hasSection && (hasStart || hasEnd) {
		return errorResult("provide either section_path or start_line/end_line, not both"), nil
	}
	if hasStart != hasEnd {
		return errorResult("start_line and end_line must be provided together"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch {
	case hasSection:
		// 段读：不缓存
		// 构建 section_path 查询参数（JSON 数组格式）
		sectionPathJSON, _ := json.Marshal(params.SectionPath)
		url := fmt.Sprintf("/api/workspaces/%s/documents/section?path=%s&section_path=%s",
			r.wsState.ID(), params.Path, string(sectionPathJSON))

		var result interface{}
		if err := r.cli.Get(ctx, url, &result); err != nil {
			return errorResult(fmt.Sprintf("failed to read section: %v", err)), nil
		}
		return jsonResult(result)

	case hasStart:
		start, end := *params.StartLine, *params.EndLine
		if start < 1 {
			return errorResult("start_line must be at least 1"), nil
		}
		if end < start {
			return errorResult("end_line must not be less than start_line"), nil
		}
		if end-start+1 > 500 {
			return errorResult("requested line range exceeds the 500-line limit"), nil
		}

		// 行读：不缓存
		url := fmt.Sprintf("/api/workspaces/%s/documents/lines?path=%s&start=%d&end=%d",
			r.wsState.ID(), params.Path, start, end)

		var result interface{}
		if err := r.cli.Get(ctx, url, &result); err != nil {
			return errorResult(fmt.Sprintf("failed to read line range: %v", err)), nil
		}
		return jsonResult(result)

	default:
		// 全文读：走 read 缓存
		cacheKey := cacheKey(r.wsState.ID(), "read:"+params.Path)
		if cached, ok := r.cache.Get(cacheKey); ok {
			return jsonResult(cached)
		}

		url := fmt.Sprintf("/api/workspaces/%s/documents/read?path=%s", r.wsState.ID(), params.Path)

		var result interface{}
		if err := r.cli.Get(ctx, url, &result); err != nil {
			return errorResult(fmt.Sprintf("failed to read document: %v", err)), nil
		}

		r.cache.Set(cacheKey, result)
		return jsonResult(result)
	}
}

func (r *Registry) documentHistoryTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_history",
		Description: "Return the revision history of a document.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Page size",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "Pagination offset",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (r *Registry) handleDocumentHistory(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path   string `json:"path"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/history?path=%s&limit=%d&offset=%d",
		r.wsState.ID(), params.Path, limit, params.Offset)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("failed to fetch revision history: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) documentRevisionTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_revision",
		Description: "Read a specific revision of a document.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path",
				},
				"revision": map[string]interface{}{
					"type":        "integer",
					"description": "Revision number",
				},
			},
			"required": []string{"path", "revision"},
		},
	}
}

func (r *Registry) handleDocumentRevision(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path     string `json:"path"`
		Revision int    `json:"revision"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}
	if params.Revision < 1 {
		return errorResult("revision must be at least 1"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/revision?path=%s&revision=%d",
		r.wsState.ID(), params.Path, params.Revision)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("failed to fetch revision: %v", err)), nil
	}

	return jsonResult(result)
}

// --- Document Write 工具 ---

func (r *Registry) documentCreateTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_create",
		Description: "Create a new document. PROJECT.md and AGENTS.md cannot be created this way; they are created when the workspace is initialized.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path (e.g. architecture/overview.md)",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "Document title",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Document type (optional)",
				},
				"content_markdown": map[string]interface{}{
					"type":        "string",
					"description": "Markdown content",
				},
				"verbose": map[string]interface{}{
					"type":        "boolean",
					"description": "Return full content_markdown in response; default false returns metadata only.",
				},
			},
			"required": []string{"path", "title", "content_markdown"},
		},
	}
}

func (r *Registry) handleDocumentCreate(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path            string `json:"path"`
		Title           string `json:"title"`
		Type            string `json:"type"`
		ContentMarkdown string `json:"content_markdown"`
		Verbose         bool   `json:"verbose"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}
	if params.Title == "" {
		return errorResult("title is required"), nil
	}
	if params.ContentMarkdown == "" {
		return errorResult("content_markdown is required"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents", r.wsState.ID())
	body := map[string]interface{}{
		"path":             params.Path,
		"title":            params.Title,
		"type":             params.Type,
		"content_markdown": params.ContentMarkdown,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		return errorResult(fmt.Sprintf("failed to create document: %v", err)), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "list"))
	return writeResult(result, params.Verbose)
}

func (r *Registry) documentReplaceTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_replace",
		Description: "Replace a document's full content. expected_revision and expected_hash are required for optimistic concurrency control.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "Document title",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Document type (optional)",
				},
				"content_markdown": map[string]interface{}{
					"type":        "string",
					"description": "New full Markdown content",
				},
				"expected_revision": map[string]interface{}{
					"type":        "integer",
					"description": "Optimistic concurrency token: the document's current revision_number from your last read.",
				},
				"expected_hash": map[string]interface{}{
					"type":        "string",
					"description": "Optimistic concurrency token: the document's current content_hash from your last read.",
				},
				"verbose": map[string]interface{}{
					"type":        "boolean",
					"description": "Return full content_markdown in response; default false returns metadata only.",
				},
			},
			"required": []string{"path", "title", "content_markdown", "expected_revision", "expected_hash"},
		},
	}
}

func (r *Registry) handleDocumentReplace(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path             string `json:"path"`
		Title            string `json:"title"`
		Type             string `json:"type"`
		ContentMarkdown  string `json:"content_markdown"`
		ExpectedRevision int    `json:"expected_revision"`
		ExpectedHash     string `json:"expected_hash"`
		Verbose          bool   `json:"verbose"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}
	if params.Title == "" {
		return errorResult("title is required"), nil
	}
	if params.ContentMarkdown == "" {
		return errorResult("content_markdown is required"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents?path=%s", r.wsState.ID(), params.Path)
	body := map[string]interface{}{
		"title":             params.Title,
		"type":              params.Type,
		"content_markdown":  params.ContentMarkdown,
		"expected_revision": params.ExpectedRevision,
		"expected_hash":     params.ExpectedHash,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Put(ctx, url, body, &result); err != nil {
		if apiErr, ok := err.(*client.APIError); ok && apiErr.IsConflict() {
			return errorResult("revision conflict: expected_revision/expected_hash do not match; read the document again"), nil
		}
		return errorResult(fmt.Sprintf("failed to replace document: %v", err)), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "read:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "outline:"+params.Path))
	return writeResult(result, params.Verbose)
}

// uploadDocumentFileTool defines the MCP tool for creating a Workspace document from a local file.
// Motivation: agents need to import an existing local Markdown file without copying its full content into arguments.
func (r *Registry) uploadDocumentFileTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "upload_document_file",
		Description: "Create a document in the active workspace from an existing local Markdown file. file_path must be absolute. Set overwrite to true to replace an existing destination document with optimistic concurrency control; otherwise creation fails if the destination path already exists.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Destination path for the new Workspace document",
				},
				"file_path": map[string]interface{}{
					"type":        "string",
					"description": "Absolute local file path on the machine running MCP; use the native path format",
				},
				"overwrite": map[string]interface{}{
					"type":        "boolean",
					"description": "When true, replace the existing destination document using optimistic concurrency control; defaults to false",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "Document title; defaults to the local filename without its extension",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "Document type (optional)",
				},
				"verbose": map[string]interface{}{
					"type":        "boolean",
					"description": "Return full content_markdown in response; default false returns metadata only.",
				},
			},
			"required": []string{"path", "file_path"},
		},
	}
}

// handleUploadDocumentFile reads a local file and creates a Workspace document.
// Motivation: importing must use document_create semantics and must not turn into an unconfirmed overwrite.
func (r *Registry) handleUploadDocumentFile(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path      string `json:"path"`
		FilePath  string `json:"file_path"`
		Title     string `json:"title"`
		Type      string `json:"type"`
		Overwrite bool   `json:"overwrite"`
		Verbose   bool   `json:"verbose"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}
	if params.FilePath == "" {
		return errorResult("file_path is required"), nil
	}
	if !filepath.IsAbs(params.FilePath) {
		return errorResult("file_path must be an absolute path"), nil
	}

	fileInfo, err := os.Stat(params.FilePath)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to stat local file: %v", err)), nil
	}
	if !fileInfo.Mode().IsRegular() {
		return errorResult("file_path must refer to a regular file"), nil
	}

	contentBytes, err := os.ReadFile(params.FilePath)
	if err != nil {
		return errorResult(fmt.Sprintf("failed to read local file: %v", err)), nil
	}
	if len(contentBytes) == 0 {
		return errorResult("local file must not be empty"), nil
	}
	if !utf8.Valid(contentBytes) {
		return errorResult("local file must contain valid UTF-8 text"), nil
	}

	if params.Title == "" {
		baseName := filepath.Base(params.FilePath)
		params.Title = strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if params.Title == "" {
			return errorResult("cannot derive title from file_path; provide title explicitly"), nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var result interface{}
	if params.Overwrite {
		readURL := fmt.Sprintf("/api/workspaces/%s/documents/read%s", r.wsState.ID(),
			client.BuildQueryParams(map[string]string{"path": params.Path}))
		var existing struct {
			Title          string `json:"title"`
			Type           string `json:"type"`
			ContentHash    string `json:"content_hash"`
			RevisionNumber int    `json:"revision_number"`
		}
		if err := r.cli.Get(ctx, readURL, &existing); err != nil {
			if apiErr, ok := err.(*client.APIError); ok && apiErr.IsNotFound() {
				return errorResult("overwrite requires an existing destination document"), nil
			}
			return errorResult(fmt.Sprintf("failed to read destination document: %v", err)), nil
		}
		if existing.Title == "" || existing.ContentHash == "" || existing.RevisionNumber < 1 {
			return errorResult("destination document response is missing title, content_hash, or revision_number"), nil
		}

		replaceURL := fmt.Sprintf("/api/workspaces/%s/documents%s", r.wsState.ID(),
			client.BuildQueryParams(map[string]string{"path": params.Path}))
		replaceBody := map[string]interface{}{
			"title":             existing.Title,
			"type":              existing.Type,
			"content_markdown":  string(contentBytes),
			"expected_revision": existing.RevisionNumber,
			"expected_hash":     existing.ContentHash,
		}
		if err := r.cli.Put(ctx, replaceURL, replaceBody, &result); err != nil {
			if apiErr, ok := err.(*client.APIError); ok && apiErr.IsConflict() {
				return errorResult("destination document changed during upload; read it again and retry"), nil
			}
			return errorResult(fmt.Sprintf("failed to overwrite destination document: %v", err)), nil
		}
		r.cache.Invalidate(cacheKey(r.wsState.ID(), "read:"+params.Path))
		r.cache.Invalidate(cacheKey(r.wsState.ID(), "outline:"+params.Path))
		slog.Info("overwrote document from local file", "path", params.Path, "bytes", len(contentBytes))
		return writeResult(result, params.Verbose)
	}

	createURL := fmt.Sprintf("/api/workspaces/%s/documents", r.wsState.ID())
	createBody := map[string]interface{}{
		"path":             params.Path,
		"title":            params.Title,
		"type":             params.Type,
		"content_markdown": string(contentBytes),
	}
	if err := r.cli.Post(ctx, createURL, createBody, &result); err != nil {
		if apiErr, ok := err.(*client.APIError); ok && apiErr.StatusCode == 409 {
			return errorResult("destination document path already exists; set overwrite to true to replace it"), nil
		}
		return errorResult(fmt.Sprintf("failed to upload file and create document: %v", err)), nil
	}

	r.cache.Invalidate(cacheKey(r.wsState.ID(), "list"))
	slog.Info("created document from local file", "path", params.Path, "bytes", len(contentBytes))
	return writeResult(result, params.Verbose)
}

func (r *Registry) documentMoveTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_move",
		Description: "Move a document to a new path. expected_revision and expected_hash are required.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Source document path",
				},
				"new_path": map[string]interface{}{
					"type":        "string",
					"description": "Destination document path",
				},
				"expected_revision": map[string]interface{}{
					"type":        "integer",
					"description": "Optimistic concurrency token: the document's current revision_number from your last read.",
				},
				"expected_hash": map[string]interface{}{
					"type":        "string",
					"description": "Optimistic concurrency token: the document's current content_hash from your last read.",
				},
				"verbose": map[string]interface{}{
					"type":        "boolean",
					"description": "Return full content_markdown in response; default false returns metadata only.",
				},
			},
			"required": []string{"path", "new_path", "expected_revision", "expected_hash"},
		},
	}
}

func (r *Registry) handleDocumentMove(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path             string `json:"path"`
		NewPath          string `json:"new_path"`
		ExpectedRevision int    `json:"expected_revision"`
		ExpectedHash     string `json:"expected_hash"`
		Verbose          bool   `json:"verbose"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}
	if params.NewPath == "" {
		return errorResult("new_path is required"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/move?path=%s", r.wsState.ID(), params.Path)
	body := map[string]interface{}{
		"new_path":          params.NewPath,
		"expected_revision": params.ExpectedRevision,
		"expected_hash":     params.ExpectedHash,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		if apiErr, ok := err.(*client.APIError); ok && apiErr.IsConflict() {
			return errorResult("revision conflict, or the destination path already exists"), nil
		}
		return errorResult(fmt.Sprintf("failed to move document: %v", err)), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "read:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "outline:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "list"))
	return writeResult(result, params.Verbose)
}

func (r *Registry) documentArchiveTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_archive",
		Description: "Archive a document. Set delete to true for permanent deletion; delete requires owner permission. Archive requests require expected_revision and expected_hash.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path",
				},
				"expected_revision": map[string]interface{}{
					"type":        "integer",
					"description": "Optimistic concurrency token: the document's current revision_number from your last read; required unless delete is true.",
				},
				"expected_hash": map[string]interface{}{
					"type":        "string",
					"description": "Optimistic concurrency token: the document's current content_hash from your last read; required unless delete is true.",
				},
				"delete": map[string]interface{}{
					"type":        "boolean",
					"description": "Permanently delete instead of archiving (requires owner permission); ignores expected_revision/expected_hash.",
				},
				"verbose": map[string]interface{}{
					"type":        "boolean",
					"description": "Return full content_markdown in response; default false returns metadata only.",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (r *Registry) handleDocumentArchive(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path             string `json:"path"`
		ExpectedRevision int    `json:"expected_revision"`
		ExpectedHash     string `json:"expected_hash"`
		Delete           bool   `json:"delete"`
		Verbose          bool   `json:"verbose"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}

	wsID := r.wsState.ID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}

	// delete=true 走永久删除：只有 owner 能通过服务端权限检查，且不需要 revision/hash。
	if params.Delete {
		url := fmt.Sprintf("/api/workspaces/%s/documents/purge%s", wsID,
			client.BuildQueryParams(map[string]string{"path": params.Path}))
		if err := r.cli.Post(ctx, url, nil, &result); err != nil {
			if apiErr, ok := err.(*client.APIError); ok {
				switch {
				case apiErr.IsForbidden():
					return errorResult("permanent deletion requires owner permission"), nil
				case apiErr.IsNotFound():
					return errorResult("document not found"), nil
				}
			}
			return errorResult(fmt.Sprintf("failed to permanently delete document: %v", err)), nil
		}
		r.cache.Invalidate(cacheKey(wsID, "read:"+params.Path))
		r.cache.Invalidate(cacheKey(wsID, "outline:"+params.Path))
		r.cache.Invalidate(cacheKey(wsID, "list"))
		slog.Info("permanently deleted document", "path", params.Path)
		return writeResult(result, params.Verbose)
	}

	if params.ExpectedHash == "" || params.ExpectedRevision < 1 {
		return errorResult("expected_revision and expected_hash are required when delete is false"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/archive%s", wsID,
		client.BuildQueryParams(map[string]string{"path": params.Path}))
	body := map[string]interface{}{
		"expected_revision": params.ExpectedRevision,
		"expected_hash":     params.ExpectedHash,
	}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		if apiErr, ok := err.(*client.APIError); ok && apiErr.IsConflict() {
			return errorResult("revision conflict: expected_revision/expected_hash do not match"), nil
		}
		return errorResult(fmt.Sprintf("failed to archive document: %v", err)), nil
	}

	r.cache.Invalidate(cacheKey(wsID, "read:"+params.Path))
	r.cache.Invalidate(cacheKey(wsID, "outline:"+params.Path))
	r.cache.Invalidate(cacheKey(wsID, "list"))
	return writeResult(result, params.Verbose)
}

// --- document_patch 工具（核心：本地 patch 算法 + 409 一次 rebase）---

func (r *Registry) documentPatchTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_patch",
		Description: "Patch a document locally: verify old_text matches exactly once, build candidate content+hash, then upload. On 409, one safe rebase is attempted.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path",
				},
				"old_text": map[string]interface{}{
					"type":        "string",
					"description": "Text to replace; it must match exactly once in the document",
				},
				"new_text": map[string]interface{}{
					"type":        "string",
					"description": "Replacement text",
				},
				"verbose": map[string]interface{}{
					"type":        "boolean",
					"description": "Return full content_markdown in response; default false returns metadata only.",
				},
			},
			"required": []string{"path", "old_text", "new_text"},
		},
	}
}

func (r *Registry) handleDocumentPatch(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
		Verbose bool   `json:"verbose"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}
	if params.OldText == "" {
		return errorResult("old_text is required"), nil
	}

	wsID := r.wsState.ID()

	// 1. 从 cache 或 server 获取 base revision/content/hash
	var doc struct {
		ContentMarkdown string `json:"content_markdown"`
		ContentHash     string `json:"content_hash"`
		RevisionNumber  int    `json:"revision_number"`
	}

	cacheKey := cacheKey(wsID, "read:"+params.Path)
	if cached, ok := r.cache.Get(cacheKey); ok {
		if cachedMap, ok := cached.(map[string]interface{}); ok {
			if cm, ok := cachedMap["content_markdown"].(string); ok {
				doc.ContentMarkdown = cm
			}
			if ch, ok := cachedMap["content_hash"].(string); ok {
				doc.ContentHash = ch
			}
			if rn, ok := cachedMap["revision_number"].(float64); ok {
				doc.RevisionNumber = int(rn)
			}
		}
	}

	if doc.ContentMarkdown == "" {
		// 从 server 获取
		url := fmt.Sprintf("/api/workspaces/%s/documents/read?path=%s", wsID, params.Path)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := r.cli.Get(ctx, url, &doc); err != nil {
			return errorResult(fmt.Sprintf("failed to fetch document: %v", err)), nil
		}

		// 缓存文档
		r.cache.Set(cacheKey, map[string]interface{}{
			"content_markdown": doc.ContentMarkdown,
			"content_hash":     doc.ContentHash,
			"revision_number":  doc.RevisionNumber,
		})
	}

	// 2. 验证 old_text 在 base content 中恰好唯一匹配
	patchResult, err := r.applyPatchAndUpload(wsID, params.Path, params.OldText, params.NewText,
		doc.ContentMarkdown, doc.ContentHash, doc.RevisionNumber)
	if err != nil {
		return errorResult(err.Error()), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey)

	return writeResult(patchResult, params.Verbose)
}

// applyPatchAndUpload 执行 patch 的核心逻辑：验证唯一匹配 → 本地生成 candidate → 计算 hash → 上传。
// 引入动机：document_patch 的本地算法，严格遵循 design/02-MCP.md §document_patch。
// 不能仅把 old/new 交服务器处理——必须在本地完成 candidate 生成和 hash 计算。
func (r *Registry) applyPatchAndUpload(wsID, path, oldText, newText, baseContent, baseHash string, baseRevision int) (interface{}, error) {
	// 1. 验证 old_text 在 base content 中恰好唯一匹配
	count := strings.Count(baseContent, oldText)
	if count == 0 {
		return nil, fmt.Errorf("old_text was not found in the document (0 matches)")
	}
	if count > 1 {
		return nil, fmt.Errorf("old_text matches %d times; exactly one match is required", count)
	}

	// 2. 本地应用 old_text→new_text 生成 candidate 完整内容
	candidateContent := strings.Replace(baseContent, oldText, newText, 1)

	// 3. 计算 candidate SHA-256 hash
	candidateHash := computeHash(candidateContent)

	// 4. 上传 candidate content + hash + expected_revision/expected_hash 到 server
	url := fmt.Sprintf("/api/workspaces/%s/documents?path=%s", wsID, path)
	body := map[string]interface{}{
		"content_markdown":  candidateContent,
		"candidate_hash":    candidateHash,
		"expected_revision": baseRevision,
		"expected_hash":     baseHash,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	err := r.cli.Patch(ctx, url, body, &result)

	if err == nil {
		return result, nil
	}

	// 检查是否为 409 冲突
	apiErr, ok := err.(*client.APIError)
	if !ok || !apiErr.IsConflict() {
		return nil, fmt.Errorf("failed to upload patch: %w", err)
	}

	// 5. 409 冲突——尝试一次安全 rebase
	slog.Info("patch hit 409 conflict; attempting one safe rebase", "path", path)

	// 获取最新文档内容
	readURL := fmt.Sprintf("/api/workspaces/%s/documents/read?path=%s", wsID, path)
	var latestDoc struct {
		ContentMarkdown string `json:"content_markdown"`
		ContentHash     string `json:"content_hash"`
		RevisionNumber  int    `json:"revision_number"`
	}

	readCtx, readCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer readCancel()

	if err := r.cli.Get(readCtx, readURL, &latestDoc); err != nil {
		return nil, fmt.Errorf("rebase: failed to fetch the latest document: %w", err)
	}

	// 检查 old_text 在最新内容中是否仍唯一匹配
	latestCount := strings.Count(latestDoc.ContentMarkdown, oldText)
	if latestCount == 0 {
		// old_text 在最新内容中不存在——返回完整 conflict 信息
		return map[string]interface{}{
			"conflict":        true,
			"reason":          "old_text was not found in the latest content (it may have been modified or removed)",
			"base_revision":   baseRevision,
			"base_hash":       baseHash,
			"latest_revision": latestDoc.RevisionNumber,
			"latest_hash":     latestDoc.ContentHash,
			"suggestion":      "Read the document again, confirm old_text still exists, then retry.",
		}, nil
	}
	if latestCount > 1 {
		return map[string]interface{}{
			"conflict":        true,
			"reason":          fmt.Sprintf("old_text matches %d times in the latest content; automatic rebase is not possible", latestCount),
			"base_revision":   baseRevision,
			"base_hash":       baseHash,
			"latest_revision": latestDoc.RevisionNumber,
			"latest_hash":     latestDoc.ContentHash,
			"suggestion":      "Read the document again and use a more specific old_text that matches exactly once, then retry.",
		}, nil
	}

	// old_text 在最新内容中仍唯一匹配——本地重建 candidate 和 hash
	rebaseCandidate := strings.Replace(latestDoc.ContentMarkdown, oldText, newText, 1)
	rebaseCandidateHash := computeHash(rebaseCandidate)

	// 执行一次 rebase retry
	rebaseUrl := fmt.Sprintf("/api/workspaces/%s/documents?path=%s", wsID, path)
	rebaseBody := map[string]interface{}{
		"content_markdown":  rebaseCandidate,
		"candidate_hash":    rebaseCandidateHash,
		"expected_revision": latestDoc.RevisionNumber,
		"expected_hash":     latestDoc.ContentHash,
	}

	rebaseCtx, rebaseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rebaseCancel()

	var rebaseResult interface{}
	if err := r.cli.Patch(rebaseCtx, rebaseUrl, rebaseBody, &rebaseResult); err != nil {
		// rebase 仍失败——返回完整 conflict 信息
		return map[string]interface{}{
			"conflict":        true,
			"reason":          fmt.Sprintf("patch still failed after rebase: %v", err),
			"base_revision":   baseRevision,
			"base_hash":       baseHash,
			"latest_revision": latestDoc.RevisionNumber,
			"latest_hash":     latestDoc.ContentHash,
			"suggestion":      "Read the latest content and apply the change manually.",
		}, nil
	}

	// rebase 成功
	return map[string]interface{}{
		"rebased": true,
		"result":  rebaseResult,
	}, nil
}

// --- Search 工具 ---

func (r *Registry) knowledgeSearchTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "knowledge_search",
		Description: "Search the knowledge base of the active workspace only. Cross-workspace search is not supported.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query",
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "Search mode: hybrid (default), lexical, semantic",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of results",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "Pagination offset",
				},
				"max_snippet_chars": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum characters per result snippet; server generates ~200-char snippets, this truncates further client-side.",
				},
			},
			"required": []string{"query"},
		},
	}
}

func (r *Registry) handleKnowledgeSearch(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Query           string `json:"query"`
		Mode            string `json:"mode"`
		Limit           int    `json:"limit"`
		Offset          int    `json:"offset"`
		MaxSnippetChars int    `json:"max_snippet_chars"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Query == "" {
		return errorResult("query is required"), nil
	}

	// mode 默认值
	mode := params.Mode
	if mode == "" {
		mode = r.defaultSearchMode
	}

	// limit 默认值
	limit := params.Limit
	if limit <= 0 {
		limit = r.searchResultLimit
	}

	// workspace ID 由内部注入，调用方不可控制
	url := fmt.Sprintf("/api/workspaces/%s/search", r.wsState.ID())
	body := map[string]interface{}{
		"query":  params.Query,
		"mode":   mode,
		"limit":  limit,
		"offset": params.Offset,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		return errorResult(fmt.Sprintf("search failed: %v", err)), nil
	}

	// max_snippet_chars>0 时对每个 result.snippet 按 rune 截断（客户端二次瘦身，属锦上添花）。
	// 结果不是可解析对象或不含 results 数组时原样返回，不视为错误。
	if params.MaxSnippetChars > 0 {
		truncateSearchSnippets(result, params.MaxSnippetChars)
	}

	return jsonResult(result)
}

// truncateSearchSnippets 对 search 结果的 results[].snippet 按 rune 截断到 max 并追加 "..."。
// 引入动机：server 生成 ~200 字符 snippet，Agent 可进一步压缩以省上下文；snippet 可能含 UTF-8 中文，
// 故按 rune 而非字节截断，避免产生乱码。
// 解析失败或结构不符时静默返回原值——截断是锦上添花，不应 fail。
func truncateSearchSnippets(result interface{}, max int) {
	obj, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	results, ok := obj["results"].([]interface{})
	if !ok {
		return
	}
	for _, item := range results {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		snippet, ok := entry["snippet"].(string)
		if !ok {
			continue
		}
		runes := []rune(snippet)
		if len(runes) > max {
			entry["snippet"] = string(runes[:max]) + "..."
		}
	}
}

// --- Source 工具 ---

func (r *Registry) sourceAttachTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "source_attach",
		Description: "Attach a source (provenance) to a document. Supported types: web/code/file/issue/commit/conversation/manual/other.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "Document path",
				},
				"source_type": map[string]interface{}{
					"type":        "string",
					"description": "Source type: web/code/file/issue/commit/conversation/manual/other",
				},
				"value": map[string]interface{}{
					"type":        "string",
					"description": "Source value (URL/path/identifier)",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "Source title (optional)",
				},
				"retrieved_at": map[string]interface{}{
					"type":        "string",
					"description": "Retrieval time in RFC3339 format (optional)",
				},
				"content_hash": map[string]interface{}{
					"type":        "string",
					"description": "Source content hash (optional)",
				},
				"refresh_interval_days": map[string]interface{}{
					"type":        "integer",
					"description": "Refresh interval in days (optional)",
				},
			},
			"required": []string{"path", "source_type", "value"},
		},
	}
}

func (r *Registry) handleSourceAttach(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path                string `json:"path"`
		SourceType          string `json:"source_type"`
		Value               string `json:"value"`
		Title               string `json:"title"`
		RetrievedAt         string `json:"retrieved_at"`
		ContentHash         string `json:"content_hash"`
		RefreshIntervalDays int    `json:"refresh_interval_days"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("failed to parse arguments: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path is required"), nil
	}
	if params.SourceType == "" {
		return errorResult("source_type is required"), nil
	}
	if params.Value == "" {
		return errorResult("value is required"), nil
	}

	// 验证 source_type
	validTypes := map[string]bool{
		"web": true, "code": true, "file": true, "issue": true,
		"commit": true, "conversation": true, "manual": true, "other": true,
	}
	if !validTypes[params.SourceType] {
		return errorResult(fmt.Sprintf("invalid source_type: %s", params.SourceType)), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/sources?path=%s", r.wsState.ID(), params.Path)
	body := map[string]interface{}{
		"source_type":           params.SourceType,
		"value":                 params.Value,
		"title":                 params.Title,
		"retrieved_at":          params.RetrievedAt,
		"content_hash":          params.ContentHash,
		"refresh_interval_days": params.RefreshIntervalDays,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		return errorResult(fmt.Sprintf("failed to attach source: %v", err)), nil
	}

	return jsonResult(result)
}
