// Package bootstrap 的测试覆盖 Bootstrap 状态查询、首次管理员创建、单次性保证。
//
// 引入动机：计划要求真实行为测试——Bootstrap 单次性、RBAC、审计脱敏。
// 禁止源码字符串包含测试，所有测试调用真实 Bootstrap 逻辑验证。
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"partitura/server/internal/auth"
)

// mockFirstAdminCreator 是 FirstAdminCreator 接口的内存 mock。
// 引入动机：单元测试需要模拟原子创建首个管理员的行为。
// 使用 mutex 保证并发安全：mutex 持有期间检查 + 插入是原子的。
type mockFirstAdminCreator struct {
	mu      sync.Mutex
	users   map[string]bool // key: username
	nextID  int
	createErr error
}

func newMockFirstAdminCreator() *mockFirstAdminCreator {
	return &mockFirstAdminCreator{users: make(map[string]bool)}
}

// CreateUserIfNoneExist 原子地检查 users 表是否为空，若为空则插入用户。
// 引入动机：模拟 PGRepository.CreateUserIfNoneExist 的行为。
func (m *mockFirstAdminCreator) CreateUserIfNoneExist(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return "", m.createErr
	}
	if len(m.users) > 0 {
		return "", auth.ErrUsersAlreadyExist
	}
	m.nextID++
	id := fmt.Sprintf("user-%d", m.nextID)
	m.users[username] = true
	return id, nil
}

// CountUsers 实现 UserCounter 接口。
// 引入动机：Bootstrap 状态查询端点需要 UserCounter。
type mockUserCounter struct {
	mu    sync.Mutex
	users map[string]bool
}

func newMockUserCounter() *mockUserCounter {
	return &mockUserCounter{users: make(map[string]bool)}
}

func (m *mockUserCounter) CountUsers(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

// mockAuditRepo 是 AuditRepository 接口的内存 mock。
type mockAuditRepo struct {
	mu      sync.Mutex
	entries []mockAuditEntry
}

type mockAuditEntry struct {
	userID       string
	action       string
	resourceType string
	resourceID   string
	detail       json.RawMessage
}

func newMockAuditRepo() *mockAuditRepo {
	return &mockAuditRepo{}
}

func (m *mockAuditRepo) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, mockAuditEntry{
		userID:       userID,
		action:       action,
		resourceType: resourceType,
		resourceID:   resourceID,
		detail:       detail,
	})
	return nil
}

// makeAuthCfg 返回测试用 AuthConfig（Argon2 参数使用最小值以加速测试）。
func makeAuthCfg() auth.AuthConfig {
	return auth.AuthConfig{
		Argon2Memory:      32,
		Argon2Iterations:  1,
		Argon2Parallelism: 1,
		Argon2SaltLength:  8,
		Argon2KeyLength:   16,
	}
}

// combinedMock 同时实现 FirstAdminCreator 和 UserCounter 接口。
// 引入动机：简化测试中 Handler 的构造，使用同一个 mock 同时满足两个接口。
type combinedMock struct {
	*mockFirstAdminCreator
	*mockUserCounter
}

func newCombinedMock() *combinedMock {
	fac := newMockFirstAdminCreator()
	uc := newMockUserCounter()
	return &combinedMock{
		mockFirstAdminCreator: fac,
		mockUserCounter:       uc,
	}
}

// syncMock 是线程安全的 combinedMock，FirstAdminCreator 和 UserCounter 共享同一数据。
// 引入动机：并发测试需要 FirstAdminCreator 和 UserCounter 操作同一状态。
type syncMock struct {
	mu    sync.Mutex
	users map[string]bool
	nextID int
}

func newSyncMock() *syncMock {
	return &syncMock{users: make(map[string]bool)}
}

func (m *syncMock) CreateUserIfNoneExist(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > 0 {
		return "", auth.ErrUsersAlreadyExist
	}
	m.nextID++
	id := fmt.Sprintf("user-%d", m.nextID)
	m.users[username] = true
	return id, nil
}

func (m *syncMock) CountUsers(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

func TestGetBootstrapStatus_EmptyDB(t *testing.T) {
	mock := newCombinedMock()
	auditRepo := newMockAuditRepo()
	handler := NewHandler(mock, mock, auditRepo, makeAuthCfg())

	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	w := httptest.NewRecorder()
	handler.GetBootstrapStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if resp["bootstrap_available"] != true {
		t.Fatalf("空数据库 bootstrap_available 应为 true, got %v", resp["bootstrap_available"])
	}
}

func TestGetBootstrapStatus_NonEmptyDB(t *testing.T) {
	mock := newCombinedMock()
	mock.mockUserCounter.users["existing-admin"] = true
	auditRepo := newMockAuditRepo()
	handler := NewHandler(mock, mock, auditRepo, makeAuthCfg())

	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	w := httptest.NewRecorder()
	handler.GetBootstrapStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	if resp["bootstrap_available"] != false {
		t.Fatalf("非空数据库 bootstrap_available 应为 false, got %v", resp["bootstrap_available"])
	}
}

func TestCreateFirstAdmin_Success(t *testing.T) {
	mock := newCombinedMock()
	auditRepo := newMockAuditRepo()
	handler := NewHandler(mock, mock, auditRepo, makeAuthCfg())

	body := `{"username":"admin","email":"admin@example.com","password":"secure-password-123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.CreateFirstAdmin(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d, want %d, body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp bootstrapResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Status != "created" {
		t.Fatalf("status = %q, want %q", resp.Status, "created")
	}
	if resp.UserID == "" {
		t.Fatal("user_id 不应为空")
	}

	// 验证审计记录已写入
	if len(auditRepo.entries) != 1 {
		t.Fatalf("审计记录数 = %d, want 1", len(auditRepo.entries))
	}
	entry := auditRepo.entries[0]
	if entry.action != "bootstrap.create_admin" {
		t.Fatalf("审计 action = %q, want %q", entry.action, "bootstrap.create_admin")
	}

	// 验证审计 detail 不含密码
	var detail map[string]string
	if err := json.Unmarshal(entry.detail, &detail); err != nil {
		t.Fatalf("解析审计 detail 失败: %v", err)
	}
	if _, hasPassword := detail["password"]; hasPassword {
		t.Fatal("审计 detail 不应包含 password 字段")
	}
	if detail["username"] != "admin" {
		t.Fatalf("审计 username = %q, want %q", detail["username"], "admin")
	}
}

func TestCreateFirstAdmin_AlreadyHasUsers(t *testing.T) {
	mock := newCombinedMock()
	mock.mockFirstAdminCreator.users["existing"] = true
	mock.mockUserCounter.users["existing"] = true
	handler := NewHandler(mock, mock, newMockAuditRepo(), makeAuthCfg())

	body := `{"username":"admin","email":"admin@example.com","password":"secure-password-123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.CreateFirstAdmin(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusForbidden)
	}

	// 验证用户未被创建
	if len(mock.mockFirstAdminCreator.users) != 1 {
		t.Fatalf("用户数 = %d, want 1（仅原有用户）", len(mock.mockFirstAdminCreator.users))
	}
}

func TestCreateFirstAdmin_SingleUse(t *testing.T) {
	mock := newCombinedMock()
	auditRepo := newMockAuditRepo()
	handler := NewHandler(mock, mock, auditRepo, makeAuthCfg())

	// 第一次创建应成功
	body := `{"username":"admin","email":"admin@example.com","password":"secure-password-123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.CreateFirstAdmin(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("第一次创建状态码 = %d, want %d", w.Code, http.StatusCreated)
	}

	// 第二次创建应被拒绝（CreateUserIfNoneExist 返回 ErrUsersAlreadyExist）
	body2 := `{"username":"attacker","email":"attacker@example.com","password":"secure-password-123"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	handler.CreateFirstAdmin(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("第二次创建状态码 = %d, want %d", w2.Code, http.StatusForbidden)
	}

	// 验证只有第一个用户被创建
	if len(mock.mockFirstAdminCreator.users) != 1 {
		t.Fatalf("用户数 = %d, want 1", len(mock.mockFirstAdminCreator.users))
	}
	if !mock.mockFirstAdminCreator.users["admin"] {
		t.Fatal("admin 用户应存在")
	}
	if mock.mockFirstAdminCreator.users["attacker"] {
		t.Fatal("attacker 用户不应被创建")
	}
}

func TestCreateFirstAdmin_ValidationErrors(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		wantStatus      int
		wantErrContains string
	}{
		{"empty username", `{"username":"","email":"a@b.com","password":"secure-password-123"}`, http.StatusBadRequest, "username"},
		{"short username", `{"username":"ab","email":"a@b.com","password":"secure-password-123"}`, http.StatusBadRequest, "username"},
		{"empty email", `{"username":"admin","email":"","password":"secure-password-123"}`, http.StatusBadRequest, "email"},
		{"invalid email", `{"username":"admin","email":"not-an-email","password":"secure-password-123"}`, http.StatusBadRequest, "email"},
		{"short password", `{"username":"admin","email":"a@b.com","password":"short"}`, http.StatusBadRequest, "password"},
		{"unknown field", `{"username":"admin","email":"a@b.com","password":"secure-password-123","extra":"field"}`, http.StatusBadRequest, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := newCombinedMock()
			handler := NewHandler(mock, mock, newMockAuditRepo(), makeAuthCfg())
			req := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			handler.CreateFirstAdmin(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("状态码 = %d, want %d, body: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantErrContains != "" {
				var resp map[string]string
				_ = json.NewDecoder(w.Body).Decode(&resp)
				if !strings.Contains(resp["error"], tt.wantErrContains) {
					t.Fatalf("错误信息应包含 %q, got %q", tt.wantErrContains, resp["error"])
				}
			}
			// 验证用户未被创建
			if len(mock.mockFirstAdminCreator.users) != 0 {
				t.Fatalf("校验失败时不应创建用户, got %d users", len(mock.mockFirstAdminCreator.users))
			}
		})
	}
}

func TestCreateFirstAdmin_MethodNotAllowed(t *testing.T) {
	mock := newCombinedMock()
	handler := NewHandler(mock, mock, newMockAuditRepo(), makeAuthCfg())
	req := httptest.NewRequest(http.MethodPut, "/api/bootstrap", nil)
	w := httptest.NewRecorder()
	handler.CreateFirstAdmin(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestEnsureNoUsers_Empty(t *testing.T) {
	mock := newCombinedMock()
	empty, err := EnsureNoUsers(context.Background(), mock)
	if err != nil {
		t.Fatalf("EnsureNoUsers 失败: %v", err)
	}
	if !empty {
		t.Fatal("空数据库应返回 true")
	}
}

func TestEnsureNoUsers_NonEmpty(t *testing.T) {
	mock := newCombinedMock()
	mock.mockUserCounter.users["someone"] = true
	empty, err := EnsureNoUsers(context.Background(), mock)
	if err != nil {
		t.Fatalf("EnsureNoUsers 失败: %v", err)
	}
	if empty {
		t.Fatal("非空数据库应返回 false")
	}
}

// TestCreateFirstAdmin_ConcurrentSingleUse 验证并发请求下只有一个成功创建管理员。
// 引入动机：这是 P0 竞态修复的核心验证——多个并发 Bootstrap 请求中最多一个成功。
// 使用 syncMock 模拟原子操作，mutex 保证检查+插入的原子性。
func TestCreateFirstAdmin_ConcurrentSingleUse(t *testing.T) {
	const concurrency = 20
	mock := newSyncMock()
	auditRepo := newMockAuditRepo()
	handler := NewHandler(mock, mock, auditRepo, makeAuthCfg())

	var wg sync.WaitGroup
	wg.Add(concurrency)

	results := make([]struct {
		status int
		body   string
	}, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"username":"admin_%d","email":"admin_%d@example.com","password":"secure-password-123"}`, idx, idx)
			req := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(body))
			w := httptest.NewRecorder()
			handler.CreateFirstAdmin(w, req)
			results[idx].status = w.Code
			results[idx].body = w.Body.String()
		}(i)
	}
	wg.Wait()

	successCount := 0
	forbiddenCount := 0
	for _, r := range results {
		switch r.status {
		case http.StatusCreated:
			successCount++
		case http.StatusForbidden:
			forbiddenCount++
		default:
			t.Errorf("意外状态码 %d, body: %s", r.status, r.body)
		}
	}

	if successCount != 1 {
		t.Fatalf("并发创建中成功数 = %d, want 1", successCount)
	}
	if forbiddenCount != concurrency-1 {
		t.Fatalf("并发创建中被拒绝数 = %d, want %d", forbiddenCount, concurrency-1)
	}

	// 验证最终只有一个用户
	if len(mock.users) != 1 {
		t.Fatalf("最终用户数 = %d, want 1", len(mock.users))
	}

	// 验证审计记录只有一条
	if len(auditRepo.entries) != 1 {
		t.Fatalf("审计记录数 = %d, want 1", len(auditRepo.entries))
	}
}

// TestCreateFirstAdmin_ConcurrentSingleUse_WithCreatorError 验证当 Creator 返回非 ErrUsersAlreadyExist 错误时返回 500。
func TestCreateFirstAdmin_ConcurrentSingleUse_WithCreatorError(t *testing.T) {
	mock := newCombinedMock()
	mock.mockFirstAdminCreator.createErr = errors.New("数据库连接失败")
	handler := NewHandler(mock, mock, newMockAuditRepo(), makeAuthCfg())

	body := `{"username":"admin","email":"admin@example.com","password":"secure-password-123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.CreateFirstAdmin(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want %d, body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

// 确保 sql 包被使用（用于 sql.ErrNoRows 检查在 EnsureNoUsers 中）
var _ = sql.ErrNoRows
