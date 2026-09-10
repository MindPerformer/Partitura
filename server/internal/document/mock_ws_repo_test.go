// mock_ws_repo_test.go 提供 workspace.Repository 接口的本地 mock 实现，供 document 测试使用。
//
// 引入动机：workspace 包的 MockRepository 定义在 _test.go 文件中，无法跨包访问。
// document 测试需要 workspace.Repository 的 mock 来构建完整 middleware 链
// （RequireWorkspacePermission 需要 workspace.Repository 查询成员角色），
// 因此在本地创建一个最小化的 mock 实现，仅覆盖 document 测试所需的方法。
package document

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"partitura/server/internal/workspace"
)

// LocalWSMockRepository 是 workspace.Repository 接口的本地 mock 实现。
// 引入动机：document 测试需要注入 workspace.Repository 到 RequireWorkspacePermission，
// 但 workspace 包的 MockRepository 在 _test.go 中不可跨包访问。
// 仅实现 document 测试所需的最小行为集。
type LocalWSMockRepository struct {
	mu sync.Mutex

	workspaces  map[string]*workspace.Workspace
	members     map[string]map[string]string // key: workspaceID -> map[userID]role
	memberIDCtr int
}

// NewLocalWSMockRepository 创建空 mock workspace repository。
func NewLocalWSMockRepository() *LocalWSMockRepository {
	return &LocalWSMockRepository{
		workspaces: make(map[string]*workspace.Workspace),
		members:    make(map[string]map[string]string),
	}
}

// CreateWorkspace 创建 workspace 并添加创建者为 owner。
func (m *LocalWSMockRepository) CreateWorkspace(ctx context.Context, name, displayName, description, createdBy string) (*workspace.Workspace, error) {
	return m.CreateWorkspaceWithInitializer(ctx, name, displayName, description, createdBy, nil, nil)
}

// CreateWorkspaceWithInitializer 创建 workspace 并添加创建者为 owner。
// mock 中 initializer 不执行（无真实事务）。
func (m *LocalWSMockRepository) CreateWorkspaceWithInitializer(ctx context.Context, name, displayName, description, createdBy string, initializer workspace.SpecialDocumentsInitializer, jobEnq workspace.TxJobEnqueuer) (*workspace.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ws := range m.workspaces {
		if ws.Name == name {
			return nil, fmt.Errorf("workspace name already exists")
		}
	}

	wsID := fmt.Sprintf("ws-%d", len(m.workspaces)+1)
	ws := &workspace.Workspace{
		ID:                    wsID,
		Name:                  name,
		DisplayName:           displayName,
		Description:           description,
		Status:                workspace.StatusActive,
		RevisionRetentionDays: 7,
		RevisionMaxCount:      30,
		MaxDocumentSizeBytes:  2097152,
		CreatedBy:             createdBy,
	}
	m.workspaces[wsID] = ws

	if m.members[wsID] == nil {
		m.members[wsID] = make(map[string]string)
	}
	m.members[wsID][createdBy] = workspace.RoleOwner

	return ws, nil
}

// GetWorkspaceByID 查询 workspace。
func (m *LocalWSMockRepository) GetWorkspaceByID(ctx context.Context, id string) (*workspace.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *ws
	return &copied, nil
}

// ListWorkspacesByUser 查询用户作为成员的 workspace 列表。
func (m *LocalWSMockRepository) ListWorkspacesByUser(ctx context.Context, userID string, limit, offset int) (*workspace.ListWorkspacesResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []workspace.Workspace
	for wsID, members := range m.members {
		if _, ok := members[userID]; ok {
			if ws, exists := m.workspaces[wsID]; exists {
				all = append(all, *ws)
			}
		}
	}
	total := len(all)
	if offset >= total {
		return &workspace.ListWorkspacesResult{Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return &workspace.ListWorkspacesResult{Workspaces: all[offset:end], Total: total}, nil
}

// UpdateWorkspace 更新 workspace。
func (m *LocalWSMockRepository) UpdateWorkspace(ctx context.Context, workspaceID, displayName, description string) (*workspace.Workspace, error) {
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

// UpdateWorkspaceSettings 更新 workspace 设置。
func (m *LocalWSMockRepository) UpdateWorkspaceSettings(ctx context.Context, workspaceID string, opts workspace.UpdateWorkspaceSettingsOptions) (*workspace.Workspace, error) {
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

// ArchiveWorkspace 归档 workspace。
func (m *LocalWSMockRepository) ArchiveWorkspace(ctx context.Context, workspaceID string) (*workspace.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.workspaces[workspaceID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	ws.Status = workspace.StatusArchived
	copied := *ws
	return &copied, nil
}

// GetMemberRole 查询用户在 workspace 中的角色。
func (m *LocalWSMockRepository) GetMemberRole(ctx context.Context, workspaceID, userID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members, ok := m.members[workspaceID]
	if !ok {
		return "", sql.ErrNoRows
	}
	role, ok := members[userID]
	if !ok {
		return "", sql.ErrNoRows
	}
	return role, nil
}

// ListMembers 查询成员列表。
func (m *LocalWSMockRepository) ListMembers(ctx context.Context, workspaceID string, limit, offset int) (*workspace.ListMembersResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members, ok := m.members[workspaceID]
	if !ok {
		return &workspace.ListMembersResult{Total: 0}, nil
	}
	var all []workspace.Member
	for userID, role := range members {
		all = append(all, workspace.Member{
			ID:          fmt.Sprintf("member-%d", m.memberIDCtr),
			WorkspaceID: workspaceID,
			UserID:      userID,
			Role:        role,
		})
		m.memberIDCtr++
	}
	total := len(all)
	if offset >= total {
		return &workspace.ListMembersResult{Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return &workspace.ListMembersResult{Members: all[offset:end], Total: total}, nil
}

// AddMember 添加成员。
func (m *LocalWSMockRepository) AddMember(ctx context.Context, workspaceID, userID, role string) (*workspace.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.members[workspaceID] == nil {
		m.members[workspaceID] = make(map[string]string)
	}
	if _, exists := m.members[workspaceID][userID]; exists {
		return nil, fmt.Errorf("user already member")
	}
	m.members[workspaceID][userID] = role
	m.memberIDCtr++
	return &workspace.Member{
		ID:          fmt.Sprintf("member-%d", m.memberIDCtr),
		WorkspaceID: workspaceID,
		UserID:      userID,
		Role:        role,
	}, nil
}

// UpdateMemberRole 更新成员角色。
func (m *LocalWSMockRepository) UpdateMemberRole(ctx context.Context, workspaceID, userID, newRole string) (*workspace.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members, ok := m.members[workspaceID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if _, ok := members[userID]; !ok {
		return nil, sql.ErrNoRows
	}
	members[userID] = newRole
	return &workspace.Member{
		WorkspaceID: workspaceID,
		UserID:      userID,
		Role:        newRole,
	}, nil
}

// RemoveMember 移除成员。
func (m *LocalWSMockRepository) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	members, ok := m.members[workspaceID]
	if !ok {
		return sql.ErrNoRows
	}
	if _, ok := members[userID]; !ok {
		return sql.ErrNoRows
	}
	delete(members, userID)
	return nil
}

// CountOwners 统计 owner 数量。
func (m *LocalWSMockRepository) CountOwners(ctx context.Context, workspaceID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	members, ok := m.members[workspaceID]
	if !ok {
		return 0, nil
	}
	count := 0
	for _, role := range members {
		if role == workspace.RoleOwner {
			count++
		}
	}
	return count, nil
}

// GetUserByID 查询用户基本信息。
func (m *LocalWSMockRepository) GetUserByID(ctx context.Context, userID string) (*workspace.Member, error) {
	return nil, sql.ErrNoRows
}

// GetUserByUsername 查询用户基本信息。
func (m *LocalWSMockRepository) GetUserByUsername(ctx context.Context, username string) (*workspace.Member, error) {
	return nil, sql.ErrNoRows
}

// ListMemberCandidates 查询成员候选。
func (m *LocalWSMockRepository) ListMemberCandidates(ctx context.Context, workspaceID, query string, limit int) ([]workspace.Member, error) {
	return []workspace.Member{}, nil
}

// ListAllWorkspaces 查询全部 workspace。
func (m *LocalWSMockRepository) ListAllWorkspaces(ctx context.Context, limit, offset int) (*workspace.ListWorkspacesResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []workspace.Workspace
	for _, ws := range m.workspaces {
		all = append(all, *ws)
	}
	total := len(all)
	if offset >= total {
		return &workspace.ListWorkspacesResult{Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return &workspace.ListWorkspacesResult{Workspaces: all[offset:end], Total: total}, nil
}

// ListAllUsers 查询全部用户。
func (m *LocalWSMockRepository) ListAllUsers(ctx context.Context, limit, offset int) (*workspace.ListMembersResult, error) {
	return &workspace.ListMembersResult{Total: 0}, nil
}

// ListAllUsersWithSystemInfo 查询全部用户（含系统信息）。
func (m *LocalWSMockRepository) ListAllUsersWithSystemInfo(ctx context.Context, limit, offset int) (*workspace.ListAdminUsersResult, error) {
	return &workspace.ListAdminUsersResult{Total: 0}, nil
}

// UpdateUserSystemRole 更新用户系统角色。
func (m *LocalWSMockRepository) UpdateUserSystemRole(ctx context.Context, userID, systemRole string) error {
	return nil
}

// UpdateUserWorkspaceCreatePerm 更新用户 workspace:create 权限。
func (m *LocalWSMockRepository) UpdateUserWorkspaceCreatePerm(ctx context.Context, userID string, perm bool) error {
	return nil
}

// GetUserSystemInfo 查询用户系统信息。
func (m *LocalWSMockRepository) GetUserSystemInfo(ctx context.Context, userID string) (string, bool, error) {
	return "user", false, nil
}

// GetWorkspaceStats 查询 workspace 统计信息。
// 引入动机：document 测试需要满足 workspace.Repository 接口（Phase6 WP2 新增）。
func (m *LocalWSMockRepository) GetWorkspaceStats(ctx context.Context, workspaceID string) (*workspace.WorkspaceStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.workspaces[workspaceID]; !ok {
		return nil, sql.ErrNoRows
	}
	return &workspace.WorkspaceStats{
		TotalDocuments:    0,
		ActiveDocuments:   0,
		DraftDocuments:    0,
		ArchivedDocuments: 0,
		MemberCount:       len(m.members[workspaceID]),
		RecentRevisions:   []workspace.RecentRevision{},
	}, nil
}
