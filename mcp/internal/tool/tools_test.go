// tools_test.go 测试 MCP 工具的核心逻辑，使用 fake HTTPS server。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求：
//   - fake HTTPS server 验证 MCP 实际调用 REST
//   - Authorization bearer 正确但日志/输出不泄露 token
//   - 工具错误映射
//   - 未 switch 时每个 project 工具拒绝
//   - switch 后 search 请求不可注入或跨 workspace
//   - document patch：唯一匹配、0/多匹配拒绝、409 一次 rebase 成功、rebase 仍冲突返回可操作信息
package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"partitura/mcp/internal/cache"
	"partitura/mcp/internal/client"
	"partitura/mcp/internal/credential"
	"partitura/mcp/internal/protocol"
	"partitura/mcp/internal/workspace"
)

// fakeServer 创建测试用 HTTPS server。
// 引入动机：design 要求使用 fake HTTPS server 而非 fake client 验证 MCP 实际调用 REST。
type fakeServer struct {
	server *httptest.Server
	mux    *http.ServeMux

	// 记录收到的请求
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   string
}

func newFakeServer() *fakeServer {
	mux := http.NewServeMux()
	fs := &fakeServer{mux: mux}

	// 使用 httptest.NewTLSServer 创建自签证书 HTTPS server
	fs.server = httptest.NewTLSServer(mux)
	return fs
}

// recordMiddleware 记录请求信息。
func (fs *fakeServer) recordMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fs.requests = append(fs.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
		})
		next(w, r)
	}
}

// close 关闭 fake server。
func (fs *fakeServer) close() {
	fs.server.Close()
}

// newTestClient 创建连接到 fake server 的 MCP client。
func newTestClient(t *testing.T, fs *fakeServer) *client.Client {
	t.Helper()

	credStore := credential.NewMockStore()
	// 保存测试 token
	_ = credStore.Save(fs.server.URL, &credential.Tokens{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ServerURL:    fs.server.URL,
	})

	cli := client.NewClient(fs.server.URL, &credAdapter{store: credStore}, true)
	if err := cli.LoadTokens(); err != nil {
		t.Fatalf("LoadTokens 失败: %v", err)
	}

	return cli
}

// credAdapter 适配 credential.Store 到 client.CredentialStore。
type credAdapter struct {
	store *credential.MockStore
}

func (a *credAdapter) Load(serverURL string) (*client.CredentialTokens, error) {
	tokens, err := a.store.Load(serverURL)
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		return &client.CredentialTokens{}, nil
	}
	return &client.CredentialTokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ServerURL:    tokens.ServerURL,
	}, nil
}

func (a *credAdapter) Save(serverURL string, tokens *client.CredentialTokens) error {
	return a.store.Save(serverURL, &credential.Tokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ServerURL:    serverURL,
	})
}

func (a *credAdapter) Delete(serverURL string) error {
	return a.store.Delete(serverURL)
}

// newTestRegistry 创建测试用工具注册中心。
func newTestRegistry(cli *client.Client) *Registry {
	wsState := workspace.NewState()
	memCache := cache.New(30*time.Minute, 200)
	return NewRegistry(cli, wsState, memCache, "hybrid", 10)
}

// --- 未 switch 时拒绝 project tools ---

func TestProjectToolsRejectWithoutSwitch(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)

	// 测试所有 project tools 在未 switch 时拒绝
	projectTools := []string{
		"document_list", "document_outline", "document_read", "document_read_section",
		"document_read_lines", "document_history", "document_revision",
		"document_create", "document_patch", "document_replace", "document_move",
		"document_archive", "knowledge_search", "source_attach", "workspace_bootstrap",
	}

	for _, toolName := range projectTools {
		t.Run(toolName, func(t *testing.T) {
			result, err := callToolByName(r, toolName, json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !result.IsError {
				t.Errorf("工具 %s 在未 switch 时应返回错误", toolName)
			}
			// 验证错误消息包含"未切换 workspace"
			if len(result.Content) > 0 {
				text := result.Content[0].Text
				if !strings.Contains(text, "未切换 workspace") {
					t.Errorf("工具 %s 错误消息应包含'未切换 workspace', 得到: %s", toolName, text)
				}
			}
		})
	}
}

// --- workspace_list 不需要 switch ---

func TestWorkspaceListWithoutSwitch(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	fs.mux.HandleFunc("GET /api/workspaces", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"workspaces":[{"id":"ws-1","name":"test","display_name":"Test","description":"","status":"active"}],"total":1,"limit":100,"offset":0}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)

	// workspace_list 不需要 switch
	result, err := callToolByName(r, "workspace_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Errorf("workspace_list 不应在未 switch 时返回错误: %s", result.Content[0].Text)
	}

	// 验证 Authorization header 正确
	if len(fs.requests) == 0 {
		t.Fatal("期望至少 1 个请求")
	}
	auth := fs.requests[0].Auth
	if auth != "Bearer test-access-token" {
		t.Errorf("Authorization = %s, 期望 Bearer test-access-token", auth)
	}
}

// --- Authorization header 正确但不泄露 token ---

func TestTokenNotInOutput(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	fs.mux.HandleFunc("GET /api/workspaces", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"workspaces":[],"total":0,"limit":100,"offset":0}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)

	result, err := callToolByName(r, "workspace_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}

	// 验证工具输出不包含 token
	for _, item := range result.Content {
		if strings.Contains(item.Text, "test-access-token") {
			t.Errorf("工具输出包含 token: %s", item.Text)
		}
		if strings.Contains(item.Text, "test-refresh-token") {
			t.Errorf("工具输出包含 refresh token: %s", item.Text)
		}
	}
}

// --- document_patch 测试 ---

func TestDocumentPatchUniqueMatch(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	// 模拟文档读取
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"doc-1","workspace_id":"ws-1","path":"test.md","title":"Test","status":"active","content_markdown":"Hello World","content_hash":"abc123","revision_number":1,"is_special":false}`))
	}))

	// 模拟 patch 成功——验证请求体包含 content_markdown 和 candidate_hash
	var receivedBody string
	patchCalled := false
	fs.mux.HandleFunc("PATCH /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		patchCalled = true
		bodyBytes := make([]byte, r.ContentLength)
		r.Body.Read(bodyBytes)
		receivedBody = string(bodyBytes)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"doc-1","workspace_id":"ws-1","path":"test.md","title":"Test","status":"active","content_markdown":"Hello Go","content_hash":"def456","revision_number":2,"is_special":false}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	args, _ := json.Marshal(map[string]string{
		"path":     "test.md",
		"old_text": "World",
		"new_text": "Go",
	})

	result, err := callToolByName(r, "document_patch", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Errorf("patch 应成功: %s", result.Content[0].Text)
	}
	if !patchCalled {
		t.Error("期望 PATCH 请求被发送")
	}

	// 验证请求体包含 content_markdown 和 candidate_hash（新契约），
	// 不包含 old_text/new_text（旧契约）
	if !strings.Contains(receivedBody, "content_markdown") {
		t.Errorf("PATCH 请求体应包含 content_markdown: %s", receivedBody)
	}
	if !strings.Contains(receivedBody, "candidate_hash") {
		t.Errorf("PATCH 请求体应包含 candidate_hash: %s", receivedBody)
	}
	if strings.Contains(receivedBody, "old_text") {
		t.Errorf("PATCH 请求体不应包含 old_text（MCP 本地处理）: %s", receivedBody)
	}
	if strings.Contains(receivedBody, "new_text") {
		t.Errorf("PATCH 请求体不应包含 new_text（MCP 本地处理）: %s", receivedBody)
	}

	// 验证 candidate_hash 是 "Hello Go" 的 SHA-256
	expectedHash := sha256Hex("Hello Go")
	if !strings.Contains(receivedBody, expectedHash) {
		t.Errorf("PATCH 请求体应包含正确的 candidate_hash %s: %s", expectedHash, receivedBody)
	}
}

func TestDocumentPatchZeroMatch(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"Hello World","content_hash":"abc123","revision_number":1}`))
	}))

	// PATCH handler 不应被调用——old_text 验证在 MCP 本地完成
	patchCalled := false
	fs.mux.HandleFunc("PATCH /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		patchCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	args, _ := json.Marshal(map[string]string{
		"path":     "test.md",
		"old_text": "NonExistent",
		"new_text": " replacement",
	})

	result, err := callToolByName(r, "document_patch", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError {
		t.Error("old_text 不存在时应返回错误")
	}
	if !strings.Contains(result.Content[0].Text, "未找到") {
		t.Errorf("错误消息应包含'未找到': %s", result.Content[0].Text)
	}
	if patchCalled {
		t.Error("old_text 未匹配时不应发送 PATCH 请求（本地验证失败）")
	}
}

func TestDocumentPatchMultipleMatch(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"Hello Hello Hello","content_hash":"abc123","revision_number":1}`))
	}))

	// PATCH handler 不应被调用——old_text 验证在 MCP 本地完成
	patchCalled := false
	fs.mux.HandleFunc("PATCH /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		patchCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	args, _ := json.Marshal(map[string]string{
		"path":     "test.md",
		"old_text": "Hello",
		"new_text": "Hi",
	})

	result, err := callToolByName(r, "document_patch", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError {
		t.Error("old_text 多次匹配时应返回错误")
	}
	if !strings.Contains(result.Content[0].Text, "3 次") {
		t.Errorf("错误消息应包含匹配次数: %s", result.Content[0].Text)
	}
	if patchCalled {
		t.Error("old_text 多匹配时不应发送 PATCH 请求（本地验证失败）")
	}
}

func TestDocumentPatch409RebaseSuccess(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	// 第一次 read 返回旧版本
	readCount := 0
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		readCount++
		if readCount == 1 {
			// 第一次 read——返回旧版本
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"Hello World","content_hash":"old-hash","revision_number":1}`))
		} else {
			// rebase 时的 read——返回最新版本（old_text 仍存在）
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"Hello World [updated]","content_hash":"new-hash","revision_number":2}`))
		}
	}))

	// 第一次 PATCH 返回 409
	patchCount := 0
	fs.mux.HandleFunc("PATCH /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		patchCount++
		if patchCount == 1 {
			// 第一次 patch——409 冲突
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"版本冲突"}`))
		} else {
			// rebase 后 patch——成功
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"Hi World [updated]","content_hash":"final-hash","revision_number":3}`))
		}
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	args, _ := json.Marshal(map[string]string{
		"path":     "test.md",
		"old_text": "Hello",
		"new_text": "Hi",
	})

	result, err := callToolByName(r, "document_patch", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}

	if result.IsError {
		t.Errorf("rebase 成功后不应返回错误: %s", result.Content[0].Text)
	}

	// 验证 PATCH 被调用了 2 次（第一次 409，第二次成功）
	if patchCount != 2 {
		t.Errorf("PATCH 调用次数 = %d, 期望 2", patchCount)
	}
}

func TestDocumentPatch409RebaseStillConflict(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	// read 返回内容
	readCount := 0
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		readCount++
		if readCount == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"Hello World","content_hash":"old-hash","revision_number":1}`))
		} else {
			// rebase 时的 read——old_text 不在最新内容中
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"Completely different content","content_hash":"new-hash","revision_number":2}`))
		}
	}))

	fs.mux.HandleFunc("PATCH /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"版本冲突"}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	args, _ := json.Marshal(map[string]string{
		"path":     "test.md",
		"old_text": "Hello",
		"new_text": "Hi",
	})

	result, err := callToolByName(r, "document_patch", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}

	// 应返回 conflict 信息（不是 error，而是包含 conflict 字段的结果）
	if result.IsError {
		t.Errorf("rebase 仍冲突不应返回 isError，应返回 conflict 信息: %s", result.Content[0].Text)
	}

	// 验证结果包含 conflict 信息
	var resultMap map[string]interface{}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &resultMap); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}

	// 检查是否有 conflict 或 rebased 字段
	// patch 结果可能是嵌套的
	if resultMap["conflict"] == nil && resultMap["rebased"] == nil {
		// 可能结果在更深层次
		t.Errorf("期望结果包含 conflict 或 rebased 字段: %s", result.Content[0].Text)
	}
}

// --- knowledge_search 不可注入 workspace ---

func TestKnowledgeSearchNoWorkspaceInjection(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	fs.mux.HandleFunc("POST /api/workspaces/ws-1/search", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[],"total":0,"limit":10,"offset":0,"degraded":false,"search_id":"s-1","reranker_used":false}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	// 尝试在参数中注入 workspace_id
	args, _ := json.Marshal(map[string]interface{}{
		"query":        "test query",
		"workspace_id": "ws-2", // 尝试注入其他 workspace
	})

	result, err := callToolByName(r, "knowledge_search", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Errorf("search 不应失败: %s", result.Content[0].Text)
	}

	// 验证请求路径使用的是 active workspace ID（ws-1），不是注入的 ws-2
	// 检查请求路径
	for _, req := range fs.requests {
		if strings.Contains(req.Path, "ws-1") {
			return // 正确——使用了 active workspace
		}
	}
	t.Fatal("期望请求路径包含 ws-1（active workspace）")
}

// --- switch_workspace 返回不含 PROJECT/AGENTS 全文 ---

func TestSwitchWorkspaceNoFullText(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	fs.mux.HandleFunc("GET /api/workspaces/ws-1", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"ws-1","name":"test","display_name":"Test WS","description":"test","status":"active"}`))
	}))

	// 模拟文档存在检查
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"doc-1","path":"PROJECT.md","content_markdown":"very long content...","content_hash":"abc","revision_number":1}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)

	args, _ := json.Marshal(map[string]string{
		"workspace_id": "ws-1",
	})

	result, err := callToolByName(r, "switch_workspace", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Errorf("switch 不应失败: %s", result.Content[0].Text)
	}

	// 验证结果不包含 PROJECT.md 全文
	text := result.Content[0].Text
	if strings.Contains(text, "very long content") {
		t.Errorf("switch 结果不应包含 PROJECT.md 全文: %s", text)
	}
}

// --- bootstrap 有上下文上限 ---

func TestBootstrapContextLimit(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	// 返回超过 100 行的 PROJECT.md
	longContent := strings.Repeat("# Line\n", 150)
	projectJSON, _ := json.Marshal(map[string]interface{}{
		"id":               "doc-1",
		"path":             "PROJECT.md",
		"content_markdown": longContent,
		"content_hash":     "abc",
		"revision_number":  1,
	})

	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(projectJSON)
	}))

	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"documents":[],"total":0,"limit":10,"offset":0}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	result, err := callToolByName(r, "workspace_bootstrap", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Errorf("bootstrap 不应失败: %s", result.Content[0].Text)
	}

	// 验证结果包含截断标记
	text := result.Content[0].Text
	if !strings.Contains(text, "已截断") {
		t.Errorf("bootstrap 结果应包含截断标记: %s", text[:min(200, len(text))])
	}
}

// --- 辅助函数 ---

// callToolByName 按名称调用工具。
func callToolByName(r *Registry, name string, args json.RawMessage) (*protocol.ToolResult, error) {
	return r.callToolForTest(name, args)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sha256Hex 计算字符串的 SHA-256 十六进制摘要。
// 引入动机：测试需要验证 MCP 本地计算的 candidate_hash 是否正确。
func sha256Hex(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}
