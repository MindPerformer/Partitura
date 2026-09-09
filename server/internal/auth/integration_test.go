// integration_test.go 测试 auth 模块的 PostgreSQL 集成。
//
// 测试覆盖：
//   - Login → session 创建 → DB 中只存 token hash
//   - Device authorize → DB 中只存 token hash
//   - Refresh → token 轮换 → 旧 token 失效
//   - Revoke → device session 撤销
//   - Session 过期/撤销拒绝
//   - Middleware cookie/bearer 身份注入全链路
//
// 运行条件：设置 TEST_DATABASE_URL 环境变量指向可用的 PostgreSQL 实例。
// 未设置时测试跳过，不算集成通过。
package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"partitura/server/internal/db/testutil"
)

// setupTestDB 创建测试数据库连接并执行迁移。
// 使用跨进程 advisory lock + 完整 reset 隔离，不依赖测试执行顺序。
// 返回 *sql.DB 和 cleanup 函数。
func setupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	return testutil.SetupTestDB(t)
}

// createTestUser 在数据库中创建一个测试用户并返回其 ID。
func createTestUser(t *testing.T, db *sql.DB, cfg AuthConfig, username, password string) string {
	t.Helper()
	hash, err := HashPassword(password, cfg)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}

	var userID string
	err = db.QueryRowContext(context.Background(),
		`INSERT INTO users (username, email, password_hash, system_role, workspace_create_perm) VALUES ($1, $2, $3, 'user', false) RETURNING id`,
		username, username+"@test.example", hash,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	return userID
}

// TestIntegration_Login_SessionHashOnly 集成测试：login 后 DB 中只存 session token hash。
func TestIntegration_Login_SessionHashOnly(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	createTestUser(t, db, cfg, "intuser1", "pass123")

	result, err := Login(context.Background(), repo, cfg, "intuser1", "pass123")
	if err != nil {
		t.Fatalf("Login 失败: %v", err)
	}

	// 验证 DB 中只存 hash
	var dbTokenHash string
	err = db.QueryRowContext(context.Background(),
		"SELECT token_hash FROM sessions WHERE id = $1", result.SessionID,
	).Scan(&dbTokenHash)
	if err != nil {
		t.Fatalf("查询 session 失败: %v", err)
	}

	if dbTokenHash == result.SessionToken {
		t.Error("DB 中不应存储明文 session token")
	}
	if dbTokenHash != HashToken(result.SessionToken) {
		t.Error("DB 中应存储 session token 的 SHA-256 哈希")
	}
}

// TestIntegration_Login_WrongPassword 集成测试：错误密码返回 ErrInvalidCredentials。
func TestIntegration_Login_WrongPassword(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	createTestUser(t, db, cfg, "intuser2", "correctpass")

	_, err := Login(context.Background(), repo, cfg, "intuser2", "wrongpass")
	if err != ErrInvalidCredentials {
		t.Errorf("错误密码应返回 ErrInvalidCredentials，实际: %v", err)
	}
}

// TestIntegration_Login_NonexistentUser 集成测试：不存在的用户返回相同错误。
func TestIntegration_Login_NonexistentUser(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)

	_, err := Login(context.Background(), repo, cfg, "nonexistent", "anypass")
	if err != ErrInvalidCredentials {
		t.Errorf("不存在的用户应返回 ErrInvalidCredentials（不泄露存在性），实际: %v", err)
	}
}

// TestIntegration_DeviceAuthorize_TokenHashOnly 集成测试：device authorize 后 DB 中只存 token hash。
func TestIntegration_DeviceAuthorize_TokenHashOnly(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser3", "pass123")

	result, err := AuthorizeDevice(context.Background(), repo, cfg, userID, "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 验证 DB 中只存 access token hash
	var dbAccessHash, dbRefreshHash string
	err = db.QueryRowContext(context.Background(),
		"SELECT access_token_hash, refresh_token_hash FROM device_sessions WHERE id = $1", result.DeviceSessionID,
	).Scan(&dbAccessHash, &dbRefreshHash)
	if err != nil {
		t.Fatalf("查询 device session 失败: %v", err)
	}

	if dbAccessHash == result.AccessToken {
		t.Error("DB 中不应存储明文 access token")
	}
	if dbAccessHash != HashToken(result.AccessToken) {
		t.Error("DB 中应存储 access token 的 SHA-256 哈希")
	}
	if dbRefreshHash == result.RefreshToken {
		t.Error("DB 中不应存储明文 refresh token")
	}
	if dbRefreshHash != HashToken(result.RefreshToken) {
		t.Error("DB 中应存储 refresh token 的 SHA-256 哈希")
	}
}

// TestIntegration_Refresh_TokenRotation 集成测试：refresh 后旧 token 失效。
func TestIntegration_Refresh_TokenRotation(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser4", "pass123")

	result, err := AuthorizeDevice(context.Background(), repo, cfg, userID, "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	oldAccess := result.AccessToken
	oldRefresh := result.RefreshToken

	// 刷新
	refreshResult, err := RefreshToken(context.Background(), repo, cfg, oldRefresh)
	if err != nil {
		t.Fatalf("RefreshToken 失败: %v", err)
	}

	// 旧 access token 应失效
	_, err = repo.GetDeviceSessionByAccessTokenHash(context.Background(), HashToken(oldAccess))
	if err == nil {
		t.Error("旧 access token 应已失效")
	}

	// 旧 refresh token 应失效
	_, err = repo.GetDeviceSessionByRefreshTokenHash(context.Background(), HashToken(oldRefresh))
	if err == nil {
		t.Error("旧 refresh token 应已失效")
	}

	// 新 token 应可用
	_, err = repo.GetDeviceSessionByAccessTokenHash(context.Background(), HashToken(refreshResult.AccessToken))
	if err != nil {
		t.Errorf("新 access token 应可用: %v", err)
	}
}

// TestIntegration_RevokeDeviceSession 集成测试：撤销 device session 后 access token 不可用。
func TestIntegration_RevokeDeviceSession(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser5", "pass123")

	result, err := AuthorizeDevice(context.Background(), repo, cfg, userID, "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 验证 access token 有效
	_, err = ValidateAccessToken(context.Background(), repo, result.AccessToken)
	if err != nil {
		t.Fatalf("撤销前 access token 应有效: %v", err)
	}

	// 撤销
	err = RevokeDeviceSession(context.Background(), repo, result.DeviceSessionID)
	if err != nil {
		t.Fatalf("RevokeDeviceSession 失败: %v", err)
	}

	// 验证 access token 已不可用
	_, err = ValidateAccessToken(context.Background(), repo, result.AccessToken)
	if err == nil {
		t.Error("撤销后 access token 应不可用")
	}
}

// TestIntegration_ValidateSession_Expired 集成测试：过期 session 被拒绝。
func TestIntegration_ValidateSession_Expired(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser6", "pass123")

	// 创建一个已过期的 session
	sessionToken, _ := GenerateToken()
	csrfToken, _ := GenerateToken()
	_, err := repo.CreateSession(context.Background(), userID, HashToken(sessionToken), HashToken(csrfToken), time.Now().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("CreateSession 失败: %v", err)
	}

	_, err = ValidateSession(context.Background(), repo, sessionToken)
	if err != ErrSessionExpired {
		t.Errorf("过期 session 应返回 ErrSessionExpired，实际: %v", err)
	}
}

// TestIntegration_ValidateSession_Revoked 集成测试：已撤销 session 被拒绝。
func TestIntegration_ValidateSession_Revoked(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser7", "pass123")

	sessionToken, _ := GenerateToken()
	csrfToken, _ := GenerateToken()
	sessionID, err := repo.CreateSession(context.Background(), userID, HashToken(sessionToken), HashToken(csrfToken), time.Now().Add(1*time.Hour))
	if err != nil {
		t.Fatalf("CreateSession 失败: %v", err)
	}

	// 撤销
	err = repo.RevokeSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("RevokeSession 失败: %v", err)
	}

	_, err = ValidateSession(context.Background(), repo, sessionToken)
	if err != ErrSessionRevoked {
		t.Errorf("已撤销 session 应返回 ErrSessionRevoked，实际: %v", err)
	}
}

// TestIntegration_Logout 集成测试：logout 后 session 被撤销。
func TestIntegration_Logout(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	createTestUser(t, db, cfg, "intuser8", "pass123")

	result, err := Login(context.Background(), repo, cfg, "intuser8", "pass123")
	if err != nil {
		t.Fatalf("Login 失败: %v", err)
	}

	// 验证 session 有效
	_, err = ValidateSession(context.Background(), repo, result.SessionToken)
	if err != nil {
		t.Fatalf("logout 前 session 应有效: %v", err)
	}

	// Logout
	err = Logout(context.Background(), repo, result.SessionToken)
	if err != nil {
		t.Fatalf("Logout 失败: %v", err)
	}

	// 验证 session 已撤销
	_, err = ValidateSession(context.Background(), repo, result.SessionToken)
	if err != ErrSessionRevoked {
		t.Errorf("logout 后 session 应被撤销，实际: %v", err)
	}
}

// TestIntegration_FullFlow 全链路集成测试：login → device authorize → refresh → revoke。
func TestIntegration_FullFlow(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser9", "pass123")

	// 1. Login
	loginResult, err := Login(context.Background(), repo, cfg, "intuser9", "pass123")
	if err != nil {
		t.Fatalf("Login 失败: %v", err)
	}
	t.Logf("login 成功: user=%s, session=%s", loginResult.UserID, loginResult.SessionID)

	// 2. Validate session
	id, err := ValidateSession(context.Background(), repo, loginResult.SessionToken)
	if err != nil {
		t.Fatalf("ValidateSession 失败: %v", err)
	}
	if id.UserID != userID {
		t.Errorf("UserID = %q, 期望 %q", id.UserID, userID)
	}

	// 3. Device authorize
	deviceResult, err := AuthorizeDevice(context.Background(), repo, cfg, userID, "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}
	t.Logf("device authorize 成功: device_session=%s", deviceResult.DeviceSessionID)

	// 4. Validate access token
	id2, err := ValidateAccessToken(context.Background(), repo, deviceResult.AccessToken)
	if err != nil {
		t.Fatalf("ValidateAccessToken 失败: %v", err)
	}
	if id2.UserID != userID {
		t.Errorf("UserID = %q, 期望 %q", id2.UserID, userID)
	}

	// 5. Refresh
	refreshResult, err := RefreshToken(context.Background(), repo, cfg, deviceResult.RefreshToken)
	if err != nil {
		t.Fatalf("RefreshToken 失败: %v", err)
	}
	t.Logf("refresh 成功")

	// 6. Validate new access token
	_, err = ValidateAccessToken(context.Background(), repo, refreshResult.AccessToken)
	if err != nil {
		t.Fatalf("新 access token 验证失败: %v", err)
	}

	// 7. Revoke
	err = RevokeDeviceSession(context.Background(), repo, deviceResult.DeviceSessionID)
	if err != nil {
		t.Fatalf("RevokeDeviceSession 失败: %v", err)
	}

	// 8. 验证撤销后 access token 不可用
	_, err = ValidateAccessToken(context.Background(), repo, refreshResult.AccessToken)
	if err == nil {
		t.Error("撤销后 access token 应不可用")
	}

	// 9. Logout
	err = Logout(context.Background(), repo, loginResult.SessionToken)
	if err != nil {
		t.Fatalf("Logout 失败: %v", err)
	}

	// 10. 验证 logout 后 session 不可用
	_, err = ValidateSession(context.Background(), repo, loginResult.SessionToken)
	if err == nil {
		t.Error("logout 后 session 应不可用")
	}

	t.Log("全链路测试通过: login → validate session → device authorize → validate access token → refresh → validate new token → revoke → logout")
}

// TestIntegration_CSRFValidation 集成测试：CSRF token 验证。
func TestIntegration_CSRFValidation(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	createTestUser(t, db, cfg, "intuser10", "pass123")

	result, err := Login(context.Background(), repo, cfg, "intuser10", "pass123")
	if err != nil {
		t.Fatalf("Login 失败: %v", err)
	}

	// 正确 CSRF token
	err = ValidateCSRF(context.Background(), repo, result.SessionToken, result.CSRFToken)
	if err != nil {
		t.Errorf("正确 CSRF token 应通过验证: %v", err)
	}

	// 错误 CSRF token
	err = ValidateCSRF(context.Background(), repo, result.SessionToken, "wrong-csrf")
	if err == nil {
		t.Error("错误 CSRF token 应不通过验证")
	}

	// 空 CSRF token
	err = ValidateCSRF(context.Background(), repo, result.SessionToken, "")
	if err == nil {
		t.Error("空 CSRF token 应不通过验证")
	}
}

// TestIntegration_RefreshToken_Expired 集成测试：过期 refresh token 被拒绝。
func TestIntegration_RefreshToken_Expired(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser11", "pass123")

	// 创建一个 refresh token 已过期的 device session
	accessToken, _ := GenerateToken()
	refreshToken, _ := GenerateToken()
	_, err := repo.CreateDeviceSession(context.Background(), userID, "test",
		HashToken(accessToken), HashToken(refreshToken),
		time.Now().Add(1*time.Hour), time.Now().Add(-1*time.Hour)) // refresh 已过期
	if err != nil {
		t.Fatalf("CreateDeviceSession 失败: %v", err)
	}

	_, err = RefreshToken(context.Background(), repo, cfg, refreshToken)
	if err != ErrRefreshTokenExpired {
		t.Errorf("过期 refresh token 应返回 ErrRefreshTokenExpired，实际: %v", err)
	}
}

// TestIntegration_RefreshToken_Revoked 集成测试：已撤销 device session 的 refresh 被拒绝。
func TestIntegration_RefreshToken_Revoked(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := testAuthCfg()
	repo := NewPGRepository(db)
	userID := createTestUser(t, db, cfg, "intuser12", "pass123")

	result, err := AuthorizeDevice(context.Background(), repo, cfg, userID, "test-device")
	if err != nil {
		t.Fatalf("AuthorizeDevice 失败: %v", err)
	}

	// 撤销
	err = RevokeDeviceSession(context.Background(), repo, result.DeviceSessionID)
	if err != nil {
		t.Fatalf("RevokeDeviceSession 失败: %v", err)
	}

	// 尝试刷新
	_, err = RefreshToken(context.Background(), repo, cfg, result.RefreshToken)
	if err != ErrDeviceSessionRevoked {
		t.Errorf("已撤销 device session 的 refresh 应返回 ErrDeviceSessionRevoked，实际: %v", err)
	}
}

