// admin_create_user_test.go 测试 system_admin 创建普通用户 API 的端到端行为。
//
// 测试覆盖：
//   - 非 system_admin 用户 403
//   - 缺少 CSRF 403
//   - 请求体格式错误 400
//   - 未知字段 400
//   - 空用户名/邮箱/密码 400
//   - 密码长度不足 400
//   - 尝试创建 system_admin 400
//   - 重复用户名 409
//   - 成功创建用户 201，响应包含正确字段
//   - 创建后可用新用户凭据登录
//   - 密码哈希非明文
//   - 审计日志记录创建事件，不含密码
package workspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateUser_NonAdminForbidden 验证非 system_admin 用户不能创建用户。
func TestCreateUser_NonAdminForbidden(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "user-001", "regularuser", "user", false)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "regularuser")

	body := `{"username":"newuser","email":"new@test.com","password":"password123"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("非 admin 创建用户应返回 403，实际 %d", rr.Code)
	}
}

// TestCreateUser_NoCSRF 验证缺少 CSRF token 时创建用户被拒绝。
func TestCreateUser_NoCSRF(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	sessionToken, _ := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"username":"newuser","email":"new@test.com","password":"password123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: "session", Value: sessionToken})
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("缺少 CSRF 应返回 403，实际 %d", rr.Code)
	}
}

// TestCreateUser_InvalidJSON 验证请求体格式错误返回 400。
func TestCreateUser_InvalidJSON(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{invalid json}`
	req := authedRequest(http.MethodPost, "/api/admin/users", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应返回 400，实际 %d", rr.Code)
	}
}

// TestCreateUser_UnknownField 验证未知字段返回 400（严格 JSON 解码）。
func TestCreateUser_UnknownField(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"username":"newuser","email":"new@test.com","password":"password123","unknown_field":"value"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("未知字段应返回 400，实际 %d", rr.Code)
	}
}

// TestCreateUser_EmptyFields 验证空用户名/邮箱/密码返回 400。
func TestCreateUser_EmptyFields(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminuser")

	cases := []struct {
		name string
		body string
	}{
		{"空用户名", `{"username":"","email":"new@test.com","password":"password123"}`},
		{"空邮箱", `{"username":"newuser","email":"","password":"password123"}`},
		{"空密码", `{"username":"newuser","email":"new@test.com","password":""}`},
	}

	for _, c := range cases {
		req := authedRequest(http.MethodPost, "/api/admin/users", sessionToken, csrfToken, c.body)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实际 %d", c.name, rr.Code)
		}
	}
}

// TestCreateUser_ShortPassword 验证密码长度不足 8 返回 400。
func TestCreateUser_ShortPassword(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"username":"newuser","email":"new@test.com","password":"short"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("短密码应返回 400，实际 %d", rr.Code)
	}
}

// TestCreateUser_SystemAdminRole 验证不允许通过此端点创建 system_admin。
func TestCreateUser_SystemAdminRole(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"username":"newadmin","email":"newadmin@test.com","password":"password123","system_role":"system_admin"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("创建 system_admin 应返回 400，实际 %d", rr.Code)
	}
}

// TestCreateUser_Success 验证成功创建用户返回 201，响应包含正确字段。
func TestCreateUser_Success(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	sessionToken, csrfToken := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"username":"newuser","email":"new@test.com","password":"password123"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", sessionToken, csrfToken, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("创建用户应返回 201，实际 %d, body: %s", rr.Code, rr.Body.String())
	}

	var resp adminCreateUserResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if resp.ID == "" {
		t.Error("响应中 ID 不应为空")
	}
	if resp.Username != "newuser" {
		t.Errorf("用户名应为 newuser，实际 %s", resp.Username)
	}
	if resp.Email != "new@test.com" {
		t.Errorf("邮箱应为 new@test.com，实际 %s", resp.Email)
	}
	if resp.SystemRole != "user" {
		t.Errorf("系统角色应为 user，实际 %s", resp.SystemRole)
	}
	if resp.WorkspaceCreatePerm != false {
		t.Error("workspace_create_perm 应为 false")
	}
}

// TestCreateUser_CanLoginAfterCreate 验证创建的用户可以用初始密码登录。
func TestCreateUser_CanLoginAfterCreate(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	adminSession, adminCSRF := loginAndGetCookies(t, mux, cfg, "adminuser")

	// 创建新用户
	body := `{"username":"loginuser","email":"login@test.com","password":"testpass123"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", adminSession, adminCSRF, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建用户失败: %d, body: %s", rr.Code, rr.Body.String())
	}

	// 用新用户登录
	loginBody := `{"username":"loginuser","password":"testpass123"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginRR := httptest.NewRecorder()
	mux.ServeHTTP(loginRR, loginReq)

	if loginRR.Code != http.StatusOK {
		t.Errorf("新用户登录应返回 200，实际 %d, body: %s", loginRR.Code, loginRR.Body.String())
	}
}

// TestCreateUser_PasswordHashNotPlaintext 验证存储的密码哈希不是明文。
func TestCreateUser_PasswordHashNotPlaintext(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	adminSession, adminCSRF := loginAndGetCookies(t, mux, cfg, "adminuser")

	// 创建新用户
	body := `{"username":"hashuser","email":"hash@test.com","password":"secretpass123"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", adminSession, adminCSRF, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建用户失败: %d", rr.Code)
	}

	// 检查存储的密码哈希
	user, err := authRepo.GetUserByUsername(t.Context(), "hashuser")
	if err != nil {
		t.Fatalf("查询新用户失败: %v", err)
	}
	if user.PasswordHash == "secretpass123" {
		t.Error("密码哈希不应为明文")
	}
	if !strings.HasPrefix(user.PasswordHash, "$argon2id$") {
		t.Errorf("密码哈希应为 Argon2id 格式，实际: %s", user.PasswordHash[:20])
	}
}

// TestCreateUser_DuplicateUsername 验证重复用户名返回 409。
func TestCreateUser_DuplicateUsername(t *testing.T) {
	mux, authRepo, _, _, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)
	createTestUserInAuth(t, authRepo, cfg, "user-002", "existinguser", "user", false)

	adminSession, adminCSRF := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"username":"existinguser","email":"another@test.com","password":"password123"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", adminSession, adminCSRF, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("重复用户名应返回 409，实际 %d", rr.Code)
	}
}

// TestCreateUser_AuditRecorded 验证创建用户后审计日志被记录，且不含密码。
func TestCreateUser_AuditRecorded(t *testing.T) {
	mux, authRepo, _, auditRepo, cfg := setupTestEnv(t)
	createTestUserInAuth(t, authRepo, cfg, "admin-001", "adminuser", "system_admin", true)

	adminSession, adminCSRF := loginAndGetCookies(t, mux, cfg, "adminuser")

	body := `{"username":"audituser","email":"audit@test.com","password":"auditpass123"}`
	req := authedRequest(http.MethodPost, "/api/admin/users", adminSession, adminCSRF, body)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("创建用户失败: %d", rr.Code)
	}

	entries := auditRepo.GetEntries()
	found := false
	for _, e := range entries {
		if e.action == "admin.user.create" {
			found = true
			// 验证审计详情不含密码
			detailStr := string(e.detail)
			if strings.Contains(detailStr, "auditpass123") {
				t.Error("审计日志不应包含密码")
			}
			if strings.Contains(detailStr, "password") {
				t.Error("审计日志不应包含 password 字段")
			}
			break
		}
	}
	if !found {
		t.Error("未找到 admin.user.create 审计记录")
	}
}
