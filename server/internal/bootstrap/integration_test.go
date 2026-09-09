// integration_test.go 测试 Bootstrap 模块的 PostgreSQL 集成，重点验证并发安全。
//
// 测试覆盖：
//   - 空数据库 Bootstrap 成功创建 system_admin
//   - 非空数据库 Bootstrap 永久拒绝
//   - 并发 Bootstrap 请求中最多一个成功（P0 竞态修复验证）
//   - 审计记录脱敏（不含密码）
//   - 创建的用户可被 auth.Repository.GetUserByUsername 查询到
//
// 运行条件：设置 TEST_DATABASE_URL 环境变量指向可用的 PostgreSQL 实例。
// 未设置时测试跳过，不算集成通过。
package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"partitura/server/internal/auth"
	"partitura/server/internal/db/testutil"
)

// setupTestDB 创建测试数据库连接并执行迁移。
// 使用跨进程 advisory lock + 完整 reset 隔离，不依赖测试执行顺序。
// 返回 *sql.DB 和 cleanup 函数。
func setupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	return testutil.SetupTestDB(t)
}

// makeIntegrationAuthCfg 返回集成测试用 AuthConfig（Argon2 参数使用较小值以加速测试）。
func makeIntegrationAuthCfg() auth.AuthConfig {
	return auth.AuthConfig{
		Argon2Memory:      32 * 1024,
		Argon2Iterations:  1,
		Argon2Parallelism: 1,
		Argon2SaltLength:  16,
		Argon2KeyLength:   32,
	}
}

// mockAuditRepo 是集成测试用的内存审计 mock。
type integrationAuditRepo struct {
	mu      sync.Mutex
	entries []integrationAuditEntry
}

type integrationAuditEntry struct {
	userID       string
	action       string
	resourceType string
	resourceID   string
	detail       json.RawMessage
}

func newIntegrationAuditRepo() *integrationAuditRepo {
	return &integrationAuditRepo{}
}

func (m *integrationAuditRepo) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, integrationAuditEntry{
		userID:       userID,
		action:       action,
		resourceType: resourceType,
		resourceID:   resourceID,
		detail:       detail,
	})
	return nil
}

// TestIntegration_Bootstrap_Success 空数据库 Bootstrap 成功创建 system_admin。
func TestIntegration_Bootstrap_Success(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := makeIntegrationAuthCfg()
	repo := auth.NewPGRepository(db)
	auditRepo := newIntegrationAuditRepo()
	handler := NewHandler(repo, repo, auditRepo, cfg)

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
	if resp.UserID == "" {
		t.Fatal("user_id 不应为空")
	}

	// 验证用户可在数据库中查询到
	user, err := repo.GetUserByUsername(context.Background(), "admin")
	if err != nil {
		t.Fatalf("查询创建的用户失败: %v", err)
	}
	if user.SystemRole != "system_admin" {
		t.Fatalf("system_role = %q, want %q", user.SystemRole, "system_admin")
	}
	if !user.WorkspaceCreatePerm {
		t.Fatal("workspace_create_perm 应为 true")
	}

	// 验证审计记录
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
}

// TestIntegration_Bootstrap_AlreadyHasUsers 非空数据库 Bootstrap 永久拒绝。
func TestIntegration_Bootstrap_AlreadyHasUsers(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := makeIntegrationAuthCfg()
	repo := auth.NewPGRepository(db)
	auditRepo := newIntegrationAuditRepo()
	handler := NewHandler(repo, repo, auditRepo, cfg)

	// 先创建一个用户
	hash, err := auth.HashPassword("existing-pass-123", cfg)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}
	_, err = repo.CreateUser(context.Background(), "existing", "existing@example.com", hash, "user", false)
	if err != nil {
		t.Fatalf("创建已有用户失败: %v", err)
	}

	// 尝试 Bootstrap 应被拒绝
	body := `{"username":"admin","email":"admin@example.com","password":"secure-password-123"}`
	req := httptest.NewRequest(http.MethodPost, "/api/bootstrap", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.CreateFirstAdmin(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusForbidden)
	}

	// 验证审计记录为空（拒绝时不写审计）
	if len(auditRepo.entries) != 0 {
		t.Fatalf("审计记录数 = %d, want 0", len(auditRepo.entries))
	}

	// 验证数据库中只有原有用户
	var count int
	err = db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM users").Scan(&count)
	if err != nil {
		t.Fatalf("查询用户总数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("用户总数 = %d, want 1", count)
	}
}

// TestIntegration_Bootstrap_ConcurrentSingleUse 并发 Bootstrap 请求中最多一个成功。
// 引入动机：这是 P0 竞态修复的核心集成验证——多个并发请求同时调用 Bootstrap API，
// 验证 PG 事务 + LOCK TABLE 确保只有第一个请求成功创建管理员。
func TestIntegration_Bootstrap_ConcurrentSingleUse(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := makeIntegrationAuthCfg()
	repo := auth.NewPGRepository(db)
	auditRepo := newIntegrationAuditRepo()
	handler := NewHandler(repo, repo, auditRepo, cfg)

	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	type result struct {
		status int
		body   string
	}
	results := make([]result, concurrency)

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

	// 验证数据库中只有一个用户
	var count int
	err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM users").Scan(&count)
	if err != nil {
		t.Fatalf("查询用户总数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("数据库用户总数 = %d, want 1", count)
	}

	// 验证审计记录只有一条
	if len(auditRepo.entries) != 1 {
		t.Fatalf("审计记录数 = %d, want 1", len(auditRepo.entries))
	}
}

// TestIntegration_Bootstrap_GetStatusEmpty 空数据库状态查询返回 bootstrap_available=true。
func TestIntegration_Bootstrap_GetStatusEmpty(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := makeIntegrationAuthCfg()
	repo := auth.NewPGRepository(db)
	handler := NewHandler(repo, repo, newIntegrationAuditRepo(), cfg)

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

// TestIntegration_Bootstrap_GetStatusNonEmpty 非空数据库状态查询返回 bootstrap_available=false。
func TestIntegration_Bootstrap_GetStatusNonEmpty(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := makeIntegrationAuthCfg()
	repo := auth.NewPGRepository(db)
	handler := NewHandler(repo, repo, newIntegrationAuditRepo(), cfg)

	// 创建一个用户
	hash, err := auth.HashPassword("some-pass-12345", cfg)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}
	_, err = repo.CreateUser(context.Background(), "someone", "someone@example.com", hash, "user", false)
	if err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

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
