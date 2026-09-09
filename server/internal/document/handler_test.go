// handler_test.go 测试 document HTTP handler 经真实 mux + 完整 middleware 链的端到端行为。
//
// 测试覆盖（通过真实 http.ServeMux 及完整链路 AuthMiddleware → RequireAuth → RequireCSRF →
// RequireWorkspacePermission → document handler，不手工注入 Identity）：
//   - RBAC 矩阵：viewer 只读且不能创建；editor 能创建/patch/replace/move但不能archive；
//     admin 能 archive 和删除 source；owner 才可 purge
//   - 非成员、跨 workspace 路径/文档/source 读写均不可越权或泄露
//   - 写操作 CSRF 缺失=403，未认证=401，严格 JSON/非法分页受控拒绝
//   - ReplaceDocument 一次成功只产生一个 revision；并发输入（同 expected revision/hash）第二次 409
//   - Patch candidate_hash 不匹配=400，expected_revision/hash 不匹配=409，正确 candidate=成功
//   - 特殊文件不可被移动（源路径为 PROJECT.md/AGENTS.md）
//   - 已处于目标状态的 archive/restore 幂等不新增 revision
package document

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"partitura/server/internal/auth"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/workspace"
)

// testAuthCfg 返回用于测试的 AuthConfig，使用较低的 Argon2id 参数以加速测试。
func testAuthCfg() auth.AuthConfig {
	return auth.AuthConfig{
		SessionDuration:            3600,
		DeviceAccessTokenDuration:  900,
		DeviceRefreshTokenDuration: 2592000,
		CookieSecure:               false,
		CookiePath:                 "/",
		CookieName:                 "session",
		CSRFCookieName:             "csrf",
		CSRFHeaderName:             "X-CSRF-Token",
		Argon2Memory:               32 * 1024,
		Argon2Iterations:           1,
		Argon2Parallelism:          1,
		Argon2SaltLength:           16,
		Argon2KeyLength:            32,
	}
}

// setupTestEnv 创建完整的测试环境：auth mock repo + workspace mock repo +
// document mock repo + audit mock repo + mux。
// 返回 mux（已包装全局 RequestID + Logging middleware）、authRepo、wsRepo、docRepo、cfg。
// 引入动机：测试需要验证全局 middleware 与认证/权限链的端到端行为，
// 因此 handler 必须与 main.go 一样被全局 middleware 包装。
func setupTestEnv(t *testing.T) (
	mux http.Handler,
	authRepo *LocalAuthMockRepository,
	wsRepo *LocalWSMockRepository,
	docRepo *MockRepository,
	cfg auth.AuthConfig,
) {
	t.Helper()
	cfg = testAuthCfg()
	authRepo = NewLocalAuthMockRepository()
	wsRepo = NewLocalWSMockRepository()
	docRepo = NewMockRepository()
	auditRepo := NewLocalAuditMockRepository()

	docHandler := NewHandler(docRepo, auditRepo, nil)

	rawMux := http.NewServeMux()
	auth.RegisterRoutes(rawMux, auth.NewHandler(authRepo, cfg), authRepo, cfg)
	workspace.RegisterRoutes(rawMux, workspace.NewHandler(wsRepo, auditRepo), workspace.NewAdminHandler(wsRepo, auditRepo), authRepo, cfg)
	RegisterRoutes(rawMux, docHandler, wsRepo, authRepo, cfg)

	// 与 main.go 保持一致：RequestID → Logging → mux
	mux = httpmw.RequestIDMiddleware(httpmw.LoggingMiddleware(rawMux))
	return
}

// createTestUser 创建用户在 auth mock repo 中，返回 userID。
func createTestUser(t *testing.T, authRepo *LocalAuthMockRepository, cfg auth.AuthConfig, userID, username, systemRole string, wsCreatePerm bool) string {
	t.Helper()
	hash, err := auth.HashPassword("testpass123", cfg)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}
	authRepo.AddUser(&auth.User{
		ID:                  userID,
		Username:            username,
		Email:               username + "@test.example",
		PasswordHash:        hash,
		SystemRole:          systemRole,
		WorkspaceCreatePerm: wsCreatePerm,
	})
	return userID
}

// loginAndGetCookies 通过完整 mux 执行 login，返回 session token 和 CSRF token。
func loginAndGetCookies(t *testing.T, mux http.Handler, cfg auth.AuthConfig, username string) (sessionToken, csrfToken string) {
	t.Helper()
	body := `{"username":"` + username + `","password":"testpass123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("login 失败: %d, body: %s", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == cfg.CookieName {
			sessionToken = c.Value
		}
		if c.Name == cfg.CSRFCookieName {
			csrfToken = c.Value
		}
	}
	if sessionToken == "" || csrfToken == "" {
		t.Fatal("login 未返回 session/CSRF cookie")
	}
	return
}

// authedRequest 创建带认证 cookie 和 CSRF header 的请求。
func authedRequest(method, path, sessionToken, csrfToken, body string) *http.Request {
	reader := strings.NewReader("")
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(&http.Cookie{Name: "session", Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: "csrf", Value: csrfToken})
	req.Header.Set("X-CSRF-Token", csrfToken)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// unauthedRequest 创建不带认证的请求。
func unauthedRequest(method, path, body string) *http.Request {
	reader := strings.NewReader("")
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// createTestWorkspace 创建 workspace（owner 为 ownerUserID），返回 workspace ID。
func createTestWorkspace(t *testing.T, wsRepo *LocalWSMockRepository, name, ownerUserID string) string {
	t.Helper()
	ws, err := wsRepo.CreateWorkspace(context.Background(), name, "Test "+name, "", ownerUserID)
	if err != nil {
		t.Fatalf("创建 workspace 失败: %v", err)
	}
	return ws.ID
}

// addMember 向 workspace 添加成员（如果已是 owner 则更新角色）。
// 引入动机：测试中创建 workspace 的 owner 可能需要被降级为 viewer/editor/admin 来测试 RBAC。
func addMember(t *testing.T, wsRepo *LocalWSMockRepository, wsID, userID, role string) {
	t.Helper()
	// 先尝试 AddMember，如果已存在则 UpdateMemberRole
	_, err := wsRepo.AddMember(context.Background(), wsID, userID, role)
	if err != nil {
		_, err = wsRepo.UpdateMemberRole(context.Background(), wsID, userID, role)
		if err != nil {
			t.Fatalf("添加/更新成员失败: %v", err)
		}
	}
}

// createDocDirect 通过 mock repository 直接创建文档，返回文档信息。
func createDocDirect(t *testing.T, docRepo *MockRepository, wsID, path, title, content, userID string) *Document {
	t.Helper()
	hash := ComputeContentHash(content)
	doc, err := docRepo.CreateDocument(context.Background(), wsID, path, title, "", content, hash, false, userID, nil)
	if err != nil {
		t.Fatalf("创建文档失败: %v", err)
	}
	return doc
}

// intToStr 将 int 转为 string。
func intToStr(n int) string {
	return fmt.Sprintf("%d", n)
}

// --- RBAC 矩阵测试 ---

// TestRBAC_ViewerCannotCreate 验证 viewer 不能创建文档。
func TestRBAC_ViewerCannotCreate(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	// 用一个 dummy owner 创建 workspace，然后将被测用户添加为 viewer
	createTestUser(t, authRepo, cfg, "dummy-owner-1", "dummyowner1", "user", true)
	createTestUser(t, authRepo, cfg, "viewer-001", "vieweruser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "viewerws", "dummy-owner-1")
	addMember(t, wsRepo, wsID, "viewer-001", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "vieweruser")

	body := `{"path":"test/doc.md","title":"Test","content_markdown":"# Test"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("viewer 创建文档应返回 403，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRBAC_ViewerCanRead 验证 viewer 可以读取文档。
func TestRBAC_ViewerCanRead(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-2", "dummyowner2", "user", true)
	createTestUser(t, authRepo, cfg, "viewer-002", "viewerread", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "viewreadws", "dummy-owner-2")
	addMember(t, wsRepo, wsID, "viewer-002", workspace.RoleViewer)

	createDocDirect(t, docRepo, wsID, "test/readable.md", "Readable", "# Readable\n\nContent here.\n", "dummy-owner-2")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "viewerread")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents/read?path=test/readable.md", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("viewer 读取文档应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp documentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !strings.Contains(resp.ContentMarkdown, "Readable") {
		t.Errorf("文档内容应包含 'Readable'，实际: %s", resp.ContentMarkdown)
	}
}

// TestRBAC_EditorCanCreatePatchReplaceMove 验证 editor 能创建/patch/replace/move。
func TestRBAC_EditorCanCreatePatchReplaceMove(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-3", "dummyowner3", "user", true)
	createTestUser(t, authRepo, cfg, "editor-001", "editoruser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "editorws", "dummy-owner-3")
	addMember(t, wsRepo, wsID, "editor-001", workspace.RoleEditor)

	docRepo.SetWorkspaceMaxSize(wsID, 2097152)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "editoruser")

	// 创建
	createBody := `{"path":"test/editor.md","title":"Editor Doc","content_markdown":"# Editor\n\nOriginal content.\n"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents", sessionToken, csrfToken, createBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("editor 创建文档应返回 201，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var createdDoc documentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &createdDoc); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}

	// Patch — 使用新契约：MCP 本地生成 candidate content + hash，server 只做乐观并发校验
	patchedContent := "# Editor\n\nPatched content.\n"
	patchedHash := ComputeContentHash(patchedContent)
	patchBody := `{"content_markdown":"` + strings.ReplaceAll(patchedContent, "\n", "\\n") + `","candidate_hash":"` + patchedHash + `","expected_revision":1,"expected_hash":"` + createdDoc.ContentHash + `"}`
	req = authedRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/documents?path=test/editor.md", sessionToken, csrfToken, patchBody)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("editor patch 文档应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var patchedDoc documentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &patchedDoc); err != nil {
		t.Fatalf("解析 patch 响应失败: %v", err)
	}

	// Replace
	replaceBody := `{"title":"Editor Doc v2","content_markdown":"# Editor v2\n\nReplaced content.\n","expected_revision":2,"expected_hash":"` + patchedDoc.ContentHash + `"}`
	req = authedRequest(http.MethodPut, "/api/workspaces/"+wsID+"/documents?path=test/editor.md", sessionToken, csrfToken, replaceBody)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("editor replace 文档应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var replacedDoc documentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &replacedDoc); err != nil {
		t.Fatalf("解析 replace 响应失败: %v", err)
	}

	// Move
	moveBody := `{"new_path":"test/editor-moved.md","expected_revision":3,"expected_hash":"` + replacedDoc.ContentHash + `"}`
	req = authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/move?path=test/editor.md", sessionToken, csrfToken, moveBody)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("editor move 文档应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRBAC_EditorCannotArchive 验证 editor 不能 archive。
func TestRBAC_EditorCannotArchive(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-4", "dummyowner4", "user", true)
	createTestUser(t, authRepo, cfg, "editor-002", "editorarch", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "editorarchws", "dummy-owner-4")
	addMember(t, wsRepo, wsID, "editor-002", workspace.RoleEditor)

	doc := createDocDirect(t, docRepo, wsID, "test/toarchive.md", "Archive Test", "# Archive\n", "editor-002")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "editorarch")

	body := `{"expected_revision":` + intToStr(doc.RevisionNumber) + `,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/archive?path=test/toarchive.md", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("editor archive 应返回 403，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRBAC_AdminCanArchive 验证 admin 能 archive。
func TestRBAC_AdminCanArchive(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-5", "dummyowner5", "user", true)
	createTestUser(t, authRepo, cfg, "admin-001", "adminuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "adminws", "dummy-owner-5")
	addMember(t, wsRepo, wsID, "admin-001", workspace.RoleAdmin)

	docRepo.SetWorkspaceMaxSize(wsID, 2097152)
	doc := createDocDirect(t, docRepo, wsID, "test/adminarch.md", "Admin Archive", "# Admin Archive\n", "admin-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"expected_revision":` + intToStr(doc.RevisionNumber) + `,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/archive?path=test/adminarch.md", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("admin archive 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp documentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Status != StatusArchived {
		t.Errorf("status = %q, 期望 archived", resp.Status)
	}
}

// TestRBAC_AdminCanDeleteSource 验证 admin 能删除 source。
func TestRBAC_AdminCanDeleteSource(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-6", "dummyowner6", "user", true)
	createTestUser(t, authRepo, cfg, "admin-002", "adminsrc", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "adminsrcws", "dummy-owner-6")
	addMember(t, wsRepo, wsID, "admin-002", workspace.RoleAdmin)

	doc := createDocDirect(t, docRepo, wsID, "test/srcdoc.md", "Src Doc", "# Src\n", "admin-002")
	src, err := docRepo.AddSource(context.Background(), wsID, doc.ID, "web", "https://example.com", "Example", "", "", 0, "", "admin-002")
	if err != nil {
		t.Fatalf("AddSource 失败: %v", err)
	}

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminsrc")

	req := authedRequest(http.MethodDelete, "/api/workspaces/"+wsID+"/documents/sources/"+src.ID, sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("admin 删除 source 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRBAC_OwnerCanPurge 验证 owner 才可 purge。
func TestRBAC_OwnerCanPurge(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "owner-001", "owneruser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "ownerws", "owner-001")

	createDocDirect(t, docRepo, wsID, "test/purge.md", "Purge Test", "# Purge\n", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owneruser")

	// purge 通过 query 参数 path 定位文档
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/purge?path=test/purge.md", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("owner purge 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRBAC_AdminCannotPurge 验证 admin 不能 purge（仅 owner）。
func TestRBAC_AdminCannotPurge(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "owner-002", "ownerpurge", "user", true)
	createTestUser(t, authRepo, cfg, "admin-003", "adminpurge", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "purgews", "owner-002")
	addMember(t, wsRepo, wsID, "admin-003", workspace.RoleAdmin)

	createDocDirect(t, docRepo, wsID, "test/purge2.md", "Purge Test", "# Purge\n", "owner-002")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminpurge")

	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/purge?path=test/purge2.md", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("admin purge 应返回 403，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// --- 跨 Workspace 隔离测试 ---

// TestCrossWorkspace_NonMemberDenied 验证非成员不能访问 workspace 文档。
func TestCrossWorkspace_NonMemberDenied(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "owner-a", "owner_a", "user", true)
	createTestUser(t, authRepo, cfg, "user-b", "user_b", "user", true)

	wsA := createTestWorkspace(t, wsRepo, "ws-a", "owner-a")
	createDocDirect(t, docRepo, wsA, "test/secret.md", "Secret", "# Secret\n", "owner-a")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user_b")

	// user_b 不是 wsA 的成员，应返回 404（不泄露 workspace 存在性）
	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsA+"/documents/read?path=test/secret.md", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("非成员访问应返回 404，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestCrossWorkspace_DocIsolation 验证不同 workspace 的文档互不可见。
func TestCrossWorkspace_DocIsolation(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "owner-x", "owner_x", "user", true)

	ws1 := createTestWorkspace(t, wsRepo, "isows1", "owner-x")
	ws2 := createTestWorkspace(t, wsRepo, "isows2", "owner-x")

	createDocDirect(t, docRepo, ws1, "test/shared.md", "WS1 Doc", "# WS1\n", "owner-x")
	createDocDirect(t, docRepo, ws2, "test/shared.md", "WS2 Doc", "# WS2\n", "owner-x")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner_x")

	// 读取 ws1 的文档
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws1+"/documents/read?path=test/shared.md", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("读取 ws1 文档失败: %d", rr.Code)
	}
	var resp1 documentResponse
	json.Unmarshal(rr.Body.Bytes(), &resp1)
	if !strings.Contains(resp1.ContentMarkdown, "WS1") {
		t.Errorf("ws1 文档应包含 'WS1'，实际: %s", resp1.ContentMarkdown)
	}

	// 读取 ws2 的文档
	req = authedRequest(http.MethodGet, "/api/workspaces/"+ws2+"/documents/read?path=test/shared.md", sessionToken, csrfToken, "")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("读取 ws2 文档失败: %d", rr.Code)
	}
	var resp2 documentResponse
	json.Unmarshal(rr.Body.Bytes(), &resp2)
	if !strings.Contains(resp2.ContentMarkdown, "WS2") {
		t.Errorf("ws2 文档应包含 'WS2'，实际: %s", resp2.ContentMarkdown)
	}
}

// --- CSRF / 认证测试 ---

// TestCSRF_MissingCSRF 验证写操作缺少 CSRF token 返回 403。
func TestCSRF_MissingCSRF(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-7", "dummyowner7", "user", true)
	createTestUser(t, authRepo, cfg, "csrf-001", "csrfuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "csrfws", "dummy-owner-7")
	addMember(t, wsRepo, wsID, "csrf-001", workspace.RoleEditor)

	sessionToken, _ := loginAndGetCookies(t, mux, cfg, "csrfuser")

	// 有 session cookie 但无 CSRF header
	body := `{"path":"test/doc.md","title":"Test","content_markdown":"# Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: sessionToken})
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("缺少 CSRF 应返回 403，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestAuth_Unauthenticated 验证未认证请求返回 401。
func TestAuth_Unauthenticated(t *testing.T) {
	mux, authRepo, wsRepo, _, _ := setupTestEnv(t)
	createTestUser(t, authRepo, testAuthCfg(), "dummy-unauth", "dummyunauth", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "unauthws", "dummy-unauth")

	req := unauthedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents", "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("未认证应返回 401，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestAuth_InvalidJSON 验证非法 JSON 请求体被拒绝。
func TestAuth_InvalidJSON(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-8", "dummyowner8", "user", true)
	createTestUser(t, authRepo, cfg, "json-001", "jsonuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "jsonws", "dummy-owner-8")
	addMember(t, wsRepo, wsID, "json-001", workspace.RoleEditor)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "jsonuser")

	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents", sessionToken, csrfToken, "not json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestAuth_InvalidPagination 验证非法分页参数被拒绝。
func TestAuth_InvalidPagination(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-9", "dummyowner9", "user", true)
	createTestUser(t, authRepo, cfg, "page-001", "pageuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "pagews", "dummy-owner-9")
	addMember(t, wsRepo, wsID, "page-001", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "pageuser")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents?limit=abc", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法分页应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// --- ReplaceDocument 原子性测试 ---

// TestReplaceDocument_SingleRevision 验证 ReplaceDocument 一次成功只产生一个 revision。
func TestReplaceDocument_SingleRevision(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-10", "dummyowner10", "user", true)
	createTestUser(t, authRepo, cfg, "replace-001", "replaceuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "replacews", "dummy-owner-10")
	addMember(t, wsRepo, wsID, "replace-001", workspace.RoleEditor)
	docRepo.SetWorkspaceMaxSize(wsID, 2097152)

	doc := createDocDirect(t, docRepo, wsID, "test/replace.md", "Original", "# Original\n\nContent.\n", "replace-001")
	initialRevCount := docRepo.GetRevisionCount(wsID, doc.ID)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "replaceuser")

	// 执行 replace，同时改变 content 和 title
	replaceBody := `{"title":"Replaced Title","content_markdown":"# Replaced\n\nNew content.\n","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPut, "/api/workspaces/"+wsID+"/documents?path=test/replace.md", sessionToken, csrfToken, replaceBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("replace 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp documentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// 验证只新增了恰好 1 条 revision
	finalRevCount := docRepo.GetRevisionCount(wsID, doc.ID)
	if finalRevCount-initialRevCount != 1 {
		t.Errorf("replace 应只产生 1 条新 revision，实际新增 %d 条（初始 %d，最终 %d）",
			finalRevCount-initialRevCount, initialRevCount, finalRevCount)
	}

	// 验证 title 和 content 都已更新
	if resp.Title != "Replaced Title" {
		t.Errorf("title = %q, 期望 'Replaced Title'", resp.Title)
	}
	if !strings.Contains(resp.ContentMarkdown, "Replaced") {
		t.Errorf("content 应包含 'Replaced'，实际: %s", resp.ContentMarkdown)
	}
}

// TestReplaceDocument_ConcurrentConflict 验证并发 replace 第二次返回 409。
func TestReplaceDocument_ConcurrentConflict(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-11", "dummyowner11", "user", true)
	createTestUser(t, authRepo, cfg, "conc-001", "concuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "concws", "dummy-owner-11")
	addMember(t, wsRepo, wsID, "conc-001", workspace.RoleEditor)
	docRepo.SetWorkspaceMaxSize(wsID, 2097152)

	doc := createDocDirect(t, docRepo, wsID, "test/conc.md", "Concurrent", "# Concurrent\n\nOriginal.\n", "conc-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "concuser")

	// 第一次 replace — 应成功
	replaceBody1 := `{"title":"V1","content_markdown":"# V1\n\nFirst.\n","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req1 := authedRequest(http.MethodPut, "/api/workspaces/"+wsID+"/documents?path=test/conc.md", sessionToken, csrfToken, replaceBody1)
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("第一次 replace 应成功，实际 %d，body: %s", rr1.Code, rr1.Body.String())
	}

	// 第二次 replace — 使用相同的 expected_revision/hash — 应 409
	replaceBody2 := `{"title":"V2","content_markdown":"# V2\n\nSecond.\n","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req2 := authedRequest(http.MethodPut, "/api/workspaces/"+wsID+"/documents?path=test/conc.md", sessionToken, csrfToken, replaceBody2)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusConflict {
		t.Errorf("并发 replace 应返回 409，实际 %d，body: %s", rr2.Code, rr2.Body.String())
	}
}

// --- Patch 契约测试 ---

// TestPatch_ValidCandidateSuccess 验证正确的 candidate content + hash 成功更新。
// 引入动机：design/02-MCP.md §document_patch 要求 MCP 本地生成 candidate，
// server 验证 candidate_hash 完整性 + expected_revision/expected_hash 乐观并发。
func TestPatch_ValidCandidateSuccess(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-12", "dummyowner12", "user", true)
	createTestUser(t, authRepo, cfg, "patch-001", "patchuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "patchws", "dummy-owner-12")
	addMember(t, wsRepo, wsID, "patch-001", workspace.RoleEditor)
	docRepo.SetWorkspaceMaxSize(wsID, 2097152)

	content := "# Test\n\nHello world.\n"
	doc := createDocDirect(t, docRepo, wsID, "test/patch.md", "Patch Test", content, "patch-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "patchuser")

	// MCP 本地生成 candidate：替换 "Hello world." 为 "Hi universe."
	candidateContent := "# Test\n\nHi universe.\n"
	candidateHash := ComputeContentHash(candidateContent)
	patchBody := `{"content_markdown":"` + strings.ReplaceAll(candidateContent, "\n", "\\n") + `","candidate_hash":"` + candidateHash + `","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/documents?path=test/patch.md", sessionToken, csrfToken, patchBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("patch 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp documentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !strings.Contains(resp.ContentMarkdown, "Hi universe.") {
		t.Errorf("patch 后内容应包含 'Hi universe.'，实际: %s", resp.ContentMarkdown)
	}
	if resp.ContentHash != candidateHash {
		t.Errorf("patch 后 hash 应等于 candidate_hash，实际 %s，期望 %s", resp.ContentHash, candidateHash)
	}
}

// TestPatch_CandidateHashMismatch 验证 candidate_hash 与 content_markdown 不一致返回 400。
// 引入动机：server 必须验证 candidate_hash == SHA256(content_markdown)，防止传输损坏或篡改。
func TestPatch_CandidateHashMismatch(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-13", "dummyowner13", "user", true)
	createTestUser(t, authRepo, cfg, "patch-002", "patchhash", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "patchhashws", "dummy-owner-13")
	addMember(t, wsRepo, wsID, "patch-002", workspace.RoleEditor)
	docRepo.SetWorkspaceMaxSize(wsID, 2097152)

	content := "# Test\n\nHello world.\n"
	doc := createDocDirect(t, docRepo, wsID, "test/patchhash.md", "Patch Hash", content, "patch-002")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "patchhash")

	// candidate_hash 故意不匹配 content_markdown
	candidateContent := "# Test\n\nHi universe.\n"
	wrongHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	patchBody := `{"content_markdown":"` + strings.ReplaceAll(candidateContent, "\n", "\\n") + `","candidate_hash":"` + wrongHash + `","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/documents?path=test/patchhash.md", sessionToken, csrfToken, patchBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("candidate_hash 不匹配应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestPatch_RevisionConflict 验证 expected_revision/expected_hash 不匹配返回 409。
// 引入动机：design/04-WEB-API.md §Concurrency 要求不匹配返回 409，禁止 silent overwrite。
func TestPatch_RevisionConflict(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-14", "dummyowner14", "user", true)
	createTestUser(t, authRepo, cfg, "patch-003", "patchconf", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "patchconfws", "dummy-owner-14")
	addMember(t, wsRepo, wsID, "patch-003", workspace.RoleEditor)
	docRepo.SetWorkspaceMaxSize(wsID, 2097152)

	content := "# Test\n\nHello world.\n"
	doc := createDocDirect(t, docRepo, wsID, "test/patchconf.md", "Patch Conflict", content, "patch-003")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "patchconf")

	// 第一次 patch — 应成功
	candidateContent1 := "# Test\n\nHi universe.\n"
	candidateHash1 := ComputeContentHash(candidateContent1)
	patchBody1 := `{"content_markdown":"` + strings.ReplaceAll(candidateContent1, "\n", "\\n") + `","candidate_hash":"` + candidateHash1 + `","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req1 := authedRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/documents?path=test/patchconf.md", sessionToken, csrfToken, patchBody1)
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, req1)
	if rr1.Code != http.StatusOK {
		t.Fatalf("第一次 patch 应成功，实际 %d，body: %s", rr1.Code, rr1.Body.String())
	}

	// 第二次 patch — 使用旧的 expected_revision/hash — 应 409
	candidateContent2 := "# Test\n\nHello again.\n"
	candidateHash2 := ComputeContentHash(candidateContent2)
	patchBody2 := `{"content_markdown":"` + strings.ReplaceAll(candidateContent2, "\n", "\\n") + `","candidate_hash":"` + candidateHash2 + `","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req2 := authedRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/documents?path=test/patchconf.md", sessionToken, csrfToken, patchBody2)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)

	if rr2.Code != http.StatusConflict {
		t.Errorf("并发 patch 应返回 409，实际 %d，body: %s", rr2.Code, rr2.Body.String())
	}
}

// TestPatch_OldFieldsRejected 验证旧契约字段 old_text/new_text 被拒绝（unknown field 400）。
// 引入动机：server PATCH 契约已改为 content_markdown/candidate_hash/expected_revision/expected_hash，
// 旧字段 old_text/new_text 不再接受。decodeDocJSONStrict 使用 DisallowUnknownFields，
// 发送旧字段必须返回 400，不允许双契约兼容或静默回退。
func TestPatch_OldFieldsRejected(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-patch-old", "patcholdowner", "user", true)
	createTestUser(t, authRepo, cfg, "patch-old-001", "patcholduser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "patcholdws", "dummy-owner-patch-old")
	addMember(t, wsRepo, wsID, "patch-old-001", workspace.RoleEditor)
	docRepo.SetWorkspaceMaxSize(wsID, 2097152)

	content := "# Test\n\nHello world.\n"
	doc := createDocDirect(t, docRepo, wsID, "test/patchold.md", "Patch Old", content, "patch-old-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "patcholduser")

	// 发送旧契约字段 old_text/new_text — 应返回 400（unknown field）
	patchBody := `{"old_text":"Hello world.","new_text":"Hi universe.","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPatch, "/api/workspaces/"+wsID+"/documents?path=test/patchold.md", sessionToken, csrfToken, patchBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("旧字段 old_text/new_text 应返回 400（unknown field），实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// --- 特殊文件不可移动测试 ---

// TestMove_SpecialFileRejected 验证移动 PROJECT.md 被拒绝。
func TestMove_SpecialFileRejected(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-15", "dummyowner15", "user", true)
	createTestUser(t, authRepo, cfg, "move-001", "moveuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "movews", "dummy-owner-15")
	addMember(t, wsRepo, wsID, "move-001", workspace.RoleEditor)

	// 直接创建一个特殊文件
	content := "# Project\n"
	doc := createDocDirect(t, docRepo, wsID, SpecialFileProject, "Project", content, "move-001")
	// 手动标记为特殊文件
	docRepo.mu.Lock()
	d := docRepo.docs[docKey(wsID, SpecialFileProject)]
	d.IsSpecial = true
	docRepo.mu.Unlock()

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "moveuser")

	moveBody := `{"new_path":"test/moved-project.md","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/move?path=PROJECT.md", sessionToken, csrfToken, moveBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("移动 PROJECT.md 应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestMove_AGENTSRejected 验证移动 AGENTS.md 被拒绝。
func TestMove_AGENTSRejected(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-16", "dummyowner16", "user", true)
	createTestUser(t, authRepo, cfg, "move-002", "moveagents", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "moveagentsws", "dummy-owner-16")
	addMember(t, wsRepo, wsID, "move-002", workspace.RoleEditor)

	content := "# Agents\n"
	doc := createDocDirect(t, docRepo, wsID, SpecialFileAgents, "Agents", content, "move-002")
	docRepo.mu.Lock()
	d := docRepo.docs[docKey(wsID, SpecialFileAgents)]
	d.IsSpecial = true
	docRepo.mu.Unlock()

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "moveagents")

	moveBody := `{"new_path":"test/moved-agents.md","expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/move?path=AGENTS.md", sessionToken, csrfToken, moveBody)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("移动 AGENTS.md 应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// --- Archive/Restore 幂等测试 ---

// TestArchive_Idempotent 验证对已 archived 文档再次 archive 不新增 revision。
func TestArchive_Idempotent(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-17", "dummyowner17", "user", true)
	createTestUser(t, authRepo, cfg, "idem-001", "idemuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "idemws", "dummy-owner-17")
	addMember(t, wsRepo, wsID, "idem-001", workspace.RoleAdmin)

	doc := createDocDirect(t, docRepo, wsID, "test/idem.md", "Idempotent", "# Idem\n", "idem-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "idemuser")

	// 第一次 archive
	body := `{"expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/archive?path=test/idem.md", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("第一次 archive 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var archivedDoc documentResponse
	json.Unmarshal(rr.Body.Bytes(), &archivedDoc)
	revAfterFirstArchive := docRepo.GetRevisionCount(wsID, doc.ID)

	// 第二次 archive — 使用更新后的 revision/hash
	body2 := `{"expected_revision":` + intToStr(archivedDoc.RevisionNumber) + `,"expected_hash":"` + archivedDoc.ContentHash + `"}`
	req = authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/archive?path=test/idem.md", sessionToken, csrfToken, body2)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("第二次 archive 应返回 200（幂等），实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	revAfterSecondArchive := docRepo.GetRevisionCount(wsID, doc.ID)
	if revAfterSecondArchive != revAfterFirstArchive {
		t.Errorf("幂等 archive 不应新增 revision，第一次后 %d，第二次后 %d",
			revAfterFirstArchive, revAfterSecondArchive)
	}
}

// TestRestore_Idempotent 验证对已 active 文档再次 restore 不新增 revision。
func TestRestore_Idempotent(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-18", "dummyowner18", "user", true)
	createTestUser(t, authRepo, cfg, "idem-002", "idemrestore", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "idemrestorews", "dummy-owner-18")
	addMember(t, wsRepo, wsID, "idem-002", workspace.RoleAdmin)

	doc := createDocDirect(t, docRepo, wsID, "test/restore.md", "Restore Idem", "# Restore\n", "idem-002")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "idemrestore")

	// 文档已是 active，直接 restore — 应幂等，不新增 revision
	body := `{"expected_revision":1,"expected_hash":"` + doc.ContentHash + `"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+wsID+"/documents/restore?path=test/restore.md", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("对 active 文档 restore 应返回 200（幂等），实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 验证 revision 数量未增加（仍为初始的 1）
	revCount := docRepo.GetRevisionCount(wsID, doc.ID)
	if revCount != 1 {
		t.Errorf("幂等 restore 不应新增 revision，实际 revision 数量 %d，期望 1", revCount)
	}
}

// --- ListDocuments 不加载 content_markdown 测试 ---

// TestListDocuments_NoContentMarkdown 验证列表 API 不返回 content_markdown。
func TestListDocuments_NoContentMarkdown(t *testing.T) {
	mux, authRepo, wsRepo, docRepo, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-19", "dummyowner19", "user", true)
	createTestUser(t, authRepo, cfg, "list-001", "listuser", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "listnows", "dummy-owner-19")
	addMember(t, wsRepo, wsID, "list-001", workspace.RoleViewer)

	createDocDirect(t, docRepo, wsID, "test/list1.md", "List 1", "# List 1\n\nFull content here.\n", "dummy-owner-19")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "listuser")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("list 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp listDocumentsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if len(resp.Documents) == 0 {
		t.Fatal("文档列表不应为空")
	}

	for _, d := range resp.Documents {
		// documentListItemResponse 不含 content_markdown 字段
		if d.ID == "" {
			t.Error("文档 ID 不应为空")
		}
	}
}

// --- 分页query解析测试 ---
//
// 引入动机：远程发现后端query解析bug——代理层损坏query string导致后端收到
// offset="0?offset=0" 或 limit="20?offset=0"。以下测试直接调用handler
// 并构造 httptest.NewRequest 验证各种query string场景。

// TestListDocuments_QueryOffsetLimit 正确解析 offset=0&limit=20。
func TestListDocuments_QueryOffsetLimit(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-q1", "qowner1", "user", true)
	createTestUser(t, authRepo, cfg, "quser1", "quser1", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "qws1", "dummy-owner-q1")
	addMember(t, wsRepo, wsID, "quser1", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "quser1")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents?offset=0&limit=20", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("offset=0&limit=20 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp listDocumentsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 20 {
		t.Errorf("limit = %d, 期望 20", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset = %d, 期望 0", resp.Offset)
	}
}

// TestListDocuments_QueryReversedOrder 正确解析反序参数 limit=20&offset=0。
func TestListDocuments_QueryReversedOrder(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-q2", "qowner2", "user", true)
	createTestUser(t, authRepo, cfg, "quser2", "quser2", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "qws2", "dummy-owner-q2")
	addMember(t, wsRepo, wsID, "quser2", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "quser2")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents?limit=20&offset=0", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("反序参数应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp listDocumentsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 20 {
		t.Errorf("limit = %d, 期望 20", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset = %d, 期望 0", resp.Offset)
	}
}

// TestListDocuments_QuerySingleParam_LimitOnly 正确解析单参数 limit。
func TestListDocuments_QuerySingleParam_LimitOnly(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-q3", "qowner3", "user", true)
	createTestUser(t, authRepo, cfg, "quser3", "quser3", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "qws3", "dummy-owner-q3")
	addMember(t, wsRepo, wsID, "quser3", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "quser3")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents?limit=10", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("单参数 limit 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp listDocumentsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 10 {
		t.Errorf("limit = %d, 期望 10", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("offset = %d, 期望 0 (默认值)", resp.Offset)
	}
}

// TestListDocuments_QueryNoParams 无query参数时使用默认值。
func TestListDocuments_QueryNoParams(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-q4", "qowner4", "user", true)
	createTestUser(t, authRepo, cfg, "quser4", "quser4", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "qws4", "dummy-owner-q4")
	addMember(t, wsRepo, wsID, "quser4", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "quser4")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("无query参数应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp listDocumentsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 20 {
		t.Errorf("默认 limit = %d, 期望 20", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("默认 offset = %d, 期望 0", resp.Offset)
	}
}

// TestListDocuments_QueryInvalidLimit 非法limit返回400。
func TestListDocuments_QueryInvalidLimit(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-q5", "qowner5", "user", true)
	createTestUser(t, authRepo, cfg, "quser5", "quser5", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "qws5", "dummy-owner-q5")
	addMember(t, wsRepo, wsID, "quser5", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "quser5")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents?limit=abc", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法 limit 应返回 400，实际 %d", rr.Code)
	}
}

// TestListDocuments_QueryInvalidOffset 非法offset返回400。
func TestListDocuments_QueryInvalidOffset(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-q6", "qowner6", "user", true)
	createTestUser(t, authRepo, cfg, "quser6", "quser6", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "qws6", "dummy-owner-q6")
	addMember(t, wsRepo, wsID, "quser6", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "quser6")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents?offset=xyz", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法 offset 应返回 400，实际 %d", rr.Code)
	}
}

// TestListDocuments_PathParamNoQuery 路径参数不含query时正确处理。
func TestListDocuments_PathParamNoQuery(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUser(t, authRepo, cfg, "dummy-owner-q7", "qowner7", "user", true)
	createTestUser(t, authRepo, cfg, "quser7", "quser7", "user", true)
	wsID := createTestWorkspace(t, wsRepo, "qws7", "dummy-owner-q7")
	addMember(t, wsRepo, wsID, "quser7", workspace.RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "quser7")

	// 路径中包含 workspace ID（UUID格式），不含query string
	req := authedRequest(http.MethodGet, "/api/workspaces/"+wsID+"/documents", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("路径参数不含query应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp listDocumentsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 20 {
		t.Errorf("默认 limit = %d, 期望 20", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("默认 offset = %d, 期望 0", resp.Offset)
	}
}

