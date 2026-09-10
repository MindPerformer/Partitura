// creator_owner_test.go 测试 workspace 创建者自动获得 owner 角色的行为。
//
// 测试覆盖：
//   - 创建 workspace 后创建者立即为 owner
//   - 新创建的普通用户无任何 workspace 成员关系
//   - owner/admin 添加成员后新成员可访问 workspace
//   - 非成员无法访问 workspace
//   - 最后一个 owner 不可被移除或降级
package workspace

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreatorIsOwner 验证创建 workspace 后创建者立即为 owner。
func TestCreatorIsOwner(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "creator-001", "creator", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "creator")

	// 创建 workspace
	body := `{"name":"test-ws","display_name":"Test WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 失败: %d, body: %s", rr.Code, rr.Body.String())
	}

	// 查询创建者的成员角色
	role, err := wsRepo.GetMemberRole(t.Context(), "ws-1", "creator-001")
	if err != nil {
		t.Fatalf("查询成员角色失败: %v", err)
	}
	if role != RoleOwner {
		t.Errorf("创建者角色应为 owner，实际 %s", role)
	}
}

// TestNewUserNoWorkspaceMembership 验证新创建的普通用户无任何 workspace 成员关系。
func TestNewUserNoWorkspaceMembership(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)
	createTestUserInAuth(t, authRepo, cfg, "creator-001", "creator", "user", true)

	adminSession, adminCSRF := loginAndGetCookies(t, mux, cfg, "adminuser")

	// 创建新用户
	body := `{"username":"freshuser","email":"fresh@test.com","password":"testpass123"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", adminSession, adminCSRF, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建用户失败: %d", rr.Code)
	}

	// 创建者创建 workspace
	creatorSession, creatorCSRF := loginAndGetCookies(t, mux, cfg, "creator")
	wsBody := `{"name":"test-ws","display_name":"Test WS"}`
	wsReq := authedRequest(http.MethodPost, "/api/workspaces", creatorSession, creatorCSRF, wsBody)
	wsRR := httptest.NewRecorder()
	mux.ServeHTTP(wsRR, wsReq)
	if wsRR.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 失败: %d", wsRR.Code)
	}

	// 新用户不应能访问该 workspace
	freshSession, freshCSRF := loginAndGetCookies(t, mux, cfg, "freshuser")
	getReq := authedRequest(http.MethodGet, "/api/workspaces/ws-1", freshSession, freshCSRF, "")
	getRR := httptest.NewRecorder()
	mux.ServeHTTP(getRR, getReq)

	if getRR.Code != http.StatusNotFound {
		t.Errorf("非成员访问 workspace 应返回 404（不泄露存在性），实际 %d", getRR.Code)
	}

	// 新用户的 workspace 列表应为空
	listReq := authedRequest(http.MethodGet, "/api/workspaces", freshSession, freshCSRF, "")
	listRR := httptest.NewRecorder()
	mux.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("查询 workspace 列表失败: %d", listRR.Code)
	}

	// 验证返回的 workspace 列表中不包含 ws-1
	if strings.Contains(listRR.Body.String(), "ws-1") {
		t.Error("新用户的 workspace 列表不应包含未加入的 workspace")
	}
}

// TestAddMemberThenAccess 验证 owner 添加成员后新成员可访问 workspace。
func TestAddMemberThenAccess(t *testing.T) {
	mux, authRepo, wsRepo, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)
	createTestUserInAuth(t, authRepo, cfg, "creator-001", "creator", "user", true)
	createTestUserInAuth(t, authRepo, cfg, "viewer-001", "vieweruser", "user", false)
	wsRepo.AddUser("viewer-001", "vieweruser", "viewer@test.com", "user", false)

	// 创建者创建 workspace
	creatorSession, creatorCSRF := loginAndGetCookies(t, mux, cfg, "creator")
	wsBody := `{"name":"test-ws","display_name":"Test WS"}`
	wsReq := authedRequest(http.MethodPost, "/api/workspaces", creatorSession, creatorCSRF, wsBody)
	wsRR := httptest.NewRecorder()
	mux.ServeHTTP(wsRR, wsReq)
	if wsRR.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 失败: %d", wsRR.Code)
	}

	// 添加 viewer 为成员
	addBody := `{"username":"vieweruser","role":"viewer"}`
	addReq := authedRequest(http.MethodPost, "/api/workspaces/ws-1/members", creatorSession, creatorCSRF, addBody)
	addRR := httptest.NewRecorder()
	mux.ServeHTTP(addRR, addReq)
	if addRR.Code != http.StatusCreated {
		t.Fatalf("添加成员失败: %d, body: %s", addRR.Code, addRR.Body.String())
	}

	// viewer 现在应该能访问 workspace
	viewerSession, viewerCSRF := loginAndGetCookies(t, mux, cfg, "vieweruser")
	getReq := authedRequest(http.MethodGet, "/api/workspaces/ws-1", viewerSession, viewerCSRF, "")
	getRR := httptest.NewRecorder()
	mux.ServeHTTP(getRR, getReq)

	if getRR.Code != http.StatusOK {
		t.Errorf("添加成员后应能访问 workspace，实际 %d", getRR.Code)
	}
}

// TestLastOwnerCannotBeRemoved 验证最后一个 owner 不可被移除。
func TestLastOwnerCannotBeRemoved(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owneruser", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owneruser")

	// 创建 workspace
	body := `{"name":"test-ws","display_name":"Test WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 失败: %d", rr.Code)
	}

	// 尝试移除自己（唯一的 owner）
	delReq := authedRequest(http.MethodDelete, "/api/workspaces/ws-1/members/owner-001", sessionToken, csrfToken, "")
	delRR := httptest.NewRecorder()
	mux.ServeHTTP(delRR, delReq)

	if delRR.Code == http.StatusOK {
		t.Error("移除最后一个 owner 应被拒绝")
	}
}

// TestLastOwnerCannotBeDowngraded 验证最后一个 owner 不可被降级。
func TestLastOwnerCannotBeDowngraded(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "owner-001", "owneruser", "user", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "owneruser")

	// 创建 workspace
	body := `{"name":"test-ws","display_name":"Test WS"}`
	req := authedRequest(http.MethodPost, "/api/workspaces", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建 workspace 失败: %d", rr.Code)
	}

	// 尝试降级自己
	updateBody := `{"role":"editor"}`
	updateReq := authedRequest(http.MethodPut, "/api/workspaces/ws-1/members/owner-001", sessionToken, csrfToken, updateBody)
	updateRR := httptest.NewRecorder()
	mux.ServeHTTP(updateRR, updateReq)

	if updateRR.Code == http.StatusOK {
		t.Error("降级最后一个 owner 应被拒绝")
	}
}
