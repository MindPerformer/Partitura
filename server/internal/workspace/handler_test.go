// handler_test.go 测试 workspace HTTP handler 经真实 mux + auth middleware + workspace routes 的端到端行为。
//
// 测试覆盖：
//   - Workspace 创建：缺少 workspace:create 拒绝；成功时 workspace 和 owner membership 原子创建
//   - 成员管理：admin 能管理，editor/viewer 不能；非法/重复成员拒绝
//   - 禁止移除或降级最后 owner，保证 owner invariant
//   - 跨 workspace 访问拒绝：非成员不能读取、更新、列成员、管理另一 workspace 的成员
//   - RBAC 矩阵：viewer/editor/admin/owner 对各操作的不同权限
//   - 系统 admin API：普通用户全部拒绝，system_admin 可分页 list/update
//   - audit：关键变更产生审计记录；普通用户不能读取 audit
//   - 请求体安全：畸形 JSON、未知字段、多 JSON 值拒绝
//   - 分页参数验证
package workspace

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

// setupTestEnv 创建完整的测试环境：auth mock repo + workspace mock repo + audit mock repo + mux。
// 返回 mux（已包装全局 RequestID + Logging middleware 的 http.Handler）、authRepo、wsRepo、auditRepo、cfg。
// 引入动机：测试需要验证全局 middleware 与认证/权限链的端到端行为，
// 因此 handler 必须与 main.go 一样被全局 middleware 包装。
func setupTestEnv(t *testing.T) (
	mux http.Handler,
	authRepo *LocalAuthMockRepository,
	wsRepo *MockRepository,
	auditRepo *MockAuditRepository,
	cfg auth.AuthConfig,
) {
	t.Helper()
	cfg = testAuthCfg()
	authRepo = NewLocalAuthMockRepository()
	wsRepo = NewMockRepository()
	auditRepo = NewMockAuditRepository()

	wsHandler := NewHandler(wsRepo, auditRepo)
	adminHandler := NewAdminHandlerWithAuth(wsRepo, auditRepo, authRepo, cfg)

	rawMux := http.NewServeMux()
	auth.RegisterRoutes(rawMux, auth.NewHandler(authRepo, cfg), authRepo, cfg)
	RegisterRoutes(rawMux, wsHandler, adminHandler, authRepo, cfg)

	// 与 main.go 保持一致：RequestID → Logging → mux
	mux = httpmw.RequestIDMiddleware(httpmw.LoggingMiddleware(rawMux))
	return
}

// createTestUserInAuth 创建用户在 auth mock repo 中，返回 userID。
func createTestUserInAuth(t *testing.T, authRepo *LocalAuthMockRepository, cfg auth.AuthConfig, userID, username, systemRole string, wsCreatePerm bool) string {
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
func authedRequest(method, path, sessionToken, csrfToken string, body string) *http.Request {
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
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

// --- Workspace 创建测试 ---

// TestCreateWorkspace_NoPermission 验证缺少 workspace:create 权限的普通用户不能创建 workspace。
func TestCreateWorkspace_NoPermission(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "regularuser")

	body := `{"name":"test-ws","display_name":"Test WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("缺少 workspace:create 应返回 403，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateWorkspace_WithPermission 验证有 workspace:create 权限的用户可以创建 workspace。
func TestCreateWorkspace_WithPermission(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	body := `{"name":"test-ws","display_name":"Test WS","description":"A test workspace"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 应返回 201，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp workspaceResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Name != "test-ws" {
		t.Errorf("name = %q, 期望 test-ws", resp.Name)
	}
	if resp.Status != StatusActive {
		t.Errorf("status = %q, 期望 active", resp.Status)
	}

	// 验证 owner membership 原子创建
	role, err := wsRepo.GetMemberRole(context.Background(), resp.ID, "user-001")
	if err != nil {
		t.Fatalf("查询创建者角色失败: %v", err)
	}
	if role != RoleOwner {
		t.Errorf("创建者角色 = %q, 期望 owner", role)
	}
}

// TestCreateWorkspace_SystemAdmin 验证 system_admin 天然可以创建 workspace。
func TestCreateWorkspace_SystemAdmin(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	// system_admin 不需要显式 workspace_create_perm
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "sysadmin", "system_admin", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "sysadmin")

	body := `{"name":"admin-ws","display_name":"Admin WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("system_admin 创建 workspace 应返回 201，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestCreateWorkspace_DuplicateName 验证重复名称返回 409。
func TestCreateWorkspace_DuplicateName(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	// 第一次创建
	body := `{"name":"dup-ws","display_name":"First"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("第一次创建应成功: %d", rr.Code)
	}

	// 第二次创建同名
	req2 := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusConflict {
		t.Errorf("重复名称应返回 409，实际 %d", rr2.Code)
	}
}

// TestCreateWorkspace_MalformedJSON 验证畸形 JSON 返回 400。
func TestCreateWorkspace_MalformedJSON(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, "not json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("畸形 JSON 应返回 400，实际 %d", rr.Code)
	}
}

// TestCreateWorkspace_UnknownField 验证未知字段返回 400。
func TestCreateWorkspace_UnknownField(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	body := `{"name":"test","display_name":"Test","extra":"malicious"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("未知字段应返回 400，实际 %d", rr.Code)
	}
}

// --- Workspace 列表和详情测试 ---

// TestListWorkspaces_OnlyMember 验证仅返回用户作为成员的 workspace。
func TestListWorkspaces_OnlyMember(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "user2", "user", true)

	// user1 创建 ws1
	ws1, _ := wsRepo.CreateWorkspace(context.Background(), "ws1", "WS1", "", "user-001")
	// user2 创建 ws2
	ws2, _ := wsRepo.CreateWorkspace(context.Background(), "ws2", "WS2", "", "user-002")

	_ = ws1
	_ = ws2

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user1")

	req := authedRequest(http.MethodGet, "/api/workspaces", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("list workspaces 应返回 200，实际 %d", rr.Code)
	}

	var resp listWorkspacesResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Total != 1 {
		t.Errorf("user1 应只看到 1 个 workspace，实际 %d", resp.Total)
	}
	if len(resp.Workspaces) != 1 || resp.Workspaces[0].Name != "ws1" {
		t.Errorf("user1 应只看到 ws1，实际: %+v", resp.Workspaces)
	}
}

// TestGetWorkspace_NonMemberDenied 验证非成员不能通过猜测 UUID 获取 workspace。
func TestGetWorkspace_NonMemberDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "user2", "user", true)

	// user1 创建 ws
	ws, _ := wsRepo.CreateWorkspace(context.Background(), "private-ws", "Private", "", "user-001")

	// user2 尝试访问
	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user2")
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	// 非成员应返回 404（不泄露 workspace 存在性）
	if rr.Code != http.StatusNotFound {
		t.Errorf("非成员访问应返回 404，实际 %d", rr.Code)
	}
}

// TestGetWorkspace_MemberSuccess 验证成员可以获取 workspace 详情。
func TestGetWorkspace_MemberSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "my-ws", "My WS", "desc", "user-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user1")
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("成员访问应返回 200，实际 %d", rr.Code)
	}

	var resp workspaceResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.ID != ws.ID {
		t.Errorf("ID = %q, 期望 %q", resp.ID, ws.ID)
	}
}

// --- 成员管理测试 ---

// TestAddMember_AdminSuccess 验证 workspace admin 可以添加成员。
func TestAddMember_AdminSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "newmember", "user", false)
	createTestUserInAuth(t, authRepo, cfg, "user-003", "newmember2", "user", false)

	// 同步用户到 workspace mock repo
	wsRepo.AddUser("owner-001", "owner1", "owner1@test.example", "user", true)
	wsRepo.AddUser("user-002", "newmember", "newmember@test.example", "user", false)
	wsRepo.AddUser("user-003", "newmember2", "newmember2@test.example", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	// 将 owner-001 降级为 admin 以测试 admin 添加成员
	wsRepo.UpdateMemberRole(context.Background(), ws.ID, "owner-001", RoleAdmin)
	// 需要再添加一个 owner 以保持不变量
	wsRepo.AddMember(context.Background(), ws.ID, "user-002", RoleOwner)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	body := `{"username":"newmember2","role":"viewer"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("admin 添加成员应返回 201，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestAddMember_EditorDenied 验证 editor 不能添加成员。
// TestListMemberCandidates_AdminGetsUnjoinedUsers 验证成员候选按用户名搜索且排除已加入成员。
func TestListMemberCandidates_AdminGetsUnjoinedUsers(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "existingmember", "user", false)
	createTestUserInAuth(t, authRepo, cfg, "user-003", "newmember", "user", false)

	wsRepo.AddUser("owner-001", "owner1", "owner1@test.example", "user", true)
	wsRepo.AddUser("user-002", "existingmember", "existing@test.example", "user", false)
	wsRepo.AddUser("user-003", "newmember", "new@test.example", "user", false)
	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "user-002", RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID+"/members/candidates?q=newmember", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("查询成员候选应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Users []memberResponse `json:"users"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析成员候选响应失败: %v", err)
	}
	if len(resp.Users) != 1 {
		t.Fatalf("应返回 1 个未加入候选，实际 %d: %+v", len(resp.Users), resp.Users)
	}
	if resp.Users[0].Username != "newmember" || resp.Users[0].UserID != "user-003" {
		t.Errorf("候选用户不匹配: %+v", resp.Users[0])
	}
}

// TestAddMember_EditorDenied 验证 editor 不能添加成员。
func TestAddMember_EditorDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "editor-001", "editor1", "user", false)
	createTestUserInAuth(t, authRepo, cfg, "user-003", "newmember", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "editor-001", RoleEditor)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "editor1")

	body := `{"username":"newmember","role":"viewer"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("editor 添加成员应返回 403，实际 %d", rr.Code)
	}
}

// TestAddMember_ViewerDenied 验证 viewer 不能添加成员。
func TestAddMember_ViewerDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "viewer-001", "viewer1", "user", false)
	createTestUserInAuth(t, authRepo, cfg, "user-003", "newmember", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "viewer-001", RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "viewer1")

	body := `{"username":"newmember","role":"viewer"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("viewer 添加成员应返回 403，实际 %d", rr.Code)
	}
}

// TestAddMember_Duplicate 验证重复添加成员返回 409。
func TestAddMember_Duplicate(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "existing", "user", false)

	// 同步用户到 workspace mock repo
	wsRepo.AddUser("owner-001", "owner1", "owner1@test.example", "user", true)
	wsRepo.AddUser("user-002", "existing", "existing@test.example", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "user-002", RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	body := `{"username":"existing","role":"editor"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("重复添加成员应返回 409，实际 %d", rr.Code)
	}
}

// TestAddMember_InvalidRole 验证非法角色返回 400。
func TestAddMember_InvalidRole(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "newmember", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	body := `{"username":"newmember","role":"superadmin"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法角色应返回 400，实际 %d", rr.Code)
	}
}

// TestAddMember_OwnerRoleRejected 验证不能直接添加 owner 角色。
func TestAddMember_OwnerRoleRejected(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "newmember", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	body := `{"username":"newmember","role":"owner"}`
	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("直接添加 owner 角色应返回 400，实际 %d", rr.Code)
	}
}

// --- Owner 不变量测试 ---

// TestRemoveMember_LastOwnerDenied 验证不能移除最后一名 owner。
func TestRemoveMember_LastOwnerDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "solo-ws", "Solo", "", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	req := authedRequest(http.MethodDelete, "/api/workspaces/"+ws.ID+"/members/owner-001", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("移除最后一名 owner 应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestUpdateMemberRole_DowngradeLastOwnerDenied 验证不能降级最后一名 owner。
func TestUpdateMemberRole_DowngradeLastOwnerDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "solo-ws", "Solo", "", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	body := `{"role":"admin"}`
	req := authedRequest(http.MethodPut, "/api/workspaces/"+ws.ID+"/members/owner-001", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("降级最后一名 owner 应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestUpdateMemberRole_OwnerRoleRejected 验证不能通过修改角色设置 owner。
func TestUpdateMemberRole_OwnerRoleRejected(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "member1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "user-002", RoleAdmin)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	body := `{"role":"owner"}`
	req := authedRequest(http.MethodPut, "/api/workspaces/"+ws.ID+"/members/user-002", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("通过修改角色设置 owner 应返回 400，实际 %d", rr.Code)
	}
}

// TestRemoveMember_NonLastOwnerSuccess 验证可以移除非最后一名 owner。
func TestRemoveMember_NonLastOwnerSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "owner-002", "owner2", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "owner-002", RoleOwner)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	req := authedRequest(http.MethodDelete, "/api/workspaces/"+ws.ID+"/members/owner-002", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("移除非最后 owner 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// --- 跨 workspace 隔离测试 ---

// TestCrossWorkspace_Isolation 验证用户不能访问另一 workspace 的资源。
func TestCrossWorkspace_Isolation(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "user2", "user", true)

	// user1 创建 ws1, user2 创建 ws2
	ws1, _ := wsRepo.CreateWorkspace(context.Background(), "ws1", "WS1", "", "user-001")
	ws2, _ := wsRepo.CreateWorkspace(context.Background(), "ws2", "WS2", "", "user-002")

	// user1 尝试读取 ws2（非成员）
	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user1")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws2.ID, sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("跨 workspace 读取应返回 404，实际 %d", rr.Code)
	}

	// user1 尝试更新 ws2
	updateBody := `{"display_name":"Hacked","description":""}`
	req2 := authedRequest(http.MethodPut, "/api/workspaces/"+ws2.ID, sessionToken, csrfToken, updateBody)
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Errorf("跨 workspace 更新应返回 404，实际 %d", rr2.Code)
	}

	// user1 尝试列出 ws2 成员
	req3 := authedRequest(http.MethodGet, "/api/workspaces/"+ws2.ID+"/members", sessionToken, csrfToken, "")
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req3)
	if rr3.Code != http.StatusNotFound {
		t.Errorf("跨 workspace 列成员应返回 404，实际 %d", rr3.Code)
	}

	// user1 尝试向 ws2 添加成员
	addBody := `{"username":"member1","role":"viewer"}`
	req4 := authedRequest(http.MethodPost, "/api/workspaces/"+ws2.ID+"/members", sessionToken, csrfToken, addBody)
	rr4 := httptest.NewRecorder()
	mux.ServeHTTP(rr4, req4)
	if rr4.Code != http.StatusNotFound {
		t.Errorf("跨 workspace 添加成员应返回 404，实际 %d", rr4.Code)
	}

	// 确认 ws1 和 ws2 仍然独立
	ws1Check, _ := wsRepo.GetWorkspaceByID(context.Background(), ws1.ID)
	ws2Check, _ := wsRepo.GetWorkspaceByID(context.Background(), ws2.ID)
	if ws1Check.DisplayName != "WS1" {
		t.Errorf("ws1 display_name 被篡改: %q", ws1Check.DisplayName)
	}
	if ws2Check.DisplayName != "WS2" {
		t.Errorf("ws2 display_name 被篡改: %q", ws2Check.DisplayName)
	}
}

// --- Admin API 测试 ---

// TestAdminAPI_RegularUserDenied 验证普通用户不能访问 admin API。
func TestAdminAPI_RegularUserDenied(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "regularuser")

	// GET /api/admin/users
	req := authedRequest(http.MethodGet, "/api/admin/users", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("普通用户访问 admin users 应返回 403，实际 %d", rr.Code)
	}

	// GET /api/admin/audit
	req2 := authedRequest(http.MethodGet, "/api/admin/audit", sessionToken, csrfToken, "")
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Errorf("普通用户访问 admin audit 应返回 403，实际 %d", rr2.Code)
	}
}

// TestAdminAPI_SystemAdminSuccess 验证 system_admin 可以访问 admin API。
func TestAdminAPI_SystemAdminSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "sysadmin", "system_admin", false)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)

	// 添加用户到 wsRepo mock
	wsRepo.AddUser("user-001", "regularuser", "regularuser@test.example", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "sysadmin")

	req := authedRequest(http.MethodGet, "/api/admin/users", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("system_admin 访问 admin users 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp adminListUsersResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Total == 0 {
		t.Error("应返回至少一个用户")
	}
}

// TestAdminAPI_UpdateUserPermission 验证 system_admin 可以更新用户权限。
func TestAdminAPI_UpdateUserPermission(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "sysadmin", "system_admin", false)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)
	wsRepo.AddUser("user-001", "regularuser", "regularuser@test.example", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "sysadmin")

	body := `{"workspace_create_perm":true}`
	req := authedRequest(http.MethodPut, "/api/admin/users/user-001", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("更新用户权限应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp adminUserResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if !resp.WorkspaceCreatePerm {
		t.Error("workspace_create_perm 应为 true")
	}
}

// TestAdminAPI_UpdateUserInvalidRole 验证非法 system_role 返回 400。
func TestAdminAPI_UpdateUserInvalidRole(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "sysadmin", "system_admin", false)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)
	wsRepo.AddUser("user-001", "regularuser", "regularuser@test.example", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "sysadmin")

	body := `{"system_role":"superadmin"}`
	req := authedRequest(http.MethodPut, "/api/admin/users/user-001", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法 system_role 应返回 400，实际 %d", rr.Code)
	}
}

// TestAdminAPI_UpdateUserAudit 验证管理员更新用户权限后产生审计记录，
// 且审计记录包含正确的 actor、resource、action、request_id，不含敏感信息。
func TestAdminAPI_UpdateUserAudit(t *testing.T) {
	mux, authRepo, wsRepo, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "sysadmin", "system_admin", false)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)
	wsRepo.AddUser("user-001", "regularuser", "regularuser@test.example", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "sysadmin")

	body := `{"system_role":"system_admin","workspace_create_perm":true}`
	req := authedRequest(http.MethodPut, "/api/admin/users/user-001", sessionToken, csrfToken, body)
	req.Header.Set("X-Request-ID", "test-req-id-audit-001")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("更新用户权限应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	entries := auditRepo.GetEntries()
	var found *mockAuditEntry
	for i := range entries {
		if entries[i].action == "admin.user.update" {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatal("应产生 admin.user.update 审计记录")
	}

	// 验证 actor 是管理员
	if found.userID != "admin-001" {
		t.Errorf("audit actor user_id = %q, 期望 admin-001", found.userID)
	}

	// 验证 resource_type 和 resource_id
	if found.resourceType != "user" {
		t.Errorf("audit resource_type = %q, 期望 user", found.resourceType)
	}
	if found.resourceID != "user-001" {
		t.Errorf("audit resource_id = %q, 期望 user-001", found.resourceID)
	}

	// 验证 request_id
	if found.requestID != "test-req-id-audit-001" {
		t.Errorf("audit request_id = %q, 期望 test-req-id-audit-001", found.requestID)
	}

	// 验证 detail 包含变更后的角色和权限
	detailStr := string(found.detail)
	if !strings.Contains(detailStr, "system_admin") {
		t.Errorf("audit detail 应包含 system_role=system_admin, detail: %s", detailStr)
	}
	if !strings.Contains(detailStr, "workspace_create_perm") {
		t.Errorf("audit detail 应包含 workspace_create_perm, detail: %s", detailStr)
	}
	if !strings.Contains(detailStr, "user-001") {
		t.Errorf("audit detail 应包含 target_user_id=user-001, detail: %s", detailStr)
	}

	// 验证 detail 不含敏感信息
	if strings.Contains(detailStr, "password") {
		t.Error("审计记录不应包含 password")
	}
	if strings.Contains(detailStr, sessionToken) {
		t.Error("审计记录不应包含 session token")
	}
	if strings.Contains(detailStr, csrfToken) {
		t.Error("审计记录不应包含 CSRF token")
	}
	if strings.Contains(detailStr, "token") {
		t.Errorf("审计记录不应包含 token 相关信息, detail: %s", detailStr)
	}
}

// TestAdminAPI_ListUsersSingleQuery 验证 ListUsers 使用单次 repository 查询，
// 而非逐用户 N+1 查询系统信息。通过 mock 调用计数器验证。
func TestAdminAPI_ListUsersSingleQuery(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "sysadmin", "system_admin", false)

	// 添加多个用户到 mock repo
	for i := 2; i <= 10; i++ {
		userID := fmt.Sprintf("user-%03d", i)
		username := fmt.Sprintf("user%d", i)
		createTestUserInAuth(t, authRepo, cfg, userID, username, "user", false)
		wsRepo.AddUser(userID, username, username+"@test.example", "user", false)
	}

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "sysadmin")

	req := authedRequest(http.MethodGet, "/api/admin/users", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("system_admin 访问 admin users 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 验证 ListAllUsersWithSystemInfo 仅被调用一次（单条 SQL 查询）
	callCount := wsRepo.GetListAllUsersWithSystemInfoCallCount()
	if callCount != 1 {
		t.Errorf("ListAllUsersWithSystemInfo 应被调用 1 次，实际 %d 次（N+1 问题）", callCount)
	}

	// 验证响应格式和分页
	var resp adminListUsersResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Total == 0 {
		t.Error("应返回至少一个用户")
	}
	if resp.Limit != 20 {
		t.Errorf("默认 limit 应为 20，实际 %d", resp.Limit)
	}
	if resp.Offset != 0 {
		t.Errorf("默认 offset 应为 0，实际 %d", resp.Offset)
	}
	// 验证每个用户都有系统信息字段
	for _, u := range resp.Users {
		if u.ID == "" {
			t.Error("用户 ID 不应为空")
		}
		if u.SystemRole == "" {
			t.Errorf("用户 %s 的 system_role 不应为空", u.ID)
		}
	}
}

// --- Audit 测试 ---

// TestAudit_LogOnCreate 验证创建 workspace 产生审计记录。
func TestAudit_LogOnCreate(t *testing.T) {
	mux, authRepo, _, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	body := `{"name":"audit-ws","display_name":"Audit WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建失败: %d", rr.Code)
	}

	entries := auditRepo.GetEntries()
	found := false
	for _, e := range entries {
		if e.action == "workspace.create" {
			found = true
			if e.userID != "user-001" {
				t.Errorf("audit user_id = %q, 期望 user-001", e.userID)
			}
		}
	}
	if !found {
		t.Error("应产生 workspace.create 审计记录")
	}
}

// TestAudit_LogOnPermissionDenied 验证权限拒绝也产生审计记录。
func TestAudit_LogOnPermissionDenied(t *testing.T) {
	mux, authRepo, _, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "regularuser")

	body := `{"name":"denied-ws","display_name":"Denied"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	entries := auditRepo.GetEntries()
	found := false
	for _, e := range entries {
		if e.action == "workspace.create.denied" {
			found = true
		}
	}
	if !found {
		t.Error("权限拒绝应产生 workspace.create.denied 审计记录")
	}
}

// TestAudit_NoSensitiveData 验证审计记录不含 password/token。
func TestAudit_NoSensitiveData(t *testing.T) {
	mux, authRepo, _, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	body := `{"name":"safe-ws","display_name":"Safe WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	entries := auditRepo.GetEntries()
	for _, e := range entries {
		detailStr := string(e.detail)
		if strings.Contains(detailStr, sessionToken) {
			t.Error("审计记录不应包含 session token")
		}
		if strings.Contains(detailStr, csrfToken) {
			t.Error("审计记录不应包含 CSRF token")
		}
		if strings.Contains(detailStr, "password") {
			t.Error("审计记录不应包含 password")
		}
	}
}

// TestAudit_RegularUserCannotRead 验证普通用户不能读取审计日志。
func TestAudit_RegularUserCannotRead(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "regularuser")

	req := authedRequest(http.MethodGet, "/api/admin/audit", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("普通用户读取 audit 应返回 403，实际 %d", rr.Code)
	}
}

// --- 分页测试 ---

// TestPagination_InvalidLimit 验证非法 limit 参数返回 400。
func TestPagination_InvalidLimit(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user1")

	req := authedRequest(http.MethodGet, "/api/workspaces?limit=abc", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法 limit 应返回 400，实际 %d", rr.Code)
	}
}

// TestPagination_NegativeOffset 验证负 offset 返回 400。
func TestPagination_NegativeOffset(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user1")

	req := authedRequest(http.MethodGet, "/api/workspaces?offset=-5", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("负 offset 应返回 400，实际 %d", rr.Code)
	}
}

// TestPagination_LimitTooLarge 验证 limit 超过 100 返回 400。
func TestPagination_LimitTooLarge(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "user1")

	req := authedRequest(http.MethodGet, "/api/workspaces?limit=200", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("limit 超过 100 应返回 400，实际 %d", rr.Code)
	}
}

// --- 未认证测试 ---

// TestWorkspace_Unauthenticated 验证未认证请求返回 401。
func TestWorkspace_Unauthenticated(t *testing.T) {
	mux, _, _, _, _ := setupTestEnv(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/workspaces"},
		{http.MethodPost, "/api/workspaces"},
		{http.MethodGet, "/api/workspaces/some-id"},
		{http.MethodGet, "/api/admin/users"},
		{http.MethodGet, "/api/admin/audit"},
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(ep.method, ep.path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未认证应返回 401，实际 %d", ep.method, ep.path, rr.Code)
		}
	}
}

// --- CSRF 测试 ---

// TestWorkspace_CSRFMissing 验证 cookie session 认证的状态变更请求缺少 CSRF 返回 403。
func TestWorkspace_CSRFMissing(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, _ := loginAndGetCookies(t, mux, cfg, "wscreator")

	// POST 不带 CSRF header
	body := `{"name":"test","display_name":"Test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: sessionToken})
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("缺少 CSRF 应返回 403，实际 %d", rr.Code)
	}
}

// --- Update workspace 测试 ---

// TestUpdateWorkspace_AdminSuccess 验证 admin 可以更新 workspace。
func TestUpdateWorkspace_AdminSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "admin1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "admin-001", RoleAdmin)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "admin1")

	body := `{"display_name":"Updated Name","description":"Updated desc"}`
	req := authedRequest(http.MethodPut, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin 更新 workspace 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp workspaceResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.DisplayName != "Updated Name" {
		t.Errorf("display_name = %q, 期望 Updated Name", resp.DisplayName)
	}
}

// TestUpdateWorkspace_ViewerDenied 验证 viewer 不能更新 workspace。
func TestUpdateWorkspace_ViewerDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "viewer-001", "viewer1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "viewer-001", RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "viewer1")

	body := `{"display_name":"Hacked","description":""}`
	req := authedRequest(http.MethodPut, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("viewer 更新 workspace 应返回 403，实际 %d", rr.Code)
	}
}

// --- Archive workspace 测试 ---

// TestArchiveWorkspace_AdminSuccess 验证 admin 可以归档 workspace。
func TestArchiveWorkspace_AdminSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "admin1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "admin-001", RoleAdmin)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "admin1")

	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/archive", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin 归档 workspace 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp workspaceResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Status != StatusArchived {
		t.Errorf("status = %q, 期望 archived", resp.Status)
	}
}

// TestArchiveWorkspace_EditorDenied 验证 editor 不能归档 workspace。
func TestArchiveWorkspace_EditorDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "editor-001", "editor1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "editor-001", RoleEditor)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "editor1")

	req := authedRequest(http.MethodPost, "/api/workspaces/"+ws.ID+"/archive", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("editor 归档 workspace 应返回 403，实际 %d", rr.Code)
	}
}

// --- List Members 测试 ---

// TestListMembers_MemberSuccess 验证成员可以列出 workspace 成员。
func TestListMembers_MemberSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "viewer-001", "viewer1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "viewer-001", RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "viewer1")

	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID+"/members", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("成员列成员应返回 200，实际 %d", rr.Code)
	}

	var resp listMembersResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Total < 2 {
		t.Errorf("应至少有 2 个成员，实际 %d", resp.Total)
	}
}

// --- Bearer token 测试 ---

// TestWorkspace_BearerTokenAuth 验证 bearer token 认证可以访问 workspace API。
func TestWorkspace_BearerTokenAuth(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	// 创建 device session 获取 access token
	deviceResult, err := auth.AuthorizeDevice(context.Background(), authRepo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 用 bearer token 创建 workspace（不需要 CSRF）
	body := `{"name":"bearer-ws","display_name":"Bearer WS"}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+deviceResult.AccessToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Errorf("bearer token 创建 workspace 应返回 201，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 验证 workspace 创建成功
	var resp workspaceResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	role, err := wsRepo.GetMemberRole(context.Background(), resp.ID, "user-001")
	if err != nil {
		t.Fatalf("查询创建者角色失败: %v", err)
	}
	if role != RoleOwner {
		t.Errorf("创建者角色 = %q, 期望 owner", role)
	}
}

// --- 多 JSON 值测试 ---

// TestCreateWorkspace_MultipleJSONValues 验证多个 JSON 值返回 400。
func TestCreateWorkspace_MultipleJSONValues(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	body := `{"name":"test","display_name":"Test"}{"extra":"second"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("多个 JSON 值应返回 400，实际 %d", rr.Code)
	}
}

// --- Admin Workspaces List 测试 ---

// TestAdminListWorkspaces_SystemAdmin 验证 system_admin 可以列出全部 workspace。
func TestAdminListWorkspaces_SystemAdmin(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "sysadmin", "system_admin", false)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "user1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "user2", "user", true)

	wsRepo.CreateWorkspace(context.Background(), "ws1", "WS1", "", "user-001")
	wsRepo.CreateWorkspace(context.Background(), "ws2", "WS2", "", "user-002")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "sysadmin")

	req := authedRequest(http.MethodGet, "/api/admin/workspaces", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("system_admin 列出全部 workspace 应返回 200，实际 %d", rr.Code)
	}

	var resp adminListWorkspacesResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Total < 2 {
		t.Errorf("应至少有 2 个 workspace，实际 %d", resp.Total)
	}
}

// TestAdminListWorkspaces_RegularUserDenied 验证普通用户不能访问 admin workspaces。
func TestAdminListWorkspaces_RegularUserDenied(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "regularuser")

	req := authedRequest(http.MethodGet, "/api/admin/workspaces", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("普通用户访问 admin workspaces 应返回 403，实际 %d", rr.Code)
	}
}

// --- Request ID 与审计 correlation 测试 ---

// TestRequestID_MissingGeneratesAndReturns 验证缺失 X-Request-ID 时服务器生成并回写。
func TestRequestID_MissingGeneratesAndReturns(t *testing.T) {
	mux, _, _, _, _ := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	rid := rr.Header().Get("X-Request-ID")
	if rid == "" {
		t.Fatal("缺失 X-Request-ID 时应生成并回写 request ID")
	}
	if len(rid) != 32 {
		t.Errorf("生成的 request ID 应为 32 字符十六进制，实际 %d 字符: %q", len(rid), rid)
	}
}

// TestRequestID_InvalidGenerates 验证非法 X-Request-ID 时服务器生成新的 ID。
func TestRequestID_InvalidGenerates(t *testing.T) {
	mux, _, _, _, _ := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("X-Request-ID", "contains spaces and <special>")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	rid := rr.Header().Get("X-Request-ID")
	if rid == "contains spaces and <special>" {
		t.Error("非法 request ID 不应被保留")
	}
	if rid == "" {
		t.Fatal("非法 request ID 应触发生成新 ID")
	}
	if len(rid) != 32 {
		t.Errorf("生成的 request ID 应为 32 字符十六进制，实际 %d 字符: %q", len(rid), rid)
	}
}

// TestRequestID_ValidPreserved 验证合法 X-Request-ID 被保留并回写。
func TestRequestID_ValidPreserved(t *testing.T) {
	mux, _, _, _, _ := setupTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("X-Request-ID", "test-req-id-abc-123")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	rid := rr.Header().Get("X-Request-ID")
	if rid != "test-req-id-abc-123" {
		t.Errorf("合法 request ID 应被保留，实际 %q", rid)
	}
}

// TestRequestID_AuditUsesContext 验证审计记录使用 context 中的 request ID，
// 而非仅依赖 header。
func TestRequestID_AuditUsesContext(t *testing.T) {
	mux, authRepo, _, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	body := `{"name":"audit-rid-ws","display_name":"Audit RID WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	// 不设置 X-Request-ID header——验证审计记录仍能获取 context 中的 request ID
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 应返回 201，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 从响应头获取服务器生成的 request ID
	expectedRID := rr.Header().Get("X-Request-ID")
	if expectedRID == "" {
		t.Fatal("响应应包含 X-Request-ID")
	}

	// 验证审计记录中的 request ID 与响应头一致
	entries := auditRepo.GetEntries()
	found := false
	for _, e := range entries {
		if e.action == "workspace.create" {
			if e.requestID != expectedRID {
				t.Errorf("审计记录 request_id = %q, 期望 %q（来自 context）", e.requestID, expectedRID)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("应产生 workspace.create 审计记录")
	}
}

// TestRequestID_AuditUsesContextWithProvidedID 验证当客户端提供合法 X-Request-ID 时，
// 审计记录使用 context 中的同一 request ID。
func TestRequestID_AuditUsesContextWithProvidedID(t *testing.T) {
	mux, authRepo, _, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "wscreator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "wscreator")

	body := `{"name":"audit-rid-ws2","display_name":"Audit RID WS2"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	req.Header.Set("X-Request-ID", "client-provided-req-id-001")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 应返回 201，实际 %d", rr.Code)
	}

	// 验证响应头保留了客户端提供的 request ID
	if rr.Header().Get("X-Request-ID") != "client-provided-req-id-001" {
		t.Errorf("响应应保留客户端 request ID，实际 %q", rr.Header().Get("X-Request-ID"))
	}

	// 验证审计记录使用了 context 中的 request ID
	entries := auditRepo.GetEntries()
	found := false
	for _, e := range entries {
		if e.action == "workspace.create" {
			if e.requestID != "client-provided-req-id-001" {
				t.Errorf("审计记录 request_id = %q, 期望 client-provided-req-id-001", e.requestID)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("应产生 workspace.create 审计记录")
	}
}

// --- Phase6 workspace membership/settings 测试 ---

// TestGetMyMembership 验证 GET /api/workspaces/{id}/me/membership 返回当前用户角色。
func TestGetMyMembership(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")
	req := authedRequest(http.MethodGet, "/api/workspaces/"+ws.ID+"/me/membership", sessionToken, csrfToken, "")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("获取 membership 应返回 200，实际 %d", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp["role"] != RoleOwner {
		t.Errorf("membership role = %q, 期望 %q", resp["role"], RoleOwner)
	}
	if resp["workspace_id"] != ws.ID {
		t.Errorf("workspace_id = %q, 期望 %q", resp["workspace_id"], ws.ID)
	}
}

// TestUpdateWorkspace_SettingsAdminSuccess 验证 admin 可以安全更新 workspace 设置。
func TestUpdateWorkspace_SettingsAdminSuccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "admin1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "admin-001", RoleAdmin)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "admin1")

	body := `{"display_name":"Updated","description":"Updated desc","revision_retention_days":60,"revision_max_count":200,"max_document_size_bytes":1048576}`
	req := authedRequest(http.MethodPut, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("admin 更新 settings 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp workspaceResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.DisplayName != "Updated" {
		t.Errorf("display_name = %q, 期望 Updated", resp.DisplayName)
	}
	if resp.RevisionRetentionDays != 60 {
		t.Errorf("revision_retention_days = %d, 期望 60", resp.RevisionRetentionDays)
	}
	if resp.RevisionMaxCount != 200 {
		t.Errorf("revision_max_count = %d, 期望 200", resp.RevisionMaxCount)
	}
	if resp.MaxDocumentSizeBytes != 1048576 {
		t.Errorf("max_document_size_bytes = %d, 期望 1048576", resp.MaxDocumentSizeBytes)
	}
}

// TestUpdateWorkspace_SettingsViewerDenied 验证 viewer 不能更新 workspace 设置。
func TestUpdateWorkspace_SettingsViewerDenied(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "viewer-001", "viewer1", "user", false)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")
	wsRepo.AddMember(context.Background(), ws.ID, "viewer-001", RoleViewer)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "viewer1")

	body := `{"revision_retention_days":60}`
	req := authedRequest(http.MethodPut, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("viewer 更新 settings 应返回 403，实际 %d", rr.Code)
	}
}

// TestUpdateWorkspace_SettingsOutOfRange 验证越界设置返回 400。
func TestUpdateWorkspace_SettingsOutOfRange(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owner1", "user", true)

	ws, _ := wsRepo.CreateWorkspace(context.Background(), "team-ws", "Team", "", "owner-001")

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owner1")

	cases := []string{
		`{"revision_retention_days":0}`,
		`{"revision_retention_days":4000}`,
		`{"revision_max_count":0}`,
		`{"revision_max_count":20000}`,
		`{"max_document_size_bytes":512}`,
		`{"max_document_size_bytes":200000000}`,
	}
	for _, c := range cases {
		req := authedRequest(http.MethodPut, "/api/workspaces/"+ws.ID, sessionToken, csrfToken, c)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("非法设置 %s 应返回 400，实际 %d", c, rr.Code)
		}
	}
}
