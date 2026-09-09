// mock_auth_repo_test.go 提供 auth.Repository 接口的本地 mock 实现，供 document 测试使用。
//
// 引入动机：auth 包的 MockRepository 定义在 _test.go 文件中，无法跨包访问。
// document 测试需要 auth.Repository 的 mock 来构建完整 middleware 链，
// 因此在本地创建一个最小化的 mock 实现，与 workspace 测试中的模式一致。
package document

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"partitura/server/internal/auth"
)

// LocalAuthMockRepository 是 auth.Repository 接口的本地 mock 实现。
// 引入动机：document 测试需要注入 auth.Repository 到 AuthMiddleware，
// 但 auth 包的 MockRepository 在 _test.go 中不可跨包访问。
type LocalAuthMockRepository struct {
	mu sync.Mutex

	users map[string]*auth.User // key: username

	sessions         map[string]*auth.SessionRecord // key: token_hash
	sessionsByID     map[string]*auth.SessionRecord // key: session ID

	deviceSessions          map[string]*auth.DeviceSessionRecord // key: access_token_hash
	deviceSessionsByRefresh map[string]*auth.DeviceSessionRecord // key: refresh_token_hash
	deviceSessionsByID      map[string]*auth.DeviceSessionRecord // key: device session ID
}

// NewLocalAuthMockRepository 创建空 mock auth repository。
func NewLocalAuthMockRepository() *LocalAuthMockRepository {
	return &LocalAuthMockRepository{
		users:                   make(map[string]*auth.User),
		sessions:                make(map[string]*auth.SessionRecord),
		sessionsByID:            make(map[string]*auth.SessionRecord),
		deviceSessions:          make(map[string]*auth.DeviceSessionRecord),
		deviceSessionsByRefresh: make(map[string]*auth.DeviceSessionRecord),
		deviceSessionsByID:      make(map[string]*auth.DeviceSessionRecord),
	}
}

// AddUser 向 mock repository 中添加一个用户。
func (m *LocalAuthMockRepository) AddUser(u *auth.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.Username] = u
}

// GetUserByUsername 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) GetUserByUsername(ctx context.Context, username string) (*auth.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *u
	return &copied, nil
}

// GetUserByID 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) GetUserByID(ctx context.Context, id string) (*auth.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.ID == id {
			copied := *u
			return &copied, nil
		}
	}
	return nil, sql.ErrNoRows
}

// CreateSession 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) CreateSession(ctx context.Context, userID, tokenHash, csrfTokenHash string, expiresAt time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessionID := "session-" + tokenHash[:8]
	s := &auth.SessionRecord{
		ID:            sessionID,
		UserID:        userID,
		TokenHash:     tokenHash,
		CSRFTokenHash: csrfTokenHash,
		ExpiresAt:     expiresAt,
		CreatedAt:     time.Now(),
	}
	m.sessions[tokenHash] = s
	m.sessionsByID[sessionID] = s
	return sessionID, nil
}

// GetSessionByTokenHash 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*auth.SessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[tokenHash]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *s
	return &copied, nil
}

// RevokeSession 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) RevokeSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessionsByID[sessionID]
	if !ok {
		return nil
	}
	s.RevokedAt = sql.NullTime{Time: time.Now(), Valid: true}
	return nil
}

// CreateDeviceSession 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) CreateDeviceSession(ctx context.Context, userID, deviceName, accessTokenHash, refreshTokenHash string, expiresAt, refreshExpiresAt time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "device-" + accessTokenHash[:8]
	d := &auth.DeviceSessionRecord{
		ID:               id,
		UserID:           userID,
		DeviceName:       deviceName,
		AccessTokenHash:  accessTokenHash,
		RefreshTokenHash: refreshTokenHash,
		ExpiresAt:        expiresAt,
		RefreshExpiresAt: refreshExpiresAt,
		CreatedAt:        time.Now(),
	}
	m.deviceSessions[accessTokenHash] = d
	m.deviceSessionsByRefresh[refreshTokenHash] = d
	m.deviceSessionsByID[id] = d
	return id, nil
}

// GetDeviceSessionByAccessTokenHash 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) GetDeviceSessionByAccessTokenHash(ctx context.Context, accessTokenHash string) (*auth.DeviceSessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessions[accessTokenHash]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *d
	return &copied, nil
}

// GetDeviceSessionByRefreshTokenHash 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) GetDeviceSessionByRefreshTokenHash(ctx context.Context, refreshTokenHash string) (*auth.DeviceSessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByRefresh[refreshTokenHash]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *d
	return &copied, nil
}

// GetDeviceSessionByID 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) GetDeviceSessionByID(ctx context.Context, deviceSessionID string) (*auth.DeviceSessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByID[deviceSessionID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *d
	return &copied, nil
}

// UpdateDeviceSessionTokens 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) UpdateDeviceSessionTokens(ctx context.Context, deviceSessionID, newAccessTokenHash, newRefreshTokenHash string, newExpiresAt, newRefreshExpiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByID[deviceSessionID]
	if !ok {
		return sql.ErrNoRows
	}
	if d.RevokedAt.Valid {
		return sql.ErrNoRows
	}
	delete(m.deviceSessions, d.AccessTokenHash)
	delete(m.deviceSessionsByRefresh, d.RefreshTokenHash)
	d.AccessTokenHash = newAccessTokenHash
	d.RefreshTokenHash = newRefreshTokenHash
	d.ExpiresAt = newExpiresAt
	d.RefreshExpiresAt = newRefreshExpiresAt
	m.deviceSessions[newAccessTokenHash] = d
	m.deviceSessionsByRefresh[newRefreshTokenHash] = d
	return nil
}

// RevokeDeviceSession 实现 auth.Repository 接口。
func (m *LocalAuthMockRepository) RevokeDeviceSession(ctx context.Context, deviceSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByID[deviceSessionID]
	if !ok {
		return nil
	}
	d.RevokedAt = sql.NullTime{Time: time.Now(), Valid: true}
	return nil
}

// CreateUser 实现 auth.Repository 接口。
// 引入动机：auth.Repository 新增了 CreateUser 方法，mock 需要实现以满足接口。
func (m *LocalAuthMockRepository) CreateUser(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[username]; exists {
		return "", fmt.Errorf("用户名已存在: %s", username)
	}
	id := fmt.Sprintf("mock-user-%d", len(m.users)+1)
	m.users[username] = &auth.User{
		ID:                  id,
		Username:            username,
		Email:               email,
		PasswordHash:        passwordHash,
		SystemRole:          systemRole,
		WorkspaceCreatePerm: workspaceCreatePerm,
	}
	return id, nil
}

// CountUsers 实现 auth.Repository 接口。
// 引入动机：auth.Repository 新增了 CountUsers 方法，mock 需要实现以满足接口。
func (m *LocalAuthMockRepository) CountUsers(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

// CreateUserIfNoneExist 实现 auth.Repository 接口。
// 引入动机：auth.Repository 新增了 CreateUserIfNoneExist 方法（Bootstrap 原子创建首个管理员），
// mock 需要实现以满足接口。users 表非空时返回 auth.ErrUsersAlreadyExist。
func (m *LocalAuthMockRepository) CreateUserIfNoneExist(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > 0 {
		return "", auth.ErrUsersAlreadyExist
	}
	id := fmt.Sprintf("mock-user-%d", len(m.users)+1)
	m.users[username] = &auth.User{
		ID:                  id,
		Username:            username,
		Email:               email,
		PasswordHash:        passwordHash,
		SystemRole:          systemRole,
		WorkspaceCreatePerm: workspaceCreatePerm,
	}
	return id, nil
}
