// mock_repo.go 提供 Repository 接口的内存 mock 实现，用于 handler/middleware 测试。
//
// 引入动机：HTTP handler 和 middleware 测试需要在不依赖 PostgreSQL 的情况下
// 验证认证流程逻辑。mock repository 在内存中模拟数据库行为。
package auth

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// MockRepository 是 Repository 接口的内存 mock 实现。
// 引入动机：测试 handler/middleware 时注入，避免依赖真实数据库。
type MockRepository struct {
	mu sync.Mutex

	users map[string]*User // key: username

	sessions     map[string]*SessionRecord // key: token_hash
	sessionsByID map[string]*SessionRecord // key: session ID

	deviceSessions          map[string]*DeviceSessionRecord // key: access_token_hash
	deviceSessionsByRefresh map[string]*DeviceSessionRecord // key: refresh_token_hash
	deviceSessionsByID      map[string]*DeviceSessionRecord // key: device session ID
}

// NewMockRepository 创建空 mock repository。
func NewMockRepository() *MockRepository {
	return &MockRepository{
		users:                   make(map[string]*User),
		sessions:                make(map[string]*SessionRecord),
		sessionsByID:            make(map[string]*SessionRecord),
		deviceSessions:          make(map[string]*DeviceSessionRecord),
		deviceSessionsByRefresh: make(map[string]*DeviceSessionRecord),
		deviceSessionsByID:      make(map[string]*DeviceSessionRecord),
	}
}

// AddUser 向 mock repository 中添加一个用户。
// 引入动机：测试需要预先存在用户才能测试 login 流程。
func (m *MockRepository) AddUser(u *User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.Username] = u
}

// GetUserByUsername 实现 Repository 接口。
func (m *MockRepository) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return nil, sql.ErrNoRows
	}
	// 返回副本
	copied := *u
	return &copied, nil
}

// GetUserByID 实现 Repository 接口。
func (m *MockRepository) GetUserByID(ctx context.Context, id string) (*User, error) {
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

// CreateSession 实现 Repository 接口。
func (m *MockRepository) CreateSession(ctx context.Context, userID, tokenHash, csrfTokenHash string, expiresAt time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sessionID := "session-" + tokenHash[:8]
	s := &SessionRecord{
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

// GetSessionByTokenHash 实现 Repository 接口。
func (m *MockRepository) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*SessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[tokenHash]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *s
	return &copied, nil
}

// RevokeSession 实现 Repository 接口。
func (m *MockRepository) RevokeSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessionsByID[sessionID]
	if !ok {
		return nil
	}
	now := time.Now()
	s.RevokedAt = sql.NullTime{Time: now, Valid: true}
	return nil
}

// CreateDeviceSession 实现 Repository 接口。
func (m *MockRepository) CreateDeviceSession(ctx context.Context, userID, deviceName, accessTokenHash, refreshTokenHash string, expiresAt, refreshExpiresAt time.Time) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "device-" + accessTokenHash[:8]
	d := &DeviceSessionRecord{
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

// GetDeviceSessionByAccessTokenHash 实现 Repository 接口。
func (m *MockRepository) GetDeviceSessionByAccessTokenHash(ctx context.Context, accessTokenHash string) (*DeviceSessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessions[accessTokenHash]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *d
	return &copied, nil
}

// GetDeviceSessionByRefreshTokenHash 实现 Repository 接口。
func (m *MockRepository) GetDeviceSessionByRefreshTokenHash(ctx context.Context, refreshTokenHash string) (*DeviceSessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByRefresh[refreshTokenHash]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *d
	return &copied, nil
}

// GetDeviceSessionByID 实现 Repository 接口。
func (m *MockRepository) GetDeviceSessionByID(ctx context.Context, deviceSessionID string) (*DeviceSessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByID[deviceSessionID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *d
	return &copied, nil
}

// ListDeviceSessions 实现 Repository 接口，只返回指定用户的非敏感元数据。
func (m *MockRepository) ListDeviceSessions(ctx context.Context, userID string, limit, offset int) (*ListDeviceSessionsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	all := make([]DeviceSessionSummary, 0)
	for _, d := range m.deviceSessionsByID {
		if d.UserID != userID {
			continue
		}
		s := DeviceSessionSummary{
			ID: d.ID, DeviceName: d.DeviceName,
			ExpiresAt:        d.ExpiresAt.UTC().Format(time.RFC3339),
			RefreshExpiresAt: d.RefreshExpiresAt.UTC().Format(time.RFC3339),
			CreatedAt:        d.CreatedAt.UTC().Format(time.RFC3339),
		}
		if d.RevokedAt.Valid {
			revoked := d.RevokedAt.Time.UTC().Format(time.RFC3339)
			s.RevokedAt = &revoked
		}
		all = append(all, s)
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return &ListDeviceSessionsResult{Sessions: all[offset:end], Total: len(all)}, nil
}

// UpdateDeviceSessionTokens 实现 Repository 接口。
func (m *MockRepository) UpdateDeviceSessionTokens(ctx context.Context, deviceSessionID, newAccessTokenHash, newRefreshTokenHash string, newExpiresAt, newRefreshExpiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByID[deviceSessionID]
	if !ok {
		return sql.ErrNoRows
	}
	if d.RevokedAt.Valid {
		return sql.ErrNoRows
	}
	// 删除旧索引
	delete(m.deviceSessions, d.AccessTokenHash)
	delete(m.deviceSessionsByRefresh, d.RefreshTokenHash)
	// 更新
	d.AccessTokenHash = newAccessTokenHash
	d.RefreshTokenHash = newRefreshTokenHash
	d.ExpiresAt = newExpiresAt
	d.RefreshExpiresAt = newRefreshExpiresAt
	// 建立新索引
	m.deviceSessions[newAccessTokenHash] = d
	m.deviceSessionsByRefresh[newRefreshTokenHash] = d
	return nil
}

// RevokeDeviceSession 实现 Repository 接口。
func (m *MockRepository) RevokeDeviceSession(ctx context.Context, deviceSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deviceSessionsByID[deviceSessionID]
	if !ok {
		return nil
	}
	now := time.Now()
	d.RevokedAt = sql.NullTime{Time: now, Valid: true}
	return nil
}

// CreateUser 实现 Repository 接口。
// 引入动机：Bootstrap 测试需要创建用户。
func (m *MockRepository) CreateUser(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.users[username]; exists {
		return "", fmt.Errorf("用户名已存在: %s", username)
	}
	id := fmt.Sprintf("mock-user-%d", len(m.users)+1)
	m.users[username] = &User{
		ID:                  id,
		Username:            username,
		Email:               email,
		PasswordHash:        passwordHash,
		SystemRole:          systemRole,
		WorkspaceCreatePerm: workspaceCreatePerm,
	}
	return id, nil
}

// CountUsers 实现 Repository 接口。
// 引入动机：Bootstrap 测试需要判断 users 表是否为空。
func (m *MockRepository) CountUsers(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

// CreateUserIfNoneExist 实现 Repository 接口。
// 引入动机：Bootstrap 原子创建首个管理员，消除 TOCTOU 竞态。
// Mock 实现使用 mutex 保证并发安全：mutex 持有期间检查 + 插入是原子的。
func (m *MockRepository) CreateUserIfNoneExist(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > 0 {
		return "", ErrUsersAlreadyExist
	}
	id := fmt.Sprintf("mock-user-%d", len(m.users)+1)
	m.users[username] = &User{
		ID:                  id,
		Username:            username,
		Email:               email,
		PasswordHash:        passwordHash,
		SystemRole:          systemRole,
		WorkspaceCreatePerm: workspaceCreatePerm,
	}
	return id, nil
}
