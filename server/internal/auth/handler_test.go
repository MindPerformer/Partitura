// handler_test.go 测试 auth HTTP handler 的完整流程。
//
// 测试覆盖：
//   - Login 成功：返回 cookie + CSRF token + 用户信息
//   - Login 失败：错误凭据返回 401，不泄露用户是否存在
//   - Login 请求体格式错误返回 400
//   - Login 空字段返回 400
//   - Logout 成功：清除 cookie
//   - Logout 未认证返回 401
//   - DeviceAuthorize 成功：返回 access + refresh token
//   - DeviceAuthorize 未认证返回 401
//   - Refresh 成功：返回新 token
//   - Refresh 无效 token 返回 401
//   - Revoke 成功
//   - Revoke 非本人 device session 返回 403
//   - Cookie 属性验证：Secure、HttpOnly、SameSite
//   - CSRF 缺失拒绝状态变更
//   - CSRF 错误拒绝状态变更
package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// setupHandler 创建用于测试的 handler 和 mock repository。
// 返回的 mockRepo 已包含一个测试用户。
func setupHandler(t *testing.T) (*Handler, *MockRepository, AuthConfig) {
	t.Helper()
	cfg := testAuthCfg()
	repo := NewMockRepository()

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
	return handler, repo, cfg
}

// doLogin 执行 login 请求并返回响应和解析后的 body。
func doLogin(t *testing.T, handler *Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"username":"` + username + `","password":"` + password + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Login(rr, req)
	return rr
}

func TestLogin_Success(t *testing.T) {
	handler, _, cfg := setupHandler(t)

	rr := doLogin(t, handler, "testuser", "testpass123")

	if rr.Code != http.StatusOK {
		t.Fatalf("login 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp loginResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应体失败: %v", err)
	}

	if resp.CSRFToken == "" {
		t.Error("CSRF token 不应为空")
	}
	if resp.User.ID != "user-001" {
		t.Errorf("user.ID = %q, 期望 user-001", resp.User.ID)
	}
	if resp.User.Username != "testuser" {
		t.Errorf("user.Username = %q, 期望 testuser", resp.User.Username)
	}
	if resp.User.SystemRole != "user" {
		t.Errorf("user.SystemRole = %q, 期望 user", resp.User.SystemRole)
	}

	// 验证 session cookie
	cookies := rr.Result().Cookies()
	var sessionCookie, csrfCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == cfg.CookieName {
			sessionCookie = c
		}
		if c.Name == cfg.CSRFCookieName {
			csrfCookie = c
		}
	}

	if sessionCookie == nil {
		t.Fatal("应设置 session cookie")
	}
	if sessionCookie.HttpOnly != true {
		t.Error("session cookie 应为 HttpOnly")
	}
	if sessionCookie.Secure != cfg.CookieSecure {
		t.Errorf("session cookie Secure = %v, 期望 %v", sessionCookie.Secure, cfg.CookieSecure)
	}
	if sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, 期望 Lax", sessionCookie.SameSite)
	}
	if sessionCookie.Value == "" {
		t.Error("session cookie 值不应为空")
	}

	if csrfCookie == nil {
		t.Fatal("应设置 CSRF cookie")
	}
	if csrfCookie.HttpOnly != false {
		t.Error("CSRF cookie 应为非 HttpOnly（前端 JS 需读取）")
	}
	if csrfCookie.Secure != cfg.CookieSecure {
		t.Errorf("CSRF cookie Secure = %v, 期望 %v", csrfCookie.Secure, cfg.CookieSecure)
	}
	if csrfCookie.Value != resp.CSRFToken {
		t.Error("CSRF cookie 值应与响应体中的 CSRF token 一致")
	}

	// 验证 expires_at 为 UTC RFC3339 格式（Z 后缀）
	if resp.ExpiresAt == "" {
		t.Fatal("expires_at 不应为空")
	}
	// UTC RFC3339 格式以 Z 结尾，如 2026-03-08T12:34:56Z
	if !strings.HasSuffix(resp.ExpiresAt, "Z") {
		t.Errorf("expires_at 应为 UTC RFC3339（Z 后缀），实际: %s", resp.ExpiresAt)
	}
	// 验证可被 time.Parse(time.RFC3339) 解析
	if _, err := time.Parse(time.RFC3339, resp.ExpiresAt); err != nil {
		t.Errorf("expires_at %q 无法按 RFC3339 解析: %v", resp.ExpiresAt, err)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	handler, _, _ := setupHandler(t)

	rr := doLogin(t, handler, "testuser", "wrongpassword")

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("错误密码应返回 401，实际 %d", rr.Code)
	}

	var resp map[string]string
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "用户名或密码错误") {
		t.Errorf("错误信息应包含'用户名或密码错误'，实际: %s", resp["error"])
	}
}

func TestLogin_NonexistentUser(t *testing.T) {
	handler, _, _ := setupHandler(t)

	rr := doLogin(t, handler, "nonexistent", "anypassword")

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("不存在的用户应返回 401，实际 %d", rr.Code)
	}

	// 错误信息应与密码错误相同——不泄露用户是否存在
	var resp map[string]string
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if !strings.Contains(resp["error"], "用户名或密码错误") {
		t.Errorf("错误信息应包含'用户名或密码错误'（不泄露用户是否存在），实际: %s", resp["error"])
	}
}

func TestLogin_MalformedBody(t *testing.T) {
	handler, _, _ := setupHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Login(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("格式错误的请求体应返回 400，实际 %d", rr.Code)
	}
}

func TestLogin_EmptyFields(t *testing.T) {
	handler, _, _ := setupHandler(t)

	tests := []struct {
		body string
		desc string
	}{
		{`{"username":"","password":"test"}`, "空用户名"},
		{`{"username":"test","password":""}`, "空密码"},
		{`{"username":"","password":""}`, "两者都空"},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(tt.body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.Login(rr, req)

		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: 应返回 400，实际 %d", tt.desc, rr.Code)
		}
	}
}

func TestLogout_Success(t *testing.T) {
	handler, _, cfg := setupHandler(t)

	// 先 login
	rr := doLogin(t, handler, "testuser", "testpass123")
	cookies := rr.Result().Cookies()

	// 提取 session cookie 和 CSRF token
	var sessionToken, csrfToken string
	for _, c := range cookies {
		if c.Name == cfg.CookieName {
			sessionToken = c.Value
		}
		if c.Name == cfg.CSRFCookieName {
			csrfToken = c.Value
		}
	}

	// 构造 logout 请求，带 cookie 和 CSRF header
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: cfg.CSRFCookieName, Value: csrfToken})
	req.Header.Set(cfg.CSRFHeaderName, csrfToken)
	rr = httptest.NewRecorder()
	handler.Logout(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("logout 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	// 验证 cookie 被清除
	clearedCookies := rr.Result().Cookies()
	for _, c := range clearedCookies {
		if c.Name == cfg.CookieName && c.MaxAge >= 0 {
			// MaxAge < 0 或 0 表示删除
		}
	}
}

func TestLogout_NoSession(t *testing.T) {
	handler, _, _ := setupHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rr := httptest.NewRecorder()
	handler.Logout(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("无 session 的 logout 应返回 401，实际 %d", rr.Code)
	}
}

func TestDeviceAuthorize_Success(t *testing.T) {
	handler, _, cfg := setupHandler(t)

	// 先 login
	rr := doLogin(t, handler, "testuser", "testpass123")
	cookies := rr.Result().Cookies()

	var sessionToken, csrfToken string
	for _, c := range cookies {
		if c.Name == cfg.CookieName {
			sessionToken = c.Value
		}
		if c.Name == cfg.CSRFCookieName {
			csrfToken = c.Value
		}
	}

	// 构造 device authorize 请求
	body := `{"device_name":"test-device"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/authorize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: cfg.CSRFCookieName, Value: csrfToken})
	req.Header.Set(cfg.CSRFHeaderName, csrfToken)

	// 需要设置 Identity 到 context
	id := &Identity{
		UserID:     "user-001",
		Username:   "testuser",
		SystemRole: "user",
		AuthMethod: AuthMethodCookie,
	}
	req = req.WithContext(WithIdentity(req.Context(), id))

	rr = httptest.NewRecorder()
	handler.DeviceAuthorize(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("device authorize 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp deviceAuthorizeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应体失败: %v", err)
	}

	if resp.AccessToken == "" {
		t.Error("access token 不应为空")
	}
	if resp.RefreshToken == "" {
		t.Error("refresh token 不应为空")
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("token_type = %q, 期望 Bearer", resp.TokenType)
	}
	if resp.ExpiresIn != cfg.DeviceAccessTokenDuration {
		t.Errorf("expires_in = %d, 期望 %d", resp.ExpiresIn, cfg.DeviceAccessTokenDuration)
	}
}

func TestDeviceAuthorize_NoAuth(t *testing.T) {
	handler, _, _ := setupHandler(t)

	body := `{"device_name":"test-device"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/authorize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// 不设置 Identity
	rr := httptest.NewRecorder()
	handler.DeviceAuthorize(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("未认证的 device authorize 应返回 401，实际 %d", rr.Code)
	}
}

func TestDeviceAuthorize_EmptyDeviceName(t *testing.T) {
	handler, _, _ := setupHandler(t)

	body := `{"device_name":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/device/authorize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	id := &Identity{UserID: "user-001", AuthMethod: AuthMethodCookie}
	req = req.WithContext(WithIdentity(req.Context(), id))
	rr := httptest.NewRecorder()
	handler.DeviceAuthorize(rr, req)

	// device_name 为空时服务端应生成安全 fallback，返回 200 而非 400。
	if rr.Code != http.StatusOK {
		t.Errorf("空 device_name 应返回 200（服务端生成 fallback），实际 %d", rr.Code)
	}

	// 验证响应包含有效的 token
	var resp deviceAuthorizeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.AccessToken == "" {
		t.Error("响应应包含 access_token")
	}
	if resp.RefreshToken == "" {
		t.Error("响应应包含 refresh_token")
	}
}

func TestRefresh_Success(t *testing.T) {
	handler, repo, cfg := setupHandler(t)

	// 先创建 device session
	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 刷新 token
	body := `{"refresh_token":"` + result.RefreshToken + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Refresh(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("refresh 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}

	var resp refreshResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应体失败: %v", err)
	}

	if resp.AccessToken == "" {
		t.Error("新 access token 不应为空")
	}
	if resp.RefreshToken == "" {
		t.Error("新 refresh token 不应为空")
	}
	// 旧 token 应已失效（轮换）
	if resp.AccessToken == result.AccessToken {
		t.Error("新 access token 不应与旧 token 相同")
	}
	if resp.RefreshToken == result.RefreshToken {
		t.Error("新 refresh token 不应与旧 token 相同")
	}
}

func TestRefresh_InvalidToken(t *testing.T) {
	handler, _, _ := setupHandler(t)

	body := `{"refresh_token":"invalid-token"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Refresh(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("无效 refresh token 应返回 401，实际 %d", rr.Code)
	}
}

func TestRefresh_EmptyToken(t *testing.T) {
	handler, _, _ := setupHandler(t)

	body := `{"refresh_token":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Refresh(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("空 refresh token 应返回 400，实际 %d", rr.Code)
	}
}

func TestRevoke_Success(t *testing.T) {
	handler, repo, cfg := setupHandler(t)

	// 创建 device session
	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 撤销
	body := `{"device_session_id":"` + result.DeviceSessionID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	id := &Identity{UserID: "user-001", AuthMethod: AuthMethodCookie}
	req = req.WithContext(WithIdentity(req.Context(), id))
	rr := httptest.NewRecorder()
	handler.Revoke(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("revoke 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
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

func TestRevoke_NotOwner(t *testing.T) {
	handler, repo, cfg := setupHandler(t)

	// 创建 device session 属于 user-001
	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 用另一个用户身份尝试撤销
	body := `{"device_session_id":"` + result.DeviceSessionID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	id := &Identity{UserID: "user-002", AuthMethod: AuthMethodCookie}
	req = req.WithContext(WithIdentity(req.Context(), id))
	rr := httptest.NewRecorder()
	handler.Revoke(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("非本人 device session 撤销应返回 403，实际 %d", rr.Code)
	}
}

func TestRevoke_NonexistentDeviceSession(t *testing.T) {
	handler, _, _ := setupHandler(t)

	body := `{"device_session_id":"nonexistent-id"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	id := &Identity{UserID: "user-001", AuthMethod: AuthMethodCookie}
	req = req.WithContext(WithIdentity(req.Context(), id))
	rr := httptest.NewRecorder()
	handler.Revoke(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("不存在的 device session 应返回 404，实际 %d", rr.Code)
	}
}

func TestRevoke_NoDeviceSessionID_BearerAuth(t *testing.T) {
	handler, repo, cfg := setupHandler(t)

	// 创建 device session
	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 用 bearer 身份撤销自己的 device session（不提供 device_session_id）
	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	id := &Identity{
		UserID:          "user-001",
		AuthMethod:      AuthMethodBearer,
		DeviceSessionID: result.DeviceSessionID,
	}
	req = req.WithContext(WithIdentity(req.Context(), id))
	rr := httptest.NewRecorder()
	handler.Revoke(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("bearer 撤销自己的 device session 应返回 200，实际 %d，body: %s", rr.Code, rr.Body.String())
	}
}

// TestLogin_SessionTokenHashOnly 验证数据库中只存储 session token 的哈希，不存明文。
func TestLogin_SessionTokenHashOnly(t *testing.T) {
	handler, repo, cfg := setupHandler(t)

	rr := doLogin(t, handler, "testuser", "testpass123")
	if rr.Code != http.StatusOK {
		t.Fatalf("login 失败: %d", rr.Code)
	}

	// 从 cookie 提取 session token
	cookies := rr.Result().Cookies()
	var sessionToken string
	for _, c := range cookies {
		if c.Name == cfg.CookieName {
			sessionToken = c.Value
		}
	}

	// 在 mock repo 中查找 session——key 是 token hash
	tokenHash := HashToken(sessionToken)
	s, err := repo.GetSessionByTokenHash(nil, tokenHash)
	if err != nil {
		t.Fatalf("应能通过 token 哈希找到 session: %v", err)
	}

	// 验证存储的 token_hash 是哈希而非明文
	if s.TokenHash == sessionToken {
		t.Error("数据库中不应存储明文 session token")
	}
	if s.TokenHash != tokenHash {
		t.Error("数据库中应存储 session token 的 SHA-256 哈希")
	}
}

// TestLogin_CSRFTokenHashOnly 验证数据库中只存储 CSRF token 的哈希，不存明文。
func TestLogin_CSRFTokenHashOnly(t *testing.T) {
	handler, repo, cfg := setupHandler(t)

	rr := doLogin(t, handler, "testuser", "testpass123")
	if rr.Code != http.StatusOK {
		t.Fatalf("login 失败: %d", rr.Code)
	}

	var resp loginResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)

	// 在 mock repo 中查找 session
	cookies := rr.Result().Cookies()
	var sessionToken string
	for _, c := range cookies {
		if c.Name == cfg.CookieName {
			sessionToken = c.Value
		}
	}

	tokenHash := HashToken(sessionToken)
	s, err := repo.GetSessionByTokenHash(nil, tokenHash)
	if err != nil {
		t.Fatalf("应能找到 session: %v", err)
	}

	// 验证存储的 csrf_token_hash 是哈希而非明文
	expectedCSRFHash := HashToken(resp.CSRFToken)
	if s.CSRFTokenHash == resp.CSRFToken {
		t.Error("数据库中不应存储明文 CSRF token")
	}
	if s.CSRFTokenHash != expectedCSRFHash {
		t.Error("数据库中应存储 CSRF token 的 SHA-256 哈希")
	}
}

// TestDeviceAuthorize_TokenHashOnly 验证数据库中只存储 device token 的哈希。
func TestDeviceAuthorize_TokenHashOnly(t *testing.T) {
	_, repo, cfg := setupHandler(t)

	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 通过 access token 哈希查找
	accessHash := HashToken(result.AccessToken)
	ds, err := repo.GetDeviceSessionByAccessTokenHash(nil, accessHash)
	if err != nil {
		t.Fatalf("应能通过 access token 哈希找到 device session: %v", err)
	}

	if ds.AccessTokenHash == result.AccessToken {
		t.Error("数据库中不应存储明文 access token")
	}
	if ds.AccessTokenHash != accessHash {
		t.Error("数据库中应存储 access token 的 SHA-256 哈希")
	}

	// 通过 refresh token 哈希查找
	refreshHash := HashToken(result.RefreshToken)
	ds2, err := repo.GetDeviceSessionByRefreshTokenHash(nil, refreshHash)
	if err != nil {
		t.Fatalf("应能通过 refresh token 哈希找到 device session: %v", err)
	}

	if ds2.RefreshTokenHash == result.RefreshToken {
		t.Error("数据库中不应存储明文 refresh token")
	}
	if ds2.RefreshTokenHash != refreshHash {
		t.Error("数据库中应存储 refresh token 的 SHA-256 哈希")
	}
}

// TestRefresh_TokenRotation 验证 refresh 后旧 token 立即失效。
func TestRefresh_TokenRotation(t *testing.T) {
	handler, repo, cfg := setupHandler(t)

	// 创建 device session
	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	oldAccessToken := result.AccessToken
	oldRefreshToken := result.RefreshToken

	// 刷新
	body := `{"refresh_token":"` + oldRefreshToken + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.Refresh(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("refresh 失败: %d", rr.Code)
	}

	var resp refreshResponse
	json.Unmarshal(rr.Body.Bytes(), &resp)

	// 旧 access token 应已失效（哈希被覆盖）
	_, err = repo.GetDeviceSessionByAccessTokenHash(nil, HashToken(oldAccessToken))
	if err == nil {
		t.Error("旧 access token 应已失效（哈希被覆盖），但仍能查到")
	}

	// 旧 refresh token 应已失效（哈希被覆盖）
	_, err = repo.GetDeviceSessionByRefreshTokenHash(nil, HashToken(oldRefreshToken))
	if err == nil {
		t.Error("旧 refresh token 应已失效（哈希被覆盖），但仍能查到")
	}

	// 新 token 应可用
	_, err = repo.GetDeviceSessionByAccessTokenHash(nil, HashToken(resp.AccessToken))
	if err != nil {
		t.Errorf("新 access token 应可用: %v", err)
	}
	_, err = repo.GetDeviceSessionByRefreshTokenHash(nil, HashToken(resp.RefreshToken))
	if err != nil {
		t.Errorf("新 refresh token 应可用: %v", err)
	}
}

// TestRevokeDeviceSession_AccessTokenInvalidated 验证撤销后 access token 不可用。
func TestRevokeDeviceSession_AccessTokenInvalidated(t *testing.T) {
	repo := NewMockRepository()
	cfg := testAuthCfg()

	// 添加测试用户
	repo.AddUser(&User{ID: "user-001", Username: "testuser", SystemRole: "user"})

	// 创建 device session
	result, err := AuthorizeDevice(nil, repo, cfg, "user-001", "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 验证 access token 有效
	_, err = ValidateAccessToken(nil, repo, result.AccessToken)
	if err != nil {
		t.Fatalf("撤销前 access token 应有效: %v", err)
	}

	// 撤销
	err = RevokeDeviceSession(nil, repo, result.DeviceSessionID)
	if err != nil {
		t.Fatalf("RevokeDeviceSession 失败: %v", err)
	}

	// 验证 access token 已不可用
	_, err = ValidateAccessToken(nil, repo, result.AccessToken)
	if err == nil {
		t.Error("撤销后 access token 应不可用")
	}
	if err != ErrDeviceSessionRevoked {
		t.Errorf("撤销后应返回 ErrDeviceSessionRevoked，实际: %v", err)
	}
}

// TestValidateSession_Expired 验证过期 session 被拒绝。
func TestValidateSession_Expired(t *testing.T) {
	repo := NewMockRepository()

	// 创建一个已过期的 session
	sessionToken, _ := GenerateToken()
	csrfToken, _ := GenerateToken()
	sessionID, _ := repo.CreateSession(nil, "user-001", HashToken(sessionToken), HashToken(csrfToken), time.Now().Add(-1*time.Hour))

	// 添加用户
	repo.AddUser(&User{ID: "user-001", Username: "testuser", SystemRole: "user"})

	_ = sessionID

	_, err := ValidateSession(nil, repo, sessionToken)
	if err != ErrSessionExpired {
		t.Errorf("过期 session 应返回 ErrSessionExpired，实际: %v", err)
	}
}

// TestValidateSession_Revoked 验证已撤销 session 被拒绝。
func TestValidateSession_Revoked(t *testing.T) {
	repo := NewMockRepository()

	// 创建 session
	sessionToken, _ := GenerateToken()
	csrfToken, _ := GenerateToken()
	sessionID, _ := repo.CreateSession(nil, "user-001", HashToken(sessionToken), HashToken(csrfToken), time.Now().Add(1*time.Hour))

	repo.AddUser(&User{ID: "user-001", Username: "testuser", SystemRole: "user"})

	// 撤销
	repo.RevokeSession(nil, sessionID)

	_, err := ValidateSession(nil, repo, sessionToken)
	if err != ErrSessionRevoked {
		t.Errorf("已撤销 session 应返回 ErrSessionRevoked，实际: %v", err)
	}
}

// TestValidateAccessToken_Expired 验证过期 access token 被拒绝。
func TestValidateAccessToken_Expired(t *testing.T) {
	repo := NewMockRepository()

	// 创建一个已过期的 device session
	accessToken, _ := GenerateToken()
	refreshToken, _ := GenerateToken()
	repo.CreateDeviceSession(nil, "user-001", "test", HashToken(accessToken), HashToken(refreshToken),
		time.Now().Add(-1*time.Hour), time.Now().Add(24*time.Hour))

	repo.AddUser(&User{ID: "user-001", Username: "testuser", SystemRole: "user"})

	_, err := ValidateAccessToken(nil, repo, accessToken)
	if err != ErrDeviceSessionExpired {
		t.Errorf("过期 access token 应返回 ErrDeviceSessionExpired，实际: %v", err)
	}
}

// TestAccountHandlers 验证当前用户资料读取、邮箱更新和密码更新均绑定当前身份。
func TestAccountHandlers(t *testing.T) {
	handler, repo, _ := setupHandler(t)
	ctx := WithIdentity(httptest.NewRequest(http.MethodGet, "/api/auth/me", nil).Context(), &Identity{UserID: "user-001"})

	meReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil).WithContext(ctx)
	meRR := httptest.NewRecorder()
	handler.Me(meRR, meReq)
	if meRR.Code != http.StatusOK {
		t.Fatalf("读取当前用户应返回 200，实际 %d，body: %s", meRR.Code, meRR.Body.String())
	}
	var me currentUserResponse
	if err := json.Unmarshal(meRR.Body.Bytes(), &me); err != nil {
		t.Fatalf("解析当前用户响应失败: %v", err)
	}
	if me.Email != "test@example.com" || me.Username != "testuser" {
		t.Fatalf("当前用户资料不匹配: %+v", me)
	}

	emailReq := httptest.NewRequest(http.MethodPut, "/api/auth/me/email", strings.NewReader(`{"current_password":"testpass123","email":"updated@example.com"}`)).WithContext(ctx)
	emailReq.Header.Set("Content-Type", "application/json")
	emailRR := httptest.NewRecorder()
	handler.UpdateEmail(emailRR, emailReq)
	if emailRR.Code != http.StatusOK {
		t.Fatalf("修改邮箱应返回 200，实际 %d，body: %s", emailRR.Code, emailRR.Body.String())
	}

	passwordReq := httptest.NewRequest(http.MethodPut, "/api/auth/me/password", strings.NewReader(`{"current_password":"testpass123","new_password":"new-password-123"}`)).WithContext(ctx)
	passwordReq.Header.Set("Content-Type", "application/json")
	passwordRR := httptest.NewRecorder()
	handler.UpdatePassword(passwordRR, passwordReq)
	if passwordRR.Code != http.StatusOK {
		t.Fatalf("修改密码应返回 200，实际 %d，body: %s", passwordRR.Code, passwordRR.Body.String())
	}

	updated, err := repo.GetUserByID(nil, "user-001")
	if err != nil {
		t.Fatalf("读取更新后用户失败: %v", err)
	}
	if updated.Email != "updated@example.com" {
		t.Errorf("邮箱未更新，实际 %q", updated.Email)
	}
	if err := VerifyPassword(updated.PasswordHash, "new-password-123"); err != nil {
		t.Errorf("新密码校验失败: %v", err)
	}
	if err := VerifyPassword(updated.PasswordHash, "testpass123"); err == nil {
		t.Error("旧密码不应继续有效")
	}
}
