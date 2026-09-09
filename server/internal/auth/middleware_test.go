// middleware_test.go 测试认证 middleware 的行为。
//
// 测试覆盖：
//   - Cookie session 认证：有效 session 设置 Identity
//   - Cookie session 认证：无效 session 不设置 Identity
//   - Bearer token 认证：有效 token 设置 Identity
//   - Bearer token 认证：无效 token 不设置 Identity
//   - Bearer 优先于 cookie
//   - RequireAuth：未认证返回 401
//   - RequireAuth：已认证放行
//   - CSRF：GET 请求不需要 CSRF
//   - CSRF：POST 请求缺少 CSRF token 返回 403
//   - CSRF：POST 请求错误 CSRF token 返回 403
//   - CSRF：POST 请求正确 CSRF token 放行
//   - CSRF：Bearer 认证不需要 CSRF
package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// setupMiddleware 创建用于测试的 middleware 链和 mock repository。
func setupMiddleware(t *testing.T) (Repository, AuthConfig) {
	t.Helper()
	cfg := testAuthCfg()
	repo := NewMockRepository()

	// 添加测试用户
	repo.AddUser(&User{
		ID:         "user-001",
		Username:   "testuser",
		SystemRole: "user",
	})

	return repo, cfg
}

// createSession 在 mock repo 中创建一个 session 并返回明文 token。
func createSession(t *testing.T, repo Repository, cfg AuthConfig) (sessionToken, csrfToken, sessionID string) {
	t.Helper()
	sessionToken, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken 失败: %v", err)
	}
	csrfToken, err = GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken 失败: %v", err)
	}
	sessionID, err = repo.CreateSession(context.Background(), "user-001", HashToken(sessionToken), HashToken(csrfToken), time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("CreateSession 失败: %v", err)
	}
	return
}

func TestAuthMiddleware_CookieSession_Valid(t *testing.T) {
	repo, cfg := setupMiddleware(t)
	sessionToken, _, _ := createSession(t, repo, cfg)

	mw := AuthMiddleware(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id == nil {
			t.Error("应设置 Identity")
		} else {
			if id.UserID != "user-001" {
				t.Errorf("UserID = %q, 期望 user-001", id.UserID)
			}
			if id.AuthMethod != AuthMethodCookie {
				t.Errorf("AuthMethod = %q, 期望 cookie", id.AuthMethod)
			}
		}
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("handler 应被调用")
	}
}

func TestAuthMiddleware_CookieSession_Invalid(t *testing.T) {
	repo, cfg := setupMiddleware(t)

	mw := AuthMiddleware(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id != nil {
			t.Error("无效 session 不应设置 Identity")
		}
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: "invalid-token"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("handler 应被调用（即使 session 无效）")
	}
}

func TestAuthMiddleware_BearerToken_Valid(t *testing.T) {
	repo, cfg := setupMiddleware(t)

	// 创建 device session
	accessToken, _ := GenerateToken()
	refreshToken, _ := GenerateToken()
	repo.CreateDeviceSession(context.Background(), "user-001", "test", HashToken(accessToken), HashToken(refreshToken),
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour))

	mw := AuthMiddleware(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id == nil {
			t.Error("应设置 Identity")
		} else {
			if id.UserID != "user-001" {
				t.Errorf("UserID = %q, 期望 user-001", id.UserID)
			}
			if id.AuthMethod != AuthMethodBearer {
				t.Errorf("AuthMethod = %q, 期望 bearer", id.AuthMethod)
			}
		}
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("handler 应被调用")
	}
}

func TestAuthMiddleware_BearerToken_Invalid(t *testing.T) {
	repo, cfg := setupMiddleware(t)

	mw := AuthMiddleware(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id != nil {
			t.Error("无效 bearer token 不应设置 Identity")
		}
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("handler 应被调用")
	}
}

func TestAuthMiddleware_BearerTakesPrecedence(t *testing.T) {
	repo, cfg := setupMiddleware(t)

	// 创建 cookie session
	sessionToken, _, _ := createSession(t, repo, cfg)

	// 创建 device session
	accessToken, _ := GenerateToken()
	refreshToken, _ := GenerateToken()
	repo.CreateDeviceSession(context.Background(), "user-001", "test", HashToken(accessToken), HashToken(refreshToken),
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour))

	mw := AuthMiddleware(repo, cfg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id == nil {
			t.Fatal("应设置 Identity")
		}
		if id.AuthMethod != AuthMethodBearer {
			t.Errorf("Bearer 应优先于 cookie，AuthMethod = %q", id.AuthMethod)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
}

func TestAuthMiddleware_NoCredentials(t *testing.T) {
	repo, cfg := setupMiddleware(t)

	mw := AuthMiddleware(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id != nil {
			t.Error("无凭据不应设置 Identity")
		}
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("handler 应被调用")
	}
}

func TestRequireAuth_Unauthenticated(t *testing.T) {
	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("未认证请求不应到达 handler")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("未认证应返回 401，实际 %d", rr.Code)
	}
}

func TestRequireAuth_Authenticated(t *testing.T) {
	handler := RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id == nil {
			t.Error("已认证请求应有 Identity")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(WithIdentity(req.Context(), &Identity{UserID: "user-001", AuthMethod: AuthMethodCookie}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("已认证应返回 200，实际 %d", rr.Code)
	}
}

func TestRequireCSRF_GetRequest_NoCSRFNeeded(t *testing.T) {
	repo, cfg := setupMiddleware(t)
	sessionToken, _, _ := createSession(t, repo, cfg)

	mw := RequireCSRF(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	// 不设置 CSRF header
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("GET 请求不需要 CSRF，handler 应被调用")
	}
}

func TestRequireCSRF_PostRequest_MissingCSRF(t *testing.T) {
	repo, cfg := setupMiddleware(t)
	sessionToken, _, _ := createSession(t, repo, cfg)

	mw := RequireCSRF(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	// 不设置 CSRF header
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if called {
		t.Error("缺少 CSRF token 的 POST 应被拒绝")
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("缺少 CSRF token 应返回 403，实际 %d", rr.Code)
	}
}

func TestRequireCSRF_PostRequest_WrongCSRF(t *testing.T) {
	repo, cfg := setupMiddleware(t)
	sessionToken, _, _ := createSession(t, repo, cfg)

	mw := RequireCSRF(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.Header.Set(cfg.CSRFHeaderName, "wrong-csrf-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if called {
		t.Error("错误 CSRF token 的 POST 应被拒绝")
	}
	if rr.Code != http.StatusForbidden {
		t.Errorf("错误 CSRF token 应返回 403，实际 %d", rr.Code)
	}
}

func TestRequireCSRF_PostRequest_CorrectCSRF(t *testing.T) {
	repo, cfg := setupMiddleware(t)
	sessionToken, csrfToken, _ := createSession(t, repo, cfg)

	mw := RequireCSRF(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	req.Header.Set(cfg.CSRFHeaderName, csrfToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("正确 CSRF token 的 POST 应放行")
	}
}

func TestRequireCSRF_BearerAuth_NoCSRFNeeded(t *testing.T) {
	repo, cfg := setupMiddleware(t)

	mw := RequireCSRF(repo, cfg)
	called := false
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	// Bearer 认证不需要 CSRF
	id := &Identity{UserID: "user-001", AuthMethod: AuthMethodBearer}
	req = req.WithContext(WithIdentity(req.Context(), id))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Error("Bearer 认证的 POST 不需要 CSRF，handler 应被调用")
	}
}

// TestAuthMiddleware_IdentityInjection 验证 cookie session middleware 正确注入身份信息。
func TestAuthMiddleware_IdentityInjection(t *testing.T) {
	repo, cfg := setupMiddleware(t)
	sessionToken, _, _ := createSession(t, repo, cfg)

	mw := AuthMiddleware(repo, cfg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id == nil {
			t.Fatal("应设置 Identity")
		}
		if id.UserID != "user-001" {
			t.Errorf("UserID = %q, 期望 user-001", id.UserID)
		}
		if id.Username != "testuser" {
			t.Errorf("Username = %q, 期望 testuser", id.Username)
		}
		if id.SystemRole != "user" {
			t.Errorf("SystemRole = %q, 期望 user", id.SystemRole)
		}
		if id.SessionID == "" {
			t.Error("SessionID 不应为空")
		}
		if id.AuthMethod != AuthMethodCookie {
			t.Errorf("AuthMethod = %q, 期望 cookie", id.AuthMethod)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cfg.CookieName, Value: sessionToken})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
}

// TestAuthMiddleware_BearerIdentityInjection 验证 bearer token middleware 正确注入身份信息。
func TestAuthMiddleware_BearerIdentityInjection(t *testing.T) {
	repo, cfg := setupMiddleware(t)

	accessToken, _ := GenerateToken()
	refreshToken, _ := GenerateToken()
	repo.CreateDeviceSession(context.Background(), "user-001", "test", HashToken(accessToken), HashToken(refreshToken),
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour))

	mw := AuthMiddleware(repo, cfg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id == nil {
			t.Fatal("应设置 Identity")
		}
		if id.UserID != "user-001" {
			t.Errorf("UserID = %q, 期望 user-001", id.UserID)
		}
		if id.DeviceSessionID == "" {
			t.Error("DeviceSessionID 不应为空")
		}
		if id.AuthMethod != AuthMethodBearer {
			t.Errorf("AuthMethod = %q, 期望 bearer", id.AuthMethod)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
}
