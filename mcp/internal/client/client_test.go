// client_test.go 测试 HTTPS REST 客户端。
//
// 引入动机：design/06-IMPLEMENTATION.md Phase 4 验收要求：
//   - fake HTTPS server 验证 MCP 实际调用 REST
//   - bearer token 正确注入
//   - 401 自动 refresh
//   - 错误映射为 APIError
package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockCredStore 是测试用的凭据存储。
type mockCredStore struct {
	tokens *CredentialTokens
	err    error
}

func (m *mockCredStore) Load(serverURL string) (*CredentialTokens, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.tokens == nil {
		return &CredentialTokens{}, nil
	}
	return m.tokens, nil
}

func (m *mockCredStore) Save(serverURL string, tokens *CredentialTokens) error {
	m.tokens = tokens
	return nil
}

func (m *mockCredStore) Delete(serverURL string) error {
	m.tokens = nil
	return nil
}

func newTestClient(t *testing.T, server *httptest.Server, tokens *CredentialTokens) *Client {
	t.Helper()
	cred := &mockCredStore{tokens: tokens}
	cli := NewClient(server.URL, cred, true)
	if err := cli.LoadTokens(); err != nil {
		t.Fatalf("LoadTokens 失败: %v", err)
	}
	return cli
}

func TestGetWithBearerToken(t *testing.T) {
	var receivedAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	cli := newTestClient(t, server, &CredentialTokens{
		AccessToken:  "my-access-token",
		RefreshToken: "my-refresh-token",
	})

	var result map[string]interface{}
	err := cli.Get(t.Context(), "/api/test", &result)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}

	if receivedAuth != "Bearer my-access-token" {
		t.Errorf("Authorization = %s, 期望 Bearer my-access-token", receivedAuth)
	}
}

func TestGetUnauthorizedTriggersRefresh(t *testing.T) {
	requestCount := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.URL.Path == "/api/auth/refresh" {
			// refresh 端点
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"access_token":  "new-access-token",
				"refresh_token": "new-refresh-token",
			})
			return
		}
		if requestCount == 1 {
			// 第一次请求——401
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"token expired"}`))
			return
		}
		// 重试请求——验证使用新 token
		if r.Header.Get("Authorization") != "Bearer new-access-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	cli := newTestClient(t, server, &CredentialTokens{
		AccessToken:  "expired-token",
		RefreshToken: "my-refresh-token",
	})

	var result map[string]interface{}
	err := cli.Get(t.Context(), "/api/test", &result)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}

	if requestCount < 3 {
		t.Errorf("期望至少 3 个请求（401 + refresh + retry），得到 %d", requestCount)
	}
}

func TestAPIErrorMapping(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"document not found"}`))
	}))
	defer server.Close()

	cli := newTestClient(t, server, &CredentialTokens{
		AccessToken:  "token",
		RefreshToken: "refresh",
	})

	var result map[string]interface{}
	err := cli.Get(t.Context(), "/api/test", &result)
	if err == nil {
		t.Fatal("期望错误")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("错误类型应为 APIError, 得到 %T", err)
	}
	if !apiErr.IsNotFound() {
		t.Errorf("期望 404, 得到 %d", apiErr.StatusCode)
	}
	if apiErr.Message != "document not found" {
		t.Errorf("Message = %s", apiErr.Message)
	}
}

func TestAPIErrorConflict(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"version conflict"}`))
	}))
	defer server.Close()

	cli := newTestClient(t, server, &CredentialTokens{
		AccessToken:  "token",
		RefreshToken: "refresh",
	})

	var result map[string]interface{}
	err := cli.Patch(t.Context(), "/api/test", map[string]string{"key": "val"}, &result)
	if err == nil {
		t.Fatal("期望错误")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("错误类型应为 APIError, 得到 %T", err)
	}
	if !apiErr.IsConflict() {
		t.Errorf("期望 409, 得到 %d", apiErr.StatusCode)
	}
}

func TestAPIErrorNonJSONBody(t *testing.T) {
	// 验证 error body 非 JSON 时使用安全 fallback 错误信息。
	// 引入动机：design 要求不可静默丢错，error body 非 JSON 时必须保留 fallback 错误。
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Server Error plain text"))
	}))
	defer server.Close()

	cli := newTestClient(t, server, &CredentialTokens{
		AccessToken:  "token",
		RefreshToken: "refresh",
	})

	var result map[string]interface{}
	err := cli.Get(t.Context(), "/api/test", &result)
	if err == nil {
		t.Fatal("期望错误")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("错误类型应为 APIError, 得到 %T", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, 期望 500", apiErr.StatusCode)
	}
	// 应有 fallback 消息（HTTP 状态码），不因 JSON 解析失败而静默
	if apiErr.Message == "" {
		t.Error("非 JSON error body 应有 fallback 消息，不应为空")
	}
	if apiErr.Message != "HTTP 500" {
		t.Errorf("Message = %s, 期望 'HTTP 500'", apiErr.Message)
	}
}

func TestNotLoggedIn(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cred := &mockCredStore{tokens: &CredentialTokens{}}
	cli := NewClient(server.URL, cred, true)
	_ = cli.LoadTokens()

	var result map[string]interface{}
	err := cli.Get(t.Context(), "/api/test", &result)
	if err == nil {
		t.Fatal("期望未登录错误")
	}
}

func TestDeviceAuthStart(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/device/start" {
			t.Errorf("Path = %s, 期望 /api/auth/device/start", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("Method = %s, 期望 POST", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"device_code":       "dc-123",
			"user_code":         "UC-456",
			"verification_url":  "https://example.com/authorize",
			"expires_in":        900,
			"interval":          5,
		})
	}))
	defer server.Close()

	cred := &mockCredStore{}
	cli := NewClient(server.URL, cred, true)

	resp, err := cli.DeviceAuthStart(t.Context(), "my-device")
	if err != nil {
		t.Fatalf("DeviceAuthStart 失败: %v", err)
	}
	if resp.DeviceCode != "dc-123" {
		t.Errorf("DeviceCode = %s", resp.DeviceCode)
	}
	if resp.UserCode != "UC-456" {
		t.Errorf("UserCode = %s", resp.UserCode)
	}
}

func TestDeviceAuthPollAuthorized(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/device/poll" {
			t.Errorf("Path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":         "authorized",
			"access_token":   "new-access",
			"refresh_token":  "new-refresh",
			"token_type":     "Bearer",
			"expires_in":     3600,
		})
	}))
	defer server.Close()

	cred := &mockCredStore{}
	cli := NewClient(server.URL, cred, true)

	resp, err := cli.DeviceAuthPoll(t.Context(), "dc-123")
	if err != nil {
		t.Fatalf("DeviceAuthPoll 失败: %v", err)
	}
	if resp.Status != "authorized" {
		t.Errorf("Status = %s", resp.Status)
	}
	if resp.AccessToken != "new-access" {
		t.Errorf("AccessToken = %s", resp.AccessToken)
	}
}

func TestDeviceAuthPollPending(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusPending)
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	}))
	defer server.Close()

	cred := &mockCredStore{}
	cli := NewClient(server.URL, cred, true)

	_, err := cli.DeviceAuthPoll(t.Context(), "dc-123")
	if err != ErrPendingAuthorization {
		t.Errorf("期望 ErrPendingAuthorization, 得到 %v", err)
	}
}

func TestDeviceAuthPollDenied(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"status": "denied"})
	}))
	defer server.Close()

	cred := &mockCredStore{}
	cli := NewClient(server.URL, cred, true)

	_, err := cli.DeviceAuthPoll(t.Context(), "dc-123")
	if err != ErrAuthorizationDenied {
		t.Errorf("期望 ErrAuthorizationDenied, 得到 %v", err)
	}
}

func TestDeviceAuthPollExpired(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		json.NewEncoder(w).Encode(map[string]string{"status": "expired"})
	}))
	defer server.Close()

	cred := &mockCredStore{}
	cli := NewClient(server.URL, cred, true)

	_, err := cli.DeviceAuthPoll(t.Context(), "dc-123")
	if err != ErrDeviceCodeExpired {
		t.Errorf("期望 ErrDeviceCodeExpired, 得到 %v", err)
	}
}

func TestRevokeSession(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/revoke" {
			t.Errorf("Path = %s, 期望 /api/auth/revoke", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("Method = %s, 期望 POST", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	cli := newTestClient(t, server, &CredentialTokens{
		AccessToken:  "token-to-revoke",
		RefreshToken: "refresh",
	})

	err := cli.RevokeSession(t.Context())
	if err != nil {
		t.Fatalf("RevokeSession 失败: %v", err)
	}
}

func TestRevokeSessionServerError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer server.Close()

	cli := newTestClient(t, server, &CredentialTokens{
		AccessToken:  "token",
		RefreshToken: "refresh",
	})

	err := cli.RevokeSession(t.Context())
	if err == nil {
		t.Fatal("期望错误")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("错误类型应为 APIError, 得到 %T", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, 期望 500", apiErr.StatusCode)
	}
}
