// Package tool 实现所有 MCP 工具的注册和执行逻辑。
//
// 引入动机：design/02-MCP.md §Document Tools 和 §Workspace 要求完整实现所有 MCP 工具。
// 每个工具通过 HTTPS REST 调用 server API，严格 schema/输入验证，
// 权限错误传播和结构化结果。
//
// 工具清单：
//   - workspace_list, workspace_current, switch_workspace, workspace_bootstrap
//   - document_list, document_outline, document_read, document_read_section, document_read_lines
//   - document_create, document_patch, document_replace, document_move, document_archive,
//     document_history, document_revision
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
	"strings"
	"time"

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
	server.RegisterTool(r.documentReadSectionTool(), r.handleDocumentReadSection)
	server.RegisterTool(r.documentReadLinesTool(), r.handleDocumentReadLines)
	server.RegisterTool(r.documentHistoryTool(), r.handleDocumentHistory)
	server.RegisterTool(r.documentRevisionTool(), r.handleDocumentRevision)

	// --- Document Write 工具 ---
	server.RegisterTool(r.documentCreateTool(), r.handleDocumentCreate)
	server.RegisterTool(r.documentPatchTool(), r.handleDocumentPatch)
	server.RegisterTool(r.documentReplaceTool(), r.handleDocumentReplace)
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
		"workspace_list":      r.handleWorkspaceList,
		"workspace_current":   r.handleWorkspaceCurrent,
		"switch_workspace":    r.handleSwitchWorkspace,
		"workspace_bootstrap": r.handleWorkspaceBootstrap,
		"document_list":       r.handleDocumentList,
		"document_outline":    r.handleDocumentOutline,
		"document_read":       r.handleDocumentRead,
		"document_read_section": r.handleDocumentReadSection,
		"document_read_lines":   r.handleDocumentReadLines,
		"document_history":      r.handleDocumentHistory,
		"document_revision":     r.handleDocumentRevision,
		"document_create":       r.handleDocumentCreate,
		"document_patch":        r.handleDocumentPatch,
		"document_replace":      r.handleDocumentReplace,
		"document_move":         r.handleDocumentMove,
		"document_archive":      r.handleDocumentArchive,
		"knowledge_search":      r.handleKnowledgeSearch,
		"source_attach":         r.handleSourceAttach,
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
	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化结果: %w", err)
	}
	return textResult(string(jsonBytes)), nil
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
		Description: "列出当前用户可访问的所有 workspace。无需先 switch_workspace。",
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
			Description string `json:"description"`
			Status      string `json:"status"`
		} `json:"workspaces"`
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}

	if err := r.cli.Get(ctx, "/api/workspaces?limit=100", &result); err != nil {
		return errorResult(fmt.Sprintf("获取 workspace 列表失败: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) workspaceCurrentTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "workspace_current",
		Description: "返回当前 active workspace 的状态信息。",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}
}

func (r *Registry) handleWorkspaceCurrent(args json.RawMessage) (*protocol.ToolResult, error) {
	if !r.wsState.IsActive() {
		return textResult("当前未切换 workspace。请先使用 switch_workspace。"), nil
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
		Description: "切换 active workspace。切换后所有 project tools 作用于此 workspace。需要用户有该 workspace 的权限。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"workspace_id": map[string]interface{}{
					"type":        "string",
					"description": "要切换的 workspace UUID",
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
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.WorkspaceID == "" {
		return errorResult("workspace_id 不能为空"), nil
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
			return errorResult("workspace 不存在或无权限"), nil
		}
		return errorResult(fmt.Sprintf("获取 workspace 信息失败: %v", err)), nil
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
		"next_step":      "建议使用 workspace_bootstrap 获取项目概况，或使用 knowledge_search 搜索已有知识。",
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
		Description: "获取当前 workspace 的项目概况：PROJECT.md 和 AGENTS.md 的必要内容、重要入口和推荐先阅读的文档 outline。控制上下文大小，不返回整篇文件。",
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
				projectContent = strings.Join(lines[:100], "\n") + "\n... (已截断，使用 document_read_section 查看更多)"
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
				agentsContent = strings.Join(lines[:100], "\n") + "\n... (已截断，使用 document_read_section 查看更多)"
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
		Description: "列出当前 workspace 的文档。默认隐藏 archived 文档。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "每页数量（默认 20，最大 100）",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "分页偏移",
				},
				"status": map[string]interface{}{
					"type":        "string",
					"description": "按状态过滤：active/draft/archived",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "按类型过滤",
				},
				"include_archived": map[string]interface{}{
					"type":        "boolean",
					"description": "是否包含 archived 文档",
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
		Limit            int    `json:"limit"`
		Offset           int    `json:"offset"`
		Status           string `json:"status"`
		Type             string `json:"type"`
		IncludeArchived  bool   `json:"include_archived"`
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
		return errorResult(fmt.Sprintf("获取文档列表失败: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) documentOutlineTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_outline",
		Description: "返回指定文档的 Markdown heading tree（heading, level, start_line, end_line, section_path）。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径（如 architecture/overview.md）",
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
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
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
		return errorResult(fmt.Sprintf("获取文档 outline 失败: %v", err)), nil
	}

	r.cache.Set(cacheKey, result)
	return jsonResult(result)
}

func (r *Registry) documentReadTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_read",
		Description: "读取指定文档的完整内容（含 revision_number 和 content_hash）。对于长文建议先使用 document_outline。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (r *Registry) handleDocumentRead(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path string `json:"path"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}

	// 检查缓存
	cacheKey := cacheKey(r.wsState.ID(), "read:"+params.Path)
	if cached, ok := r.cache.Get(cacheKey); ok {
		return jsonResult(cached)
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/read?path=%s", r.wsState.ID(), params.Path)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("读取文档失败: %v", err)), nil
	}

	r.cache.Set(cacheKey, result)
	return jsonResult(result)
}

func (r *Registry) documentReadSectionTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_read_section",
		Description: "按 section_path 数组读取指定 section 的 Markdown 内容。section_path 使用结构化数组避免同名 heading 冲突。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"section_path": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "section 路径数组（如 [\"Architecture\", \"Components\"]）",
				},
			},
			"required": []string{"path", "section_path"},
		},
	}
}

func (r *Registry) handleDocumentReadSection(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path        string   `json:"path"`
		SectionPath []string `json:"section_path"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if len(params.SectionPath) == 0 {
		return errorResult("section_path 不能为空"), nil
	}

	// 构建 section_path 查询参数（JSON 数组格式）
	sectionPathJSON, _ := json.Marshal(params.SectionPath)
	url := fmt.Sprintf("/api/workspaces/%s/documents/section?path=%s&section_path=%s",
		r.wsState.ID(), params.Path, string(sectionPathJSON))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("读取 section 失败: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) documentReadLinesTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_read_lines",
		Description: "按行范围读取指定文档的内容。限制最大 500 行。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"start_line": map[string]interface{}{
					"type":        "integer",
					"description": "起始行号（从 1 开始，含）",
				},
				"end_line": map[string]interface{}{
					"type":        "integer",
					"description": "结束行号（含）",
				},
			},
			"required": []string{"path", "start_line", "end_line"},
		},
	}
}

func (r *Registry) handleDocumentReadLines(args json.RawMessage) (*protocol.ToolResult, error) {
	if err := r.wsState.RequireActive(); err != nil {
		return errorResult(err.Error()), nil
	}

	var params struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if params.StartLine < 1 {
		return errorResult("start_line 不能小于 1"), nil
	}
	if params.EndLine < params.StartLine {
		return errorResult("end_line 不能小于 start_line"), nil
	}
	if params.EndLine-params.StartLine+1 > 500 {
		return errorResult("请求行数超过最大限制 500"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/lines?path=%s&start=%d&end=%d",
		r.wsState.ID(), params.Path, params.StartLine, params.EndLine)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("读取行范围失败: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) documentHistoryTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_history",
		Description: "返回指定文档的版本历史列表。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "每页数量",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "分页偏移",
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
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
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
		return errorResult(fmt.Sprintf("获取版本历史失败: %v", err)), nil
	}

	return jsonResult(result)
}

func (r *Registry) documentRevisionTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_revision",
		Description: "读取指定文档的指定版本内容。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"revision": map[string]interface{}{
					"type":        "integer",
					"description": "版本号",
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
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if params.Revision < 1 {
		return errorResult("revision 不能小于 1"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/revision?path=%s&revision=%d",
		r.wsState.ID(), params.Path, params.Revision)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Get(ctx, url, &result); err != nil {
		return errorResult(fmt.Sprintf("获取版本失败: %v", err)), nil
	}

	return jsonResult(result)
}

// --- Document Write 工具 ---

func (r *Registry) documentCreateTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_create",
		Description: "创建新文档。不能创建 PROJECT.md 或 AGENTS.md（特殊文件在 workspace 初始化时自动创建）。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径（如 architecture/overview.md）",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "文档标题",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "文档类型（可选）",
				},
				"content_markdown": map[string]interface{}{
					"type":        "string",
					"description": "Markdown 内容",
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
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if params.Title == "" {
		return errorResult("title 不能为空"), nil
	}
	if params.ContentMarkdown == "" {
		return errorResult("content_markdown 不能为空"), nil
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
		return errorResult(fmt.Sprintf("创建文档失败: %v", err)), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "list"))
	return jsonResult(result)
}

func (r *Registry) documentReplaceTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_replace",
		Description: "替换文档全文。必须携带 expected_revision 和 expected_hash 进行乐观并发控制。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "文档标题",
				},
				"type": map[string]interface{}{
					"type":        "string",
					"description": "文档类型（可选）",
				},
				"content_markdown": map[string]interface{}{
					"type":        "string",
					"description": "新的 Markdown 全文",
				},
				"expected_revision": map[string]interface{}{
					"type":        "integer",
					"description": "期望的当前版本号（用于乐观并发控制）",
				},
				"expected_hash": map[string]interface{}{
					"type":        "string",
					"description": "期望的当前内容哈希（用于乐观并发控制）",
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
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if params.Title == "" {
		return errorResult("title 不能为空"), nil
	}
	if params.ContentMarkdown == "" {
		return errorResult("content_markdown 不能为空"), nil
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
			return errorResult("版本冲突：expected_revision/expected_hash 不匹配，请重新 document_read 获取最新内容"), nil
		}
		return errorResult(fmt.Sprintf("替换文档失败: %v", err)), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "read:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "outline:"+params.Path))
	return jsonResult(result)
}

func (r *Registry) documentMoveTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_move",
		Description: "移动文档到新路径。必须携带 expected_revision 和 expected_hash。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "源文档路径",
				},
				"new_path": map[string]interface{}{
					"type":        "string",
					"description": "目标文档路径",
				},
				"expected_revision": map[string]interface{}{
					"type":        "integer",
					"description": "期望的当前版本号",
				},
				"expected_hash": map[string]interface{}{
					"type":        "string",
					"description": "期望的当前内容哈希",
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
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if params.NewPath == "" {
		return errorResult("new_path 不能为空"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/move?path=%s", r.wsState.ID(), params.Path)
	body := map[string]interface{}{
		"new_path":         params.NewPath,
		"expected_revision": params.ExpectedRevision,
		"expected_hash":     params.ExpectedHash,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		if apiErr, ok := err.(*client.APIError); ok && apiErr.IsConflict() {
			return errorResult("版本冲突或目标路径已存在"), nil
		}
		return errorResult(fmt.Sprintf("移动文档失败: %v", err)), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "read:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "outline:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "list"))
	return jsonResult(result)
}

func (r *Registry) documentArchiveTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_archive",
		Description: "归档文档。归档后文档默认不出现在列表和搜索结果中。必须携带 expected_revision 和 expected_hash。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"expected_revision": map[string]interface{}{
					"type":        "integer",
					"description": "期望的当前版本号",
				},
				"expected_hash": map[string]interface{}{
					"type":        "string",
					"description": "期望的当前内容哈希",
				},
			},
			"required": []string{"path", "expected_revision", "expected_hash"},
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
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/archive?path=%s", r.wsState.ID(), params.Path)
	body := map[string]interface{}{
		"expected_revision": params.ExpectedRevision,
		"expected_hash":     params.ExpectedHash,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		if apiErr, ok := err.(*client.APIError); ok && apiErr.IsConflict() {
			return errorResult("版本冲突：expected_revision/expected_hash 不匹配"), nil
		}
		return errorResult(fmt.Sprintf("归档文档失败: %v", err)), nil
	}

	// 失效缓存
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "read:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "outline:"+params.Path))
	r.cache.Invalidate(cacheKey(r.wsState.ID(), "list"))
	return jsonResult(result)
}

// --- document_patch 工具（核心：本地 patch 算法 + 409 一次 rebase）---

func (r *Registry) documentPatchTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "document_patch",
		Description: "本地 patch 文档：获取 base revision/content → 验证 old_text 唯一匹配 → 生成 candidate → 计算 hash → 上传。409 时自动尝试一次安全 rebase。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"old_text": map[string]interface{}{
					"type":        "string",
					"description": "要替换的原文（必须在文档中恰好唯一匹配）",
				},
				"new_text": map[string]interface{}{
					"type":        "string",
					"description": "替换后的新文本",
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
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if params.OldText == "" {
		return errorResult("old_text 不能为空"), nil
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
			return errorResult(fmt.Sprintf("获取文档失败: %v", err)), nil
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

	return jsonResult(patchResult)
}

// applyPatchAndUpload 执行 patch 的核心逻辑：验证唯一匹配 → 本地生成 candidate → 计算 hash → 上传。
// 引入动机：document_patch 的本地算法，严格遵循 design/02-MCP.md §document_patch。
// 不能仅把 old/new 交服务器处理——必须在本地完成 candidate 生成和 hash 计算。
func (r *Registry) applyPatchAndUpload(wsID, path, oldText, newText, baseContent, baseHash string, baseRevision int) (interface{}, error) {
	// 1. 验证 old_text 在 base content 中恰好唯一匹配
	count := strings.Count(baseContent, oldText)
	if count == 0 {
		return nil, fmt.Errorf("old_text 在文档中未找到（0 次匹配）")
	}
	if count > 1 {
		return nil, fmt.Errorf("old_text 在文档中匹配 %d 次，必须唯一匹配", count)
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
		return nil, fmt.Errorf("patch 上传失败: %w", err)
	}

	// 5. 409 冲突——尝试一次安全 rebase
	slog.Info("patch 409 冲突，尝试一次安全 rebase", "path", path)

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
		return nil, fmt.Errorf("rebase: 获取最新文档失败: %w", err)
	}

	// 检查 old_text 在最新内容中是否仍唯一匹配
	latestCount := strings.Count(latestDoc.ContentMarkdown, oldText)
	if latestCount == 0 {
		// old_text 在最新内容中不存在——返回完整 conflict 信息
		return map[string]interface{}{
			"conflict":        true,
			"reason":          "old_text 在最新内容中未找到（可能已被修改或删除）",
			"base_revision":   baseRevision,
			"base_hash":       baseHash,
			"latest_revision": latestDoc.RevisionNumber,
			"latest_hash":     latestDoc.ContentHash,
			"suggestion":      "请重新 document_read 获取最新内容，确认 old_text 仍然存在后重试。",
		}, nil
	}
	if latestCount > 1 {
		return map[string]interface{}{
			"conflict":        true,
			"reason":          fmt.Sprintf("old_text 在最新内容中匹配 %d 次，无法自动 rebase", latestCount),
			"base_revision":   baseRevision,
			"base_hash":       baseHash,
			"latest_revision": latestDoc.RevisionNumber,
			"latest_hash":     latestDoc.ContentHash,
			"suggestion":      "请重新 document_read 获取最新内容，使用更具体的 old_text 确保唯一匹配后重试。",
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
			"reason":          fmt.Sprintf("rebase 后 patch 仍失败: %v", err),
			"base_revision":   baseRevision,
			"base_hash":       baseHash,
			"latest_revision": latestDoc.RevisionNumber,
			"latest_hash":     latestDoc.ContentHash,
			"suggestion":      "请重新 document_read 获取最新内容后手动修改。",
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
		Description: "搜索当前 active workspace 的知识库。永远只搜索 active workspace，不支持跨 workspace 搜索。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "搜索查询",
				},
				"mode": map[string]interface{}{
					"type":        "string",
					"description": "搜索模式：hybrid（默认）、lexical、semantic",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "结果数量上限",
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "分页偏移",
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
		Query  string `json:"query"`
		Mode   string `json:"mode"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Query == "" {
		return errorResult("query 不能为空"), nil
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
		return errorResult(fmt.Sprintf("搜索失败: %v", err)), nil
	}

	return jsonResult(result)
}

// --- Source 工具 ---

func (r *Registry) sourceAttachTool() *protocol.Tool {
	return &protocol.Tool{
		Name:        "source_attach",
		Description: "为指定文档添加来源（provenance）。支持 web/code/file/issue/commit/conversation/manual/other 类型。",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"path": map[string]interface{}{
					"type":        "string",
					"description": "文档路径",
				},
				"source_type": map[string]interface{}{
					"type":        "string",
					"description": "来源类型：web/code/file/issue/commit/conversation/manual/other",
				},
				"value": map[string]interface{}{
					"type":        "string",
					"description": "来源值（URL/path/identifier）",
				},
				"title": map[string]interface{}{
					"type":        "string",
					"description": "来源标题（可选）",
				},
				"retrieved_at": map[string]interface{}{
					"type":        "string",
					"description": "获取时间（RFC3339 格式，可选）",
				},
				"content_hash": map[string]interface{}{
					"type":        "string",
					"description": "来源内容哈希（可选）",
				},
				"refresh_interval_days": map[string]interface{}{
					"type":        "integer",
					"description": "刷新间隔天数（可选）",
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
		Path               string `json:"path"`
		SourceType         string `json:"source_type"`
		Value              string `json:"value"`
		Title              string `json:"title"`
		RetrievedAt        string `json:"retrieved_at"`
		ContentHash        string `json:"content_hash"`
		RefreshIntervalDays int    `json:"refresh_interval_days"`
	}
	if err := parseArgs(args, &params); err != nil {
		return errorResult(fmt.Sprintf("参数解析失败: %v", err)), nil
	}
	if params.Path == "" {
		return errorResult("path 不能为空"), nil
	}
	if params.SourceType == "" {
		return errorResult("source_type 不能为空"), nil
	}
	if params.Value == "" {
		return errorResult("value 不能为空"), nil
	}

	// 验证 source_type
	validTypes := map[string]bool{
		"web": true, "code": true, "file": true, "issue": true,
		"commit": true, "conversation": true, "manual": true, "other": true,
	}
	if !validTypes[params.SourceType] {
		return errorResult(fmt.Sprintf("非法 source_type: %s", params.SourceType)), nil
	}

	url := fmt.Sprintf("/api/workspaces/%s/documents/sources?path=%s", r.wsState.ID(), params.Path)
	body := map[string]interface{}{
		"source_type":          params.SourceType,
		"value":                params.Value,
		"title":                params.Title,
		"retrieved_at":         params.RetrievedAt,
		"content_hash":         params.ContentHash,
		"refresh_interval_days": params.RefreshIntervalDays,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	if err := r.cli.Post(ctx, url, body, &result); err != nil {
		return errorResult(fmt.Sprintf("添加来源失败: %v", err)), nil
	}

	return jsonResult(result)
}
