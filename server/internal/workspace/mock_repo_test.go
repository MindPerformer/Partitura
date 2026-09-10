// mock_repo_test.go 提供 Repository 接口的内存 mock 实现，用于 handler/middleware 测试。
//
// 引入动机：HTTP handler 和 middleware 测试需要在不依赖 PostgreSQL 的情况下
// 验证 workspace RBAC 和 isolation 逻辑。mock repository 在内存中模拟数据库行为。
package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"partitura/server/internal/audit"
)

// MockRepository 是 Repository 接口的内存 mock 实现。
// 引入动机：测试 handler/middleware 时注入，避免依赖真实数据库。
type MockRepository struct {
	mu sync.Mutex

	workspaces    map[string]*Workspace        // key: workspace ID
	wsByName      map[string]*Workspace        // key: workspace name
	members       map[string][]*Member         // key: workspace ID -> members list
	membersByUser map[string]map[string]string // key: userID -> map[workspaceID]role
	users         map[string]*Member           // key: user ID -> user info
	userSysInfo   map[string]*userSysInfo      // key: user ID

	// 用于模拟自增 ID
	memberIDCounter int

	// listAllUsersWithSystemInfoCallCount 跟踪 ListAllUsersWithSystemInfo 调用次数，
	// 供测试验证 ListUsers handler 使用单次查询而非 N+1。
	listAllUsersWithSystemInfoCallCount int

	// stats 存储测试预先注入的 workspace 统计信息。
	stats map[string]*WorkspaceStats // key: workspace ID
}

// userSysInfo 存储用户的系统角色和 workspace:create 权限。
type userSysInfo struct {
	systemRole          string
	workspaceCreatePerm bool
}

// NewMockRepository 创建空 mock repository。
func NewMockRepository() *MockRepository {
	return &MockRepository{
		workspaces:    make(map[string]*Workspace),
		wsByName:      make(map[string]*Workspace),
		members:       make(map[string][]*Member),
		membersByUser: make(map[string]map[string]string),
		users:         make(map[string]*Member),
		userSysInfo:   make(map[string]*userSysInfo),
		stats:         make(map[string]*WorkspaceStats),
	}
}

// AddUser 向 mock repository 中添加一个用户。
// 引入动机：测试需要预先存在用户才能测试成员管理。
func (m *MockRepository) AddUser(userID, username, email, systemRole string, wsCreatePerm bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[userID] = &Member{
		UserID:   userID,
		Username: username,
		Email:    email,
	}
	m.userSysInfo[userID] = &userSysInfo{
		systemRole:          systemRole,
		workspaceCreatePerm: wsCreatePerm,
	}
}

// CreateWorkspace 实现 Repository 接口。
func (m *MockRepository) CreateWorkspace(ctx context.Context, name, displayName, description, createdBy string) (*Workspace, error) {
	return m.CreateWorkspaceWithInitializer(ctx, name, displayName, description, createdBy, nil, nil)
}

// CreateWorkspaceWithInitializer 实现 Repository 接口。
// 引入动机：Phase 2 需要在 workspace 创建事务中原子地初始化特殊文件。
// mock 中 initializer 为 nil 时不执行任何特殊文件初始化。
func (m *MockRepository) CreateWorkspaceWithInitializer(ctx context.Context, name, displayName, description, createdBy string, initializer SpecialDocumentsInitializer, jobEnq TxJobEnqueuer) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.wsByName[name]; exists {
		return nil, &mockDuplicateKeyError{msg: "workspace name already exists"}
	}

	wsID := fmt.Sprintf("ws-%d", len(m.workspaces)+1)
	ws := &Workspace{
		ID:                    wsID,
		Name:                  name,
		DisplayName:           displayName,
		Description:           description,
		Status:                StatusActive,
		RevisionRetentionDays: 7,
		RevisionMaxCount:      30,
		MaxDocumentSizeBytes:  2097152,
		CreatedBy:             createdBy,
	}
	m.workspaces[wsID] = ws
	m.wsByName[name] = ws

	// 创建 owner 成员记录
	memberID := fmt.Sprintf("member-%d", m.memberIDCounter+1)
	m.memberIDCounter++
	member := &Member{
		ID:          memberID,
		WorkspaceID: wsID,
		UserID:      createdBy,
		Role:        RoleOwner,
	}
	if u, ok := m.users[createdBy]; ok {
		member.Username = u.Username
		member.Email = u.Email
	}
	m.members[wsID] = append(m.members[wsID], member)

	if m.membersByUser[createdBy] == nil {
		m.membersByUser[createdBy] = make(map[string]string)
	}
	m.membersByUser[createdBy][wsID] = RoleOwner

	// mock 中不执行 initializer（无真实事务和数据库表）
	// 真实初始化在 PGRepository.CreateWorkspaceWithInitializer 中完成

	return ws, nil
}

// GetWorkspaceByID 实现 Repository 接口。
func (m *MockRepository) GetWorkspaceByID(ctx context.Context, id string) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *ws
	return &copied, nil
}

// ListWorkspacesByUser 实现 Repository 接口。
func (m *MockRepository) ListWorkspacesByUser(ctx context.Context, userID string, limit, offset int) (*ListWorkspacesResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	wsMap := m.membersByUser[userID]
	var all []Workspace
	for wsID := range wsMap {
		if ws, ok := m.workspaces[wsID]; ok {
			all = append(all, *ws)
		}
	}

	total := len(all)
	if offset >= total {
		return &ListWorkspacesResult{Workspaces: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}

	return &ListWorkspacesResult{Workspaces: all[offset:end], Total: total}, nil
}

// UpdateWorkspace 实现 Repository 接口。
func (m *MockRepository) UpdateWorkspace(ctx context.Context, workspaceID, displayName, description string) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[workspaceID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	ws.DisplayName = displayName
	ws.Description = description
	copied := *ws
	return &copied, nil
}

// UpdateWorkspaceSettings 实现 Repository 接口。
func (m *MockRepository) UpdateWorkspaceSettings(ctx context.Context, workspaceID string, opts UpdateWorkspaceSettingsOptions) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[workspaceID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if opts.DisplayName != nil {
		ws.DisplayName = *opts.DisplayName
	}
	if opts.Description != nil {
		ws.Description = *opts.Description
	}
	if opts.RevisionRetentionDays != nil {
		ws.RevisionRetentionDays = *opts.RevisionRetentionDays
	}
	if opts.RevisionMaxCount != nil {
		ws.RevisionMaxCount = *opts.RevisionMaxCount
	}
	if opts.MaxDocumentSizeBytes != nil {
		ws.MaxDocumentSizeBytes = *opts.MaxDocumentSizeBytes
	}
	copied := *ws
	return &copied, nil
}

// ArchiveWorkspace 实现 Repository 接口。
func (m *MockRepository) ArchiveWorkspace(ctx context.Context, workspaceID string) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[workspaceID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	ws.Status = StatusArchived
	copied := *ws
	return &copied, nil
}

// GetMemberRole 实现 Repository 接口。
func (m *MockRepository) GetMemberRole(ctx context.Context, workspaceID, userID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wsMap, ok := m.membersByUser[userID]
	if !ok {
		return "", sql.ErrNoRows
	}
	role, ok := wsMap[workspaceID]
	if !ok {
		return "", sql.ErrNoRows
	}
	return role, nil
}

// ListMembers 实现 Repository 接口。
func (m *MockRepository) ListMembers(ctx context.Context, workspaceID string, limit, offset int) (*ListMembersResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.members[workspaceID]
	total := len(all)
	if offset >= total {
		return &ListMembersResult{Members: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	result := make([]Member, 0, end-offset)
	for i := offset; i < end; i++ {
		result = append(result, *all[i])
	}
	return &ListMembersResult{Members: result, Total: total}, nil
}

// AddMember 实现 Repository 接口。
func (m *MockRepository) AddMember(ctx context.Context, workspaceID, userID, role string) (*Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查重复
	wsMap, exists := m.membersByUser[userID]
	if exists {
		if _, alreadyMember := wsMap[workspaceID]; alreadyMember {
			return nil, &mockDuplicateKeyError{msg: "user already member"}
		}
	}

	memberID := fmt.Sprintf("member-%d", m.memberIDCounter+1)
	m.memberIDCounter++
	member := &Member{
		ID:          memberID,
		WorkspaceID: workspaceID,
		UserID:      userID,
		Role:        role,
	}
	if u, ok := m.users[userID]; ok {
		member.Username = u.Username
		member.Email = u.Email
	}
	m.members[workspaceID] = append(m.members[workspaceID], member)

	if m.membersByUser[userID] == nil {
		m.membersByUser[userID] = make(map[string]string)
	}
	m.membersByUser[userID][workspaceID] = role

	copied := *member
	return &copied, nil
}

// UpdateMemberRole 实现 Repository 接口。
func (m *MockRepository) UpdateMemberRole(ctx context.Context, workspaceID, userID, newRole string) (*Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	wsMap, ok := m.membersByUser[userID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	oldRole, ok := wsMap[workspaceID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	_ = oldRole

	// 更新
	wsMap[workspaceID] = newRole
	for _, member := range m.members[workspaceID] {
		if member.UserID == userID {
			member.Role = newRole
			copied := *member
			return &copied, nil
		}
	}
	return nil, sql.ErrNoRows
}

// RemoveMember 实现 Repository 接口。
func (m *MockRepository) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	wsMap, ok := m.membersByUser[userID]
	if !ok {
		return sql.ErrNoRows
	}
	if _, ok := wsMap[workspaceID]; !ok {
		return sql.ErrNoRows
	}

	delete(wsMap, workspaceID)

	members := m.members[workspaceID]
	for i, member := range members {
		if member.UserID == userID {
			m.members[workspaceID] = append(members[:i], members[i+1:]...)
			break
		}
	}

	return nil
}

// CountOwners 实现 Repository 接口。
func (m *MockRepository) CountOwners(ctx context.Context, workspaceID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, member := range m.members[workspaceID] {
		if member.Role == RoleOwner {
			count++
		}
	}
	return count, nil
}

// GetUserByID 实现 Repository 接口。
func (m *MockRepository) GetUserByID(ctx context.Context, userID string) (*Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *u
	return &copied, nil
}

// GetUserByUsername 实现 Repository 接口。
func (m *MockRepository) GetUserByUsername(ctx context.Context, username string) (*Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, user := range m.users {
		if user.Username == username {
			copied := *user
			return &copied, nil
		}
	}
	return nil, sql.ErrNoRows
}

// ListMemberCandidates 实现 Repository 接口。
func (m *MockRepository) ListMemberCandidates(ctx context.Context, workspaceID, query string, limit int) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	query = strings.ToLower(query)
	members := make(map[string]bool)
	for _, member := range m.members[workspaceID] {
		members[member.UserID] = true
	}
	candidates := make([]Member, 0)
	for userID, user := range m.users {
		if members[userID] {
			continue
		}
		if !strings.Contains(strings.ToLower(user.Username), query) && !strings.Contains(strings.ToLower(user.Email), query) {
			continue
		}
		candidates = append(candidates, *user)
		if len(candidates) >= limit {
			break
		}
	}
	return candidates, nil
}

// ListAllWorkspaces 实现 Repository 接口。
func (m *MockRepository) ListAllWorkspaces(ctx context.Context, limit, offset int) (*ListWorkspacesResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Workspace
	for _, ws := range m.workspaces {
		all = append(all, *ws)
	}
	total := len(all)
	if offset >= total {
		return &ListWorkspacesResult{Workspaces: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return &ListWorkspacesResult{Workspaces: all[offset:end], Total: total}, nil
}

// ListAllUsers 实现 Repository 接口。
func (m *MockRepository) ListAllUsers(ctx context.Context, limit, offset int) (*ListMembersResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Member
	for _, u := range m.users {
		all = append(all, *u)
	}
	total := len(all)
	if offset >= total {
		return &ListMembersResult{Members: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return &ListMembersResult{Members: all[offset:end], Total: total}, nil
}

// ListAllUsersWithSystemInfo 实现 Repository 接口。
// 引入动机：admin ListUsers handler 使用此方法一次性获取用户基础信息和系统角色/权限，
// 避免逐用户 N+1 查询。mock 中递增调用计数器供测试验证单次调用。
func (m *MockRepository) ListAllUsersWithSystemInfo(ctx context.Context, limit, offset int) (*ListAdminUsersResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listAllUsersWithSystemInfoCallCount++

	var all []AdminUser
	for userID, u := range m.users {
		info := m.userSysInfo[userID]
		all = append(all, AdminUser{
			UserID:              u.UserID,
			Username:            u.Username,
			Email:               u.Email,
			SystemRole:          info.systemRole,
			WorkspaceCreatePerm: info.workspaceCreatePerm,
		})
	}
	total := len(all)
	if offset >= total {
		return &ListAdminUsersResult{Users: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return &ListAdminUsersResult{Users: all[offset:end], Total: total}, nil
}

// GetListAllUsersWithSystemInfoCallCount 返回 ListAllUsersWithSystemInfo 的调用次数。
// 引入动机：测试需要验证 ListUsers handler 仅调用一次 repository 查询，而非逐用户 N+1。
func (m *MockRepository) GetListAllUsersWithSystemInfoCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listAllUsersWithSystemInfoCallCount
}

// UpdateUserSystemRole 实现 Repository 接口。
func (m *MockRepository) UpdateUserSystemRole(ctx context.Context, userID, systemRole string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.userSysInfo[userID]
	if !ok {
		return sql.ErrNoRows
	}
	info.systemRole = systemRole
	return nil
}

// UpdateUserWorkspaceCreatePerm 实现 Repository 接口。
func (m *MockRepository) UpdateUserWorkspaceCreatePerm(ctx context.Context, userID string, perm bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.userSysInfo[userID]
	if !ok {
		return sql.ErrNoRows
	}
	info.workspaceCreatePerm = perm
	return nil
}

// GetWorkspaceStats 实现 Repository 接口。
// 引入动机：测试 GetWorkspaceStats handler 时注入预设统计信息。
func (m *MockRepository) GetWorkspaceStats(ctx context.Context, workspaceID string) (*WorkspaceStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats, ok := m.stats[workspaceID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return stats, nil
}

// SetWorkspaceStats 为测试预先注入 workspace 统计信息。
func (m *MockRepository) SetWorkspaceStats(workspaceID string, stats *WorkspaceStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats[workspaceID] = stats
}

// GetUserSystemInfo 实现 Repository 接口。
func (m *MockRepository) GetUserSystemInfo(ctx context.Context, userID string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.userSysInfo[userID]
	if !ok {
		return "", false, sql.ErrNoRows
	}
	return info.systemRole, info.workspaceCreatePerm, nil
}

// --- Mock Audit Repository ---

// MockAuditRepository 是 audit.Repository 的内存 mock 实现。
type MockAuditRepository struct {
	mu      sync.Mutex
	entries []mockAuditEntry
}

type mockAuditEntry struct {
	userID       string
	workspaceID  string
	action       string
	resourceType string
	resourceID   string
	detail       json.RawMessage
	requestID    string
	timestamp    time.Time
}

// NewMockAuditRepository 创建空 mock audit repository。
func NewMockAuditRepository() *MockAuditRepository {
	return &MockAuditRepository{}
}

// Record 实现 audit.Repository 接口。
func (m *MockAuditRepository) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, mockAuditEntry{
		userID:       userID,
		workspaceID:  workspaceID,
		action:       action,
		resourceType: resourceType,
		resourceID:   resourceID,
		detail:       detail,
		requestID:    requestID,
		timestamp:    time.Now(),
	})
	return nil
}

// List 实现 audit.Repository 接口。
func (m *MockAuditRepository) List(ctx context.Context, limit, offset int) (*audit.ListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := len(m.entries)
	if offset >= total {
		return &audit.ListResult{Entries: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}

	// 反转顺序（按时间降序）
	var result []audit.Entry
	for i := total - 1 - offset; i >= total-end; i-- {
		e := m.entries[i]
		result = append(result, audit.Entry{
			UserID:       e.userID,
			WorkspaceID:  e.workspaceID,
			Action:       e.action,
			ResourceType: e.resourceType,
			ResourceID:   e.resourceID,
			Detail:       e.detail,
			RequestID:    e.requestID,
		})
	}
	return &audit.ListResult{Entries: result, Total: total}, nil
}

// GetEntries 返回所有记录的审计条目（测试辅助函数）。
func (m *MockAuditRepository) GetEntries() []mockAuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]mockAuditEntry, len(m.entries))
	copy(copied, m.entries)
	return copied
}

// --- 辅助类型 ---

// mockDuplicateKeyError 模拟唯一约束冲突。
type mockDuplicateKeyError struct {
	msg string
}

func (e *mockDuplicateKeyError) Error() string {
	return e.msg
}
