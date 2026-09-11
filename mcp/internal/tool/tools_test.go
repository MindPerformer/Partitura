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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"partitura/mcp/internal/cache"
	"partitura/mcp/internal/client"
	"partitura/mcp/internal/credential"
	"partitura/mcp/internal/guidance"
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusInternalServerError)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		fs.requests = append(fs.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Body:   string(body),
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
		"document_list", "document_outline", "document_read",
		"document_history", "document_revision",
		"document_create", "document_patch", "document_replace", "upload_document_file", "document_move",
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
			// 验证错误消息包含 "no active workspace"
			if len(result.Content) > 0 {
				text := result.Content[0].Text
				if !strings.Contains(text, "no active workspace") {
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
	if !strings.Contains(result.Content[0].Text, "not found") {
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
	if !strings.Contains(result.Content[0].Text, "matches 3 times") {
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

// --- document_read 三模式（全文/段读/行读 + 互斥校验）---

func TestDocumentReadModes(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"doc-1","path":"test.md","content_markdown":"full","content_hash":"h","revision_number":1}`))
	}))
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/section", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"path":"test.md","section_path":["A"],"content_markdown":"section body"}`))
	}))
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/lines", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"path":"test.md","start_line":1,"end_line":3,"lines":["a","b","c"]}`))
	}))

	cli := newTestClient(t, fs)
	r := newTestRegistry(cli)
	r.wsState.Switch("ws-1", "Test Workspace")

	// (a) 无参=全文读
	result, err := callToolByName(r, "document_read", json.RawMessage(`{"path":"test.md"}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("全文读应成功: %s", result.Content[0].Text)
	}
	if last := fs.requests[len(fs.requests)-1]; !strings.HasSuffix(last.Path, "/documents/read") {
		t.Errorf("全文读应打到 /documents/read, 得到 %s", last.Path)
	}

	// (b) section_path → 段读
	result, err = callToolByName(r, "document_read", json.RawMessage(`{"path":"test.md","section_path":["A"]}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("段读应成功: %s", result.Content[0].Text)
	}
	if last := fs.requests[len(fs.requests)-1]; !strings.HasSuffix(last.Path, "/documents/section") {
		t.Errorf("段读应打到 /documents/section, 得到 %s", last.Path)
	}

	// (c) start_line+end_line → 行读
	result, err = callToolByName(r, "document_read", json.RawMessage(`{"path":"test.md","start_line":1,"end_line":3}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("行读应成功: %s", result.Content[0].Text)
	}
	if last := fs.requests[len(fs.requests)-1]; !strings.HasSuffix(last.Path, "/documents/lines") {
		t.Errorf("行读应打到 /documents/lines, 得到 %s", last.Path)
	}

	// (d) 互斥：section_path + start_line 同时给出 → 报错
	result, err = callToolByName(r, "document_read", json.RawMessage(`{"path":"test.md","section_path":["A"],"start_line":1,"end_line":2}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "not both") {
		t.Errorf("互斥参数应报错 'not both', 得到: %v", result.Content[0].Text)
	}

	// (d) 单边：只给 start_line → 报错
	result, err = callToolByName(r, "document_read", json.RawMessage(`{"path":"test.md","start_line":1}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "must be provided together") {
		t.Errorf("单边参数应报错 'must be provided together', 得到: %v", result.Content[0].Text)
	}

	// (d) 行读范围超限 → 报错
	result, err = callToolByName(r, "document_read", json.RawMessage(`{"path":"test.md","start_line":1,"end_line":600}`))
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "500-line limit") {
		t.Errorf("超限行读应报错 '500-line limit', 得到: %v", result.Content[0].Text)
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
	if !strings.Contains(text, "truncated") {
		t.Errorf("bootstrap 结果应包含截断标记: %s", text[:min(200, len(text))])
	}
}

// --- upload_document_file ---

func TestUploadDocumentFileCreatesDocumentFromLocalFile(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	var receivedBody map[string]interface{}
	fs.mux.HandleFunc("POST /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("解析上传请求失败: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"path":"notes/imported.md","title":"Imported file","revision_number":1}`))
	}))

	localDir := t.TempDir()
	localPath := filepath.Join(localDir, "nested", "source.md")
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		t.Fatalf("创建测试目录失败: %v", err)
	}
	content := "# Uploaded\\n\\nContent from local file.\\n"
	if err := os.WriteFile(localPath, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	args, err := json.Marshal(map[string]string{
		"path":      "notes/imported.md",
		"file_path": localPath,
	})
	if err != nil {
		t.Fatalf("序列化参数失败: %v", err)
	}

	result, err := callToolByName(r, "upload_document_file", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("上传失败: %s", result.Content[0].Text)
	}
	if receivedBody["path"] != "notes/imported.md" || receivedBody["title"] != "source" {
		t.Fatalf("路径或默认标题错误: %#v", receivedBody)
	}
	if receivedBody["content_markdown"] != content {
		t.Fatalf("上传正文 = %#v, 期望 %#v", receivedBody["content_markdown"], content)
	}
	if len(fs.requests) != 1 || fs.requests[0].Method != http.MethodPost {
		t.Fatalf("上传应只调用一次 POST 创建接口: %#v", fs.requests)
	}
	if fs.requests[0].Auth != "Bearer test-access-token" {
		t.Fatalf("Authorization = %q", fs.requests[0].Auth)
	}
}

func TestUploadDocumentFileRejectsInvalidLocalFiles(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")

	tests := []struct {
		name     string
		filePath string
		wantText string
	}{
		{name: "missing", filePath: filepath.Join(t.TempDir(), "missing.md"), wantText: "failed to stat local file"},
		{name: "directory", filePath: t.TempDir(), wantText: "must refer to a regular file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(map[string]string{"path": "notes/existing.md", "file_path": tc.filePath})
			if err != nil {
				t.Fatalf("序列化参数失败: %v", err)
			}
			result, err := callToolByName(r, "upload_document_file", args)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if !result.IsError || !strings.Contains(result.Content[0].Text, tc.wantText) {
				t.Fatalf("错误结果 = %#v", result)
			}
		})
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.md")
	if err := os.WriteFile(invalidPath, []byte{0xff, 0xfe}, 0644); err != nil {
		t.Fatalf("写入无效文件失败: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"path": "notes/existing.md", "file_path": invalidPath})
	result, err := callToolByName(r, "upload_document_file", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "valid UTF-8") {
		t.Fatalf("无效 UTF-8 错误结果 = %#v", result)
	}
	if len(fs.requests) != 0 {
		t.Fatalf("本地文件校验失败后不应发起请求: %#v", fs.requests)
	}
}

func TestUploadDocumentFileRejectsExistingDestination(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	fs.mux.HandleFunc("POST /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"目标路径已存在"}`))
	}))

	localPath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(localPath, []byte("# Content\\n"), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	args, _ := json.Marshal(map[string]string{"path": "notes/existing.md", "file_path": localPath})
	result, err := callToolByName(r, "upload_document_file", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "set overwrite to true") {
		t.Fatalf("已存在目标错误结果 = %#v", result)
	}
	if len(fs.requests) != 1 || fs.requests[0].Method != http.MethodPost {
		t.Fatalf("目标冲突应只调用创建接口: %#v", fs.requests)
	}
}

// TestUploadDocumentFileOverwriteReplacesExistingDocument 验证 overwrite=true 时
// 先读取目标文档的 revision/hash，再用 PUT 覆盖，且不调用创建接口。
func TestUploadDocumentFileOverwriteReplacesExistingDocument(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	var replaceBody map[string]interface{}
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"title":"Existing title","type":"guide","content_hash":"old-hash","revision_number":4}`))
	}))
	fs.mux.HandleFunc("PUT /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&replaceBody); err != nil {
			t.Fatalf("解析覆盖请求失败: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"path":"notes/existing.md","revision_number":5}`))
	}))

	localPath := filepath.Join(t.TempDir(), "overwrite.md")
	content := "# Replaced\n"
	if err := os.WriteFile(localPath, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	args, err := json.Marshal(map[string]interface{}{
		"path":      "notes/existing.md",
		"file_path": localPath,
		"overwrite": true,
	})
	if err != nil {
		t.Fatalf("序列化参数失败: %v", err)
	}

	result, err := callToolByName(r, "upload_document_file", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("覆盖上传失败: %s", result.Content[0].Text)
	}
	if replaceBody["title"] != "Existing title" || replaceBody["type"] != "guide" {
		t.Fatalf("未保留已有文档元数据: %#v", replaceBody)
	}
	if replaceBody["content_markdown"] != content {
		t.Fatalf("覆盖正文 = %#v, 期望 %#v", replaceBody["content_markdown"], content)
	}
	if replaceBody["expected_revision"] != float64(4) || replaceBody["expected_hash"] != "old-hash" {
		t.Fatalf("覆盖请求缺少乐观并发控制参数: %#v", replaceBody)
	}
	if len(fs.requests) != 2 || fs.requests[0].Method != http.MethodGet || fs.requests[1].Method != http.MethodPut {
		t.Fatalf("overwrite 应为 GET + PUT 两个请求: %#v", fs.requests)
	}
}

// TestUploadDocumentFileOverwriteRequiresExistingDocument 验证 overwrite=true 时
// 目标文档不存在会失败，且不会退化为创建。
func TestUploadDocumentFileOverwriteRequiresExistingDocument(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"文档不存在"}`))
	}))

	localPath := filepath.Join(t.TempDir(), "source.md")
	if err := os.WriteFile(localPath, []byte("# Content\n"), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	args, _ := json.Marshal(map[string]interface{}{
		"path":      "notes/missing.md",
		"file_path": localPath,
		"overwrite": true,
	})
	result, err := callToolByName(r, "upload_document_file", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "requires an existing destination document") {
		t.Fatalf("overwrite 缺失目标错误结果 = %#v", result)
	}
	for _, request := range fs.requests {
		if request.Method == http.MethodPost || request.Method == http.MethodPut {
			t.Fatalf("overwrite 目标缺失时不应写文档: %#v", fs.requests)
		}
	}
}

// TestUploadDocumentFileRejectsRelativePath 验证 file_path 必须是绝对路径，
// 避免 MCP 进程工作目录与用户预期不一致导致读错文件。
func TestUploadDocumentFileRejectsRelativePath(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")

	args, _ := json.Marshal(map[string]string{"path": "notes/existing.md", "file_path": filepath.Join("relative", "dir", "file.md")})
	result, err := callToolByName(r, "upload_document_file", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "must be an absolute path") {
		t.Fatalf("相对路径错误结果 = %#v", result)
	}
	if len(fs.requests) != 0 {
		t.Fatalf("相对路径应在本地拒绝，不发请求: %#v", fs.requests)
	}
}

// --- document_archive delete 选项 ---

// TestDocumentArchiveDeleteUsesPurge 验证 delete=true 走永久删除接口。
func TestDocumentArchiveDeleteUsesPurge(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	fs.mux.HandleFunc("POST /api/workspaces/ws-1/documents/purge", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "notes/doomed.md" {
			t.Errorf("purge path = %q", r.URL.Query().Get("path"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))

	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	args, _ := json.Marshal(map[string]interface{}{"path": "notes/doomed.md", "delete": true})
	result, err := callToolByName(r, "document_archive", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("永久删除失败: %s", result.Content[0].Text)
	}
	if len(fs.requests) != 1 || fs.requests[0].Method != http.MethodPost {
		t.Fatalf("delete 应只调用一次 POST purge: %#v", fs.requests)
	}
}

// TestDocumentArchiveDeleteRequiresOwnerPermission 验证服务端 403 被映射为清晰的权限错误。
func TestDocumentArchiveDeleteRequiresOwnerPermission(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	fs.mux.HandleFunc("POST /api/workspaces/ws-1/documents/purge", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"永久删除需要 owner 权限"}`))
	}))

	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	args, _ := json.Marshal(map[string]interface{}{"path": "notes/doomed.md", "delete": true})
	result, err := callToolByName(r, "document_archive", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "owner permission") {
		t.Fatalf("权限错误结果 = %#v", result)
	}
}

// TestDocumentArchiveWithoutDeleteRequiresRevisionAndHash 验证默认归档仍需要乐观并发参数。
func TestDocumentArchiveWithoutDeleteRequiresRevisionAndHash(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")

	args, _ := json.Marshal(map[string]interface{}{"path": "notes/keep.md"})
	result, err := callToolByName(r, "document_archive", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content[0].Text, "required when delete is false") {
		t.Fatalf("缺失并发参数错误结果 = %#v", result)
	}
	if len(fs.requests) != 0 {
		t.Fatalf("缺参数应在本地拒绝，不发请求: %#v", fs.requests)
	}
}

// TestDocumentArchiveWithoutDeleteArchives 验证默认行为仍走 archive 接口。
func TestDocumentArchiveWithoutDeleteArchives(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	fs.mux.HandleFunc("POST /api/workspaces/ws-1/documents/archive", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"path":"notes/keep.md","status":"archived"}`))
	}))

	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	args, _ := json.Marshal(map[string]interface{}{
		"path":              "notes/keep.md",
		"expected_revision": 2,
		"expected_hash":     "hash-2",
	})
	result, err := callToolByName(r, "document_archive", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("归档失败: %s", result.Content[0].Text)
	}
	if len(fs.requests) != 1 || fs.requests[0].Method != http.MethodPost {
		t.Fatalf("归档应只调用一次 POST archive: %#v", fs.requests)
	}
}

// --- 写操作 verbose 语义 ---

// newSwitchedRegistry 创建已 switch 到 ws-1 的注册中心。
func newSwitchedRegistry(t *testing.T, fs *fakeServer) *Registry {
	t.Helper()
	r := newTestRegistry(newTestClient(t, fs))
	r.wsState.Switch("ws-1", "Test Workspace")
	return r
}

// writeLocalFile 写入测试用本地文件并返回绝对路径。
func writeLocalFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	return path
}

// documentWriteResponse 是写接口返回的文档响应，故意包含完整正文。
const documentWriteResponse = `{"path":"notes/target.md","title":"T","content_hash":"h","revision_number":3,"content_markdown":"# Very long body\n"}`

// TestWriteToolsOmitContentUnlessVerbose 验证写操作默认不回传正文，避免上下文膨胀；
// 只有 verbose=true 才返回 content_markdown。
func TestWriteToolsOmitContentUnlessVerbose(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		pattern string
		tool    string
		args    map[string]interface{}
	}{
		{
			name:    "document_create",
			method:  http.MethodPost,
			pattern: "POST /api/workspaces/ws-1/documents",
			tool:    "document_create",
			args:    map[string]interface{}{"path": "notes/target.md", "title": "T", "content_markdown": "# Body\n"},
		},
		{
			name:    "document_replace",
			method:  http.MethodPut,
			pattern: "PUT /api/workspaces/ws-1/documents",
			tool:    "document_replace",
			args: map[string]interface{}{
				"path": "notes/target.md", "title": "T", "content_markdown": "# Body\n",
				"expected_revision": 2, "expected_hash": "h",
			},
		},
		{
			name:    "document_move",
			method:  http.MethodPost,
			pattern: "POST /api/workspaces/ws-1/documents/move",
			tool:    "document_move",
			args: map[string]interface{}{
				"path": "notes/target.md", "new_path": "notes/moved.md",
				"expected_revision": 2, "expected_hash": "h",
			},
		},
		{
			name:    "document_archive",
			method:  http.MethodPost,
			pattern: "POST /api/workspaces/ws-1/documents/archive",
			tool:    "document_archive",
			args:    map[string]interface{}{"path": "notes/target.md", "expected_revision": 2, "expected_hash": "h"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFakeServer()
			defer fs.close()
			fs.mux.HandleFunc(tc.pattern, fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(documentWriteResponse))
			}))

			r := newSwitchedRegistry(t, fs)

			// 默认：响应不含正文。
			args, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatalf("序列化参数失败: %v", err)
			}
			result, err := callToolByName(r, tc.tool, args)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if result.IsError {
				t.Fatalf("调用失败: %s", result.Content[0].Text)
			}
			text := result.Content[0].Text
			if strings.Contains(text, "content_markdown") {
				t.Fatalf("默认响应不应包含 content_markdown: %s", text)
			}
			if !strings.Contains(text, `"revision_number":3`) {
				t.Fatalf("默认响应应保留元数据: %s", text)
			}

			// verbose=true：响应包含正文。
			verboseArgs := make(map[string]interface{}, len(tc.args)+1)
			for k, v := range tc.args {
				verboseArgs[k] = v
			}
			verboseArgs["verbose"] = true
			args, err = json.Marshal(verboseArgs)
			if err != nil {
				t.Fatalf("序列化参数失败: %v", err)
			}
			result, err = callToolByName(r, tc.tool, args)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if result.IsError {
				t.Fatalf("调用失败: %s", result.Content[0].Text)
			}
			if !strings.Contains(result.Content[0].Text, "# Very long body") {
				t.Fatalf("verbose=true 应包含正文: %s", result.Content[0].Text)
			}
		})
	}
}

// TestUploadDocumentFileOmitContentUnlessVerbose 验证 upload 的创建与覆盖两条分支
// 都遵循默认不回传正文的约定。
func TestUploadDocumentFileOmitContentUnlessVerbose(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"title":"T","type":"guide","content_hash":"h","revision_number":2}`))
	}))
	fs.mux.HandleFunc("POST /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(documentWriteResponse))
	}))
	fs.mux.HandleFunc("PUT /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(documentWriteResponse))
	}))

	localPath := writeLocalFile(t, "source.md", "# Body\n")
	r := newSwitchedRegistry(t, fs)

	for _, overwrite := range []bool{false, true} {
		name := "create"
		if overwrite {
			name = "overwrite"
		}
		t.Run(name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]interface{}{
				"path":      "notes/target.md",
				"file_path": localPath,
				"overwrite": overwrite,
			})
			result, err := callToolByName(r, "upload_document_file", args)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if result.IsError {
				t.Fatalf("调用失败: %s", result.Content[0].Text)
			}
			if strings.Contains(result.Content[0].Text, "content_markdown") {
				t.Fatalf("默认响应不应包含 content_markdown: %s", result.Content[0].Text)
			}

			verboseArgs, _ := json.Marshal(map[string]interface{}{
				"path":      "notes/target.md",
				"file_path": localPath,
				"overwrite": overwrite,
				"verbose":   true,
			})
			result, err = callToolByName(r, "upload_document_file", verboseArgs)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if result.IsError {
				t.Fatalf("调用失败: %s", result.Content[0].Text)
			}
			if !strings.Contains(result.Content[0].Text, "# Very long body") {
				t.Fatalf("verbose=true 应包含正文: %s", result.Content[0].Text)
			}
		})
	}
}

// TestDocumentPatchOmitContentUnlessVerbose 验证 patch 成功路径同样默认不回传正文。
func TestDocumentPatchOmitContentUnlessVerbose(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()
	fs.mux.HandleFunc("GET /api/workspaces/ws-1/documents/read", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content_markdown":"# Test\n","content_hash":"base-hash","revision_number":1}`))
	}))
	fs.mux.HandleFunc("PATCH /api/workspaces/ws-1/documents", fs.recordMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(documentWriteResponse))
	}))

	r := newSwitchedRegistry(t, fs)

	args, _ := json.Marshal(map[string]string{
		"path":     "notes/target.md",
		"old_text": "# Test",
		"new_text": "# Patched",
	})
	result, err := callToolByName(r, "document_patch", args)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("patch 失败: %s", result.Content[0].Text)
	}
	if strings.Contains(result.Content[0].Text, "content_markdown") {
		t.Fatalf("默认响应不应包含 content_markdown: %s", result.Content[0].Text)
	}

	verboseArgs, _ := json.Marshal(map[string]interface{}{
		"path":     "notes/target.md",
		"old_text": "# Test",
		"new_text": "# Patched",
		"verbose":  true,
	})
	result, err = callToolByName(r, "document_patch", verboseArgs)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if result.IsError {
		t.Fatalf("patch 失败: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "# Very long body") {
		t.Fatalf("verbose=true 应包含正文: %s", result.Content[0].Text)
	}
}

// TestStripContentMarkdownHandlesNestedStructures 验证剥离逻辑覆盖嵌套 map 与数组，
// 因为 patch 的 rebase 结果会把文档响应嵌在 result 字段里。
func TestStripContentMarkdownHandlesNestedStructures(t *testing.T) {
	input := map[string]interface{}{
		"rebased": true,
		"result": map[string]interface{}{
			"content_markdown": "# Nested\n",
			"revision_number":  2,
		},
		"entries": []interface{}{
			map[string]interface{}{"content_markdown": "# Listed\n", "path": "a.md"},
		},
	}

	stripped := stripContentMarkdown(input)

	if _, exists := stripped.(map[string]interface{})["content_markdown"]; exists {
		t.Fatal("顶层 content_markdown 未被移除")
	}
	nested := stripped.(map[string]interface{})["result"].(map[string]interface{})
	if _, exists := nested["content_markdown"]; exists {
		t.Fatal("嵌套 content_markdown 未被移除")
	}
	if nested["revision_number"] != 2 {
		t.Fatalf("嵌套元数据被误删: %#v", nested)
	}
	entry := stripped.(map[string]interface{})["entries"].([]interface{})[0].(map[string]interface{})
	if _, exists := entry["content_markdown"]; exists {
		t.Fatal("数组内 content_markdown 未被移除")
	}
	if entry["path"] != "a.md" {
		t.Fatalf("数组内元数据被误删: %#v", entry)
	}
}

// --- tools/list 契约 ---

// registeredTool 是 tools/list 响应中的单个工具，字段与 MCP 协议保持一致。
type registeredTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// hasCJK 判断字符串是否包含中日韩统一表意文字。
// 引入动机：MCP 面向 Agent 的输出统一使用英文，契约测试需要显式检出残留中文。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// findTool 按名称取出工具，缺失即失败。
func findTool(t *testing.T, tools []registeredTool, name string) registeredTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tools/list 缺少工具 %s", name)
	return registeredTool{}
}

// toolPropertyNames 返回工具声明的属性名集合。
func toolPropertyNames(t *testing.T, tool registeredTool) []string {
	t.Helper()
	props, ok := tool.InputSchema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("工具 %s 缺少 properties", tool.Name)
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// toolRequiredNames 返回工具声明的 required 集合。
func toolRequiredNames(t *testing.T, tool registeredTool) []string {
	t.Helper()
	raw, ok := tool.InputSchema["required"].([]interface{})
	if !ok {
		return nil
	}
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		name, ok := item.(string)
		if !ok {
			t.Fatalf("工具 %s required 含非字符串项: %v", tool.Name, item)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestToolsListCJKDetectionIsEffective 防止"无中文"断言变成空断言：
// 必须证明 hasCJK 能识别中文，且 encoding/json 不会把中文转义掉（否则契约测试永远通过）。
func TestToolsListCJKDetectionIsEffective(t *testing.T) {
	if !hasCJK("中文描述") {
		t.Fatal("hasCJK 未能识别中文")
	}
	if hasCJK("english description") {
		t.Fatal("hasCJK 误报英文为中文")
	}

	// tools/list 的响应体经 json.Marshal 产生；中文必须原样保留，才可能被 hasCJK 检出。
	marshaled, err := json.Marshal(registeredTool{Name: "sample", Description: "中文描述"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !hasCJK(string(marshaled)) {
		t.Fatalf("JSON 序列化掩盖了中文，契约测试将失去意义: %s", marshaled)
	}
}

// TestToolsListContractIsEnglishAndComplete 通过真实 JSON-RPC 往返校验 tools/list 契约。
// 引入动机：工具元数据直接面向 Agent，必须保证工具清单完整、每个属性都有类型和描述、
// required 只引用已声明属性，且 initialize instructions 与 tools/list 无中文残留。
func TestToolsListContractIsEnglishAndComplete(t *testing.T) {
	fs := newFakeServer()
	defer fs.close()

	r := newTestRegistry(newTestClient(t, fs))

	input := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"

	var stdout bytes.Buffer
	server := protocol.NewServerWithIO(guidance.MCPGuidance, strings.NewReader(input), &stdout)
	r.RegisterAll(server)

	if err := server.Run(); err != nil {
		t.Fatalf("server.Run 失败: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 个 JSON-RPC 响应，得到 %d: %s", len(lines), stdout.String())
	}
	for _, line := range lines {
		if hasCJK(line) {
			t.Fatalf("MCP 响应残留中文: %s", line)
		}
	}

	var listResp struct {
		Result struct {
			Tools []registeredTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatalf("解析 tools/list 响应失败: %v", err)
	}

	gotNames := make([]string, 0, len(listResp.Result.Tools))
	for _, tool := range listResp.Result.Tools {
		gotNames = append(gotNames, tool.Name)
	}
	sort.Strings(gotNames)

	wantNames := []string{
		"document_archive", "document_create", "document_history", "document_list",
		"document_move", "document_outline", "document_patch", "document_read",
		"document_replace", "document_revision",
		"knowledge_search", "source_attach", "switch_workspace", "upload_document_file",
		"workspace_bootstrap", "workspace_current", "workspace_list",
	}
	sort.Strings(wantNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("工具清单不匹配\n got: %v\nwant: %v", gotNames, wantNames)
	}

	for _, tool := range listResp.Result.Tools {
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("工具 %s 缺少 description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("工具 %s inputSchema.type = %v, 期望 object", tool.Name, tool.InputSchema["type"])
		}

		props, ok := tool.InputSchema["properties"].(map[string]interface{})
		if !ok {
			t.Errorf("工具 %s 缺少 properties", tool.Name)
			continue
		}
		for name, rawProp := range props {
			prop, ok := rawProp.(map[string]interface{})
			if !ok {
				t.Errorf("工具 %s 属性 %s 不是对象", tool.Name, name)
				continue
			}
			typ, _ := prop["type"].(string)
			if typ == "" {
				t.Errorf("工具 %s 属性 %s 缺少 type", tool.Name, name)
			}
			if name == "verbose" && typ != "boolean" {
				t.Errorf("工具 %s 的 verbose 类型应为 boolean, 得到 %v", tool.Name, typ)
			}
			if desc, _ := prop["description"].(string); strings.TrimSpace(desc) == "" {
				t.Errorf("工具 %s 属性 %s 缺少 description", tool.Name, name)
			}
		}
		for _, name := range toolRequiredNames(t, tool) {
			if _, ok := props[name]; !ok {
				t.Errorf("工具 %s 的 required 引用未声明属性 %s", tool.Name, name)
			}
		}
	}

	// upload_document_file：本地文件导入参数，overwrite 控制是否覆盖已有文档。
	upload := findTool(t, listResp.Result.Tools, "upload_document_file")
	wantUploadProps := []string{"file_path", "overwrite", "path", "title", "type", "verbose"}
	if got := toolPropertyNames(t, upload); !reflect.DeepEqual(got, wantUploadProps) {
		t.Errorf("upload_document_file properties = %v, 期望 %v", got, wantUploadProps)
	}
	if got, want := toolRequiredNames(t, upload), []string{"file_path", "path"}; !reflect.DeepEqual(got, want) {
		t.Errorf("upload_document_file required = %v, 期望 %v", got, want)
	}
	uploadProps, _ := upload.InputSchema["properties"].(map[string]interface{})
	if got := uploadProps["overwrite"].(map[string]interface{})["type"]; got != "boolean" {
		t.Errorf("upload_document_file overwrite type = %v, 期望 boolean", got)
	}
	if got := uploadProps["file_path"].(map[string]interface{})["type"]; got != "string" {
		t.Errorf("upload_document_file file_path type = %v, 期望 string", got)
	}

	// document_archive：delete 控制归档还是永久删除，required 只含 path。
	archive := findTool(t, listResp.Result.Tools, "document_archive")
	wantArchiveProps := []string{"delete", "expected_hash", "expected_revision", "path", "verbose"}
	if got := toolPropertyNames(t, archive); !reflect.DeepEqual(got, wantArchiveProps) {
		t.Errorf("document_archive properties = %v, 期望 %v", got, wantArchiveProps)
	}
	if got, want := toolRequiredNames(t, archive), []string{"path"}; !reflect.DeepEqual(got, want) {
		t.Errorf("document_archive required = %v, 期望 %v", got, want)
	}
	archiveProps, _ := archive.InputSchema["properties"].(map[string]interface{})
	if got := archiveProps["delete"].(map[string]interface{})["type"]; got != "boolean" {
		t.Errorf("document_archive delete type = %v, 期望 boolean", got)
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
