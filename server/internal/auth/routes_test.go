// routes_test.go 测试 auth 模块经真实 mux + 完整 middleware 链的端到端路由行为。
//
// 引入动机：WP-2 缺陷修复要求验证 middleware 链顺序正确性——
// 请求必须按 AuthMiddleware → RequireAuth → RequireCSRF → handler 顺序进入，
// 否则 RequireAuth 在 Identity 尚未注入 context 时永远返回 401。
// 此测试通过真实 http.ServeMux + RegisterRoutes 构建完整路由，
// 不手动注入 Identity，确保 middleware 链本身正确工作。
//
// 测试覆盖：
//   - 已登录 + CSRF 正确时，各 protected endpoint 到达 handler（不返回 401）
//   - 缺失身份时 protected endpoint 返回 401
//   - 有效身份 + 缺失 CSRF 返回 403
//   - 有效身份 + 错误 CSRF 返回 403
//   - writeError 能安全编码含引号、反斜杠等字符的消息，响应为合法 JSON
//   - Revoke 空 body 成功沿用既有语义
//   - Revoke 畸形 JSON 返回 400
//   - Revoke 未知字段返回 400
//   - Revoke 多个 JSON 值返回 400
package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// setupRouter 创建用于端到端测试的完整 mux + handler + mock repository。
// 返回 mux、mockRepo、cfg，以及一个已登录用户的 session token 和 CSRF token。
func setupRouter(t *testing.T) (mux *http.ServeMux, repo *MockRepository, cfg AuthConfig) {
	t.Helper()
	cfg = testAuthCfg()
	repo = NewMockRepository()

	// 添加测试用户
	hash, err := HashPassword("testpass123", cfg)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}
	repo.AddUser(&User{
		ID:                  "user-001",
		Username:            "testuser",
		Email:               "test@example.com",
		PasswordHash:        hash,
		SystemRole:          "user",
		WorkspaceCreatePerm: false,
	})

	handler := NewHandler(repo, cfg)
	mux = http.NewServeMux()
	RegisterRoutes(mux, handler, repo, cfg)
	return mux, repo, cfg
}

// doLoginViaRouter 通过完整 mux 执行 login 请求，返回响应和解析后的 body。
func doLoginViaRouter(t *testing.T, mux *http.ServeMux, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// extractSessionAndCSRF 从 login 响应中提取 session token 和 CSRF token。
func extractSessionAndCSRF(t *testing.T, rr *httptest.ResponseRecorder, cfg AuthConfig) (sessionToken, csrfToken string) {
	t.Helper()
	cookies := rr.Result().Cookies()
	for _, c := range cookies {
		if c.Name == cfg.CookieName {
			sessionToken = c.Value
		}
		if c.Name == cfg.CSRFCookieName {
			csrfToken = c.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("login 响应中应包含 session cookie")
	}
	if csrfToken == "" {
		t.Fatal("login 响应中应包含 CSRF cookie")
	}
	return
}

// --- middleware 链顺序测试 ---

// TestRoute_Logout_WithAuthAndCSRF 验证已登录 + 正确 CSRF 时 logout 到达 handler。
// 此测试覆盖 WP-2 核心缺陷：修复前 RequireAuth 在 AuthMiddleware 外层，
// Identity 尚未注入，永远返回 401；修复后应返回 200。
func TestRoute_Logout_WithAuthAndCSRF(t *testing.T) {
	mux, _, cfg := setupRouter(t)

	// 先 login
	rr := doLoginViaRouter(t, mux, "testuser", "testpass123")
	if rr.Code != http.StatusOK {
		t.Fatalf("login 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
	sessionToken, csrfToken := extractSessionAndCSRF(t, rr, cfg)

	// 通过完整 mux 发起 logout 请求
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: cfg.CSRFCookieName, Value: csrfToken})
	req.Header.Set(cfg.CSRFHeaderName, csrfToken)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("已登录 + 正确 CSRF 的 logout 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRoute_DeviceAuthorize_WithAuthAndCSRF 验证已登录 + 正确 CSRF 时 device/authorize 到达 handler。
func TestRoute_DeviceAuthorize_WithAuthAndCSRF(t *testing.T) {
	mux, _, cfg := setupRouter(t)

	rr := doLoginViaRouter(t, mux, "testuser", "testpass123")
	if rr.Code != http.StatusOK {
		t.Fatalf("login 应返回 200，实际 %d", rr.Code)
	}
	sessionToken, csrfToken := extractSessionAndCSRF(t, rr, cfg)

	body := `{"device_name":"test-device"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/authorize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: cfg.CSRFCookieName, Value: csrfToken})
	req.Header.Set(cfg.CSRFHeaderName, csrfToken)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("已登录 + 正确 CSRF 的 device/authorize 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp deviceAuthorizeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应体失败: %v", err)
	}
	if resp.AccessToken == "" {
		t.Error("access token 不应为空")
	}
}

// TestRoute_Revoke_WithAuthAndCSRF 验证已登录 + 正确 CSRF 时 revoke 到达 handler。
func TestRoute_Revoke_WithAuthAndCSRF(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	// 先创建一个 device session 供撤销
	deviceResult, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// Login
	rr := doLoginViaRouter(t, mux, "testuser", "testpass123")
	if rr.Code != http.StatusOK {
		t.Fatalf("login 应返回 200，实际 %d", rr.Code)
	}
	sessionToken, csrfToken := extractSessionAndCSRF(t, rr, cfg)

	body := `{"device_session_id":"` + deviceResult.DeviceSessionID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: cfg.CSRFCookieName, Value: csrfToken})
	req.Header.Set(cfg.CSRFHeaderName, csrfToken)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("已登录 + 正确 CSRF 的 revoke 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRoute_Protected_NoAuth_Returns401 验证缺失身份时所有 protected endpoint 返回 401。
func TestRoute_Protected_NoAuth_Returns401(t *testing.T) {
	mux, _, _ := setupRouter(t)

	endpoints := []string{
		"/api/auth/logout",
		"/api/auth/device/authorize",
		"/api/auth/revoke",
	}

	for _, ep := range endpoints {
		req := httptest.NewRequest(http.MethodPost, ep, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s 无身份应返回 401，实际 %d", ep, rr.Code)
		}
	}
}

// TestRoute_Protected_AuthMissingCSRF_Returns403 验证有效身份 + 缺失 CSRF 返回 403。
func TestRoute_Protected_AuthMissingCSRF_Returns403(t *testing.T) {
	mux, _, cfg := setupRouter(t)

	rr := doLoginViaRouter(t, mux, "testuser", "testpass123")
	sessionToken, _ := extractSessionAndCSRF(t, rr, cfg)

	// 发起 logout 请求，带 session cookie 但不带 CSRF header
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("有效身份 + 缺失 CSRF 应返回 403，实际 %d", rr.Code)
	}
}

// TestRoute_Protected_AuthWrongCSRF_Returns403 验证有效身份 + 错误 CSRF 返回 403。
func TestRoute_Protected_AuthWrongCSRF_Returns403(t *testing.T) {
	mux, _, cfg := setupRouter(t)

	rr := doLoginViaRouter(t, mux, "testuser", "testpass123")
	sessionToken, _ := extractSessionAndCSRF(t, rr, cfg)

	// 发起 logout 请求，带 session cookie 和错误的 CSRF header
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.Header.Set(cfg.CSRFHeaderName, "wrong-csrf-token")
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("有效身份 + 错误 CSRF 应返回 403，实际 %d", rr.Code)
	}
}

// TestRoute_Protected_BearerAuth_NoCSRFNeeded 验证 bearer token 认证不需要 CSRF，
// 且能正确通过 AuthMiddleware → RequireAuth → RequireCSRF 链到达 handler。
func TestRoute_Protected_BearerAuth_NoCSRFNeeded(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	// 创建 device session 获取 access token
	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 用 bearer token 发起 revoke 请求（空 body 撤销当前 session）
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+result.AccessToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("bearer token 认证的 revoke 应返回 200（不需要 CSRF），实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// --- writeError JSON 安全编码测试 ---

// TestWriteError_SpecialCharacters 验证 writeError 能安全编码含引号、反斜杠等字符的消息，
// 响应必须是合法 JSON。
func TestWriteError_SpecialCharacters(t *testing.T) {
	tests := []struct {
		desc    string
		message string
	}{
		{"含双引号", `用户名或密码"错误`},
		{"含反斜杠", `路径\C\错误`},
		{"含换行符", "第一行\n第二行"},
		{"含制表符", "字段\t值"},
		{"含Unicode", "用户名或密码错误：密码不匹配"},
		{"混合特殊字符", `"quotes" and \backslash\ and {braces}`},
	}

	for _, tt := range tests {
		rr := httptest.NewRecorder()
		writeError(rr, http.StatusBadRequest, tt.message)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码应为 400，实际 %d", tt.desc, rr.Code)
		}

		// 验证响应体是合法 JSON
		var resp map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Errorf("%s: 响应体不是合法 JSON: %v, body: %s", tt.desc, err, rr.Body.String())
			continue
		}

		// 验证 error 字段值与原始消息一致
		if resp["error"] != tt.message {
			t.Errorf("%s: error 字段值 = %q, 期望 %q", tt.desc, resp["error"], tt.message)
		}
	}
}

// --- Revoke 请求体解析测试 ---

// TestRoute_Revoke_EmptyBody_BearerAuth 验证空 body 时 revoke 沿用既有默认撤销逻辑。
func TestRoute_Revoke_EmptyBody_BearerAuth(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 空 body，bearer token 认证——应撤销当前 device session
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+result.AccessToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("空 body + bearer 认证的 revoke 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 验证 device session 已撤销
	ds, err := repo.GetDeviceSessionByID(nil, result.DeviceSessionID)
	if err != nil {
		t.Fatalf("查询 device session 失败: %v", err)
	}
	if !ds.RevokedAt.Valid {
		t.Error("device session 应已撤销")
	}
}

// TestRoute_Revoke_MalformedJSON_Returns400 验证畸形 JSON 返回 400。
func TestRoute_Revoke_MalformedJSON_Returns400(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 畸形 JSON
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader("not json at all"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+result.AccessToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("畸形 JSON 应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRoute_Revoke_UnknownField_Returns400 验证未知字段返回 400。
func TestRoute_Revoke_UnknownField_Returns400(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 包含未知字段
	body := `{"device_session_id":"","extra_field":"malicious"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+result.AccessToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("包含未知字段的请求体应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRoute_Revoke_MultipleJSONValues_Returns400 验证多个 JSON 值返回 400。
func TestRoute_Revoke_MultipleJSONValues_Returns400(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 多个 JSON 值
	body := `{"device_session_id":""}{"extra":"second"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+result.AccessToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("包含多个 JSON 值的请求体应返回 400，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRoute_Revoke_ValidJSONWithDeviceSessionID 验证合法 JSON + device_session_id 通过完整链路。
func TestRoute_Revoke_ValidJSONWithDeviceSessionID(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	// 创建两个 device session，一个供撤销，一个用于 bearer 认证
	authResult, err := AuthorizeDevice(nil, repo, cfg, "user-001", "auth-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}
	targetResult, err := AuthorizeDevice(nil, repo, cfg, "user-001", "target-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	body := `{"device_session_id":"` + targetResult.DeviceSessionID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authResult.AccessToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("合法 JSON + bearer 认证的 revoke 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 验证目标 device session 已撤销
	ds, err := repo.GetDeviceSessionByID(nil, targetResult.DeviceSessionID)
	if err != nil {
		t.Fatalf("查询目标 device session 失败: %v", err)
	}
	if !ds.RevokedAt.Valid {
		t.Error("目标 device session 应已撤销")
	}
}

// TestRoute_Refresh_NoCSRF 验证 refresh 端点不需要 CSRF（基于 refresh token 认证）。
func TestRoute_Refresh_NoCSRF(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	body := `{"refresh_token":"` + result.RefreshToken + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// 不带任何 cookie 或 CSRF header
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("refresh 不需要 CSRF，应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRoute_Login_NoCSRF 验证 login 端点不需要 CSRF。
func TestRoute_Login_NoCSRF(t *testing.T) {
	mux, _, _ := setupRouter(t)

	body := `{"username":"testuser","password":"testpass123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// 不带任何 cookie 或 CSRF header
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("login 不需要 CSRF，应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestRoute_Protected_ExpiredSession_Returns401 验证过期 session 的请求返回 401。
func TestRoute_Protected_ExpiredSession_Returns401(t *testing.T) {
	mux, repo, cfg := setupRouter(t)

	// 创建一个已过期的 session
	sessionToken, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken 失败: %v", err)
	}
	csrfToken, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken 失败: %v", err)
	}
	_, err = repo.CreateSession(nil, "user-001", HashToken(sessionToken), HashToken(csrfToken), time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("CreateSession 失败: %v", err)
	}

	// 发起 logout 请求，带过期 session cookie 和 CSRF header
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.Header.Set(cfg.CSRFHeaderName, csrfToken)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("过期 session 应返回 401，实际 %d", rr.Code)
	}
}

// TestWriteError_ContentType 验证 writeError 设置正确的 Content-Type。
func TestWriteError_ContentType(t *testing.T) {
	rr := httptest.NewRecorder()
	writeError(rr, http.StatusForbidden, "测试消息")

	ct := rr.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, 期望 application/json", ct)
	}
}

// TestWriteError_ValidJSON 验证 writeError 的输出始终是合法 JSON。
func TestWriteError_ValidJSON(t *testing.T) {
	// 使用一个极端消息测试
	extreme := `"\\\n\t{"`
	rr := httptest.NewRecorder()
	writeError(rr, http.StatusInternalServerError, extreme)

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("writeError 输出不是合法 JSON: %v, body: %s", err, rr.Body.String())
	}
	if resp["error"] != extreme {
		t.Errorf("error = %q, 期望 %q", resp["error"], extreme)
	}
}

// TestRoute_DeviceSessions_ListsOwnMetadata 验证设备会话列表只返回当前用户的非敏感元数据。
func TestRoute_DeviceSessions_ListsOwnMetadata(t *testing.T) {
	mux, repo, cfg := setupRouter(t)
	deviceResult, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}
	login := doLoginViaRouter(t, mux, "testuser", "testpass123")
	sessionToken, _ := extractSessionAndCSRF(t, login, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/device/sessions", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("设备会话列表应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var response struct {
		Sessions []DeviceSessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("解析设备会话列表失败: %v", err)
	}
	if len(response.Sessions) != 1 || response.Sessions[0].ID != deviceResult.DeviceSessionID {
		t.Fatalf("设备会话列表内容不正确: %+v", response.Sessions)
	}
	if response.Sessions[0].DeviceName != "test-device" {
		t.Errorf("device_name = %q", response.Sessions[0].DeviceName)
	}
	if strings.Contains(rr.Body.String(), deviceResult.AccessToken) || strings.Contains(rr.Body.String(), deviceResult.RefreshToken) {
		t.Error("设备会话列表不得暴露 token")
	}
}
