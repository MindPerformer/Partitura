// repository.go 实现 workspace 模块的 PostgreSQL 数据访问层。
//
// 引入动机：Workspace CRUD、成员管理、权限判断需要访问 workspaces 和 workspace_members 表。
// 将数据访问集中到 repository，使领域逻辑和 HTTP handler 不直接依赖 SQL。
//
// 设计原则：
//   - 接口定义与实现分离，领域逻辑依赖接口而非具体 PG 实现
//   - 所有 workspace-scoped 查询 WHERE 子句必须包含 workspace_id
//   - 错误包装具体上下文，不吞错
package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
)

// ListWorkspacesResult 是 workspace 列表查询结果。
// 引入动机：list API 需要返回分页数据和总数。
type ListWorkspacesResult struct {
	Workspaces []Workspace
	Total      int
}

// ListMembersResult 是成员列表查询结果。
// 引入动机：成员列表 API 需要返回分页数据和总数。
type ListMembersResult struct {
	Members []Member
	Total   int
}

// RecentRevision 是 workspace 最近一次 revision 的摘要。
// 引入动机：Phase6 WP2 首页工作台需要展示近期变更摘要。
type RecentRevision struct {
	Path           string `json:"path"`
	Title          string `json:"title"`
	RevisionNumber int    `json:"revision_number"`
	CreatedAt      string `json:"created_at"`
}

// WorkspaceStats 是 workspace 工作台的聚合统计信息。
// 引入动机：Phase6 WP2 需要 GET /api/workspaces/{id}/stats 返回文档/成员/近期 revision 统计。
// 所有字段使用 snake_case JSON tag，与前端契约一致。
type WorkspaceStats struct {
	TotalDocuments    int              `json:"total_documents"`
	ActiveDocuments   int              `json:"active_documents"`
	DraftDocuments    int              `json:"draft_documents"`
	ArchivedDocuments int              `json:"archived_documents"`
	MemberCount       int              `json:"member_count"`
	RecentRevisions   []RecentRevision `json:"recent_revisions"`
}

// Repository 定义 workspace 模块所需的数据访问接口。
// 引入动机：领域逻辑和 HTTP handler 依赖此接口而非具体 PG 实现，
// 便于测试时注入 mock 或使用替代存储。
type Repository interface {
	// CreateWorkspace 在事务中创建 workspace 并为创建者创建 owner 成员记录。
	// 引入动机：design/04-WEB-API.md 要求创建 workspace 时在同一事务创建 owner membership，
	// 保证原子性——失败不留下孤立 workspace 记录。
	//
	// 参数：
	//   - ctx：请求 context
	//   - name：workspace 唯一名称
	//   - displayName：显示名称
	//   - description：描述（可为空）
	//   - createdBy：创建者用户 UUID
	//
	// 返回创建的 Workspace 记录（含生成的 UUID）。
	CreateWorkspace(ctx context.Context, name, displayName, description, createdBy string) (*Workspace, error)

	// CreateWorkspaceWithInitializer 在事务中创建 workspace、owner 成员记录和特殊文件。
	// 引入动机：Phase 2 需要在 workspace 创建事务中原子地初始化 PROJECT.md 和 AGENTS.md。
	// 如果 initializer 为 nil，则只创建 workspace 和 owner membership（向后兼容 Phase 1 测试）。
	// C1：jobEnq 非空时，在同一事务中为特殊文档 enqueue index job。
	CreateWorkspaceWithInitializer(ctx context.Context, name, displayName, description, createdBy string, initializer SpecialDocumentsInitializer, jobEnq TxJobEnqueuer) (*Workspace, error)

	// GetWorkspaceByID 根据 ID 查询 workspace。
	// 不存在返回 sql.ErrNoRows。
	GetWorkspaceByID(ctx context.Context, id string) (*Workspace, error)

	// ListWorkspacesByUser 查询用户作为成员的 workspace 列表（分页）。
	ListWorkspacesByUser(ctx context.Context, userID string, limit, offset int) (*ListWorkspacesResult, error)

	// UpdateWorkspace 更新 workspace 的可修改字段。
	UpdateWorkspace(ctx context.Context, workspaceID, displayName, description string) (*Workspace, error)

	// UpdateWorkspaceSettings 更新 workspace 设置。
	// 引入动机：Phase6 要求 workspace admin 安全更新 revision_retention_days、
	// revision_max_count、max_document_size_bytes，同时兼容 display_name/description 部分更新。
	// 参数字段为可选指针：nil 表示保持不变，非 nil 表示更新为指针值。
	// 不存在返回 sql.ErrNoRows。
	UpdateWorkspaceSettings(ctx context.Context, workspaceID string, opts UpdateWorkspaceSettingsOptions) (*Workspace, error)

	// ArchiveWorkspace 将 workspace 状态设置为 archived。
	ArchiveWorkspace(ctx context.Context, workspaceID string) (*Workspace, error)

	// GetMemberRole 查询用户在指定 workspace 中的角色。
	GetMemberRole(ctx context.Context, workspaceID, userID string) (string, error)

	// ListMembers 查询指定 workspace 的成员列表（分页）。
	ListMembers(ctx context.Context, workspaceID string, limit, offset int) (*ListMembersResult, error)

	// AddMember 向指定 workspace 添加成员。
	AddMember(ctx context.Context, workspaceID, userID, role string) (*Member, error)

	// UpdateMemberRole 更新指定 workspace 中成员的角色。
	UpdateMemberRole(ctx context.Context, workspaceID, userID, newRole string) (*Member, error)

	// RemoveMember 从指定 workspace 中移除成员。
	RemoveMember(ctx context.Context, workspaceID, userID string) error

	// CountOwners 查询指定 workspace 中拥有 owner 角色的活跃成员数量。
	CountOwners(ctx context.Context, workspaceID string) (int, error)

	// GetUserByID 根据 ID 查询用户基本信息（不含密码哈希）。
	GetUserByID(ctx context.Context, userID string) (*Member, error)

	// GetUserByUsername 根据用户名查询用户基本信息（不含密码哈希）。
	// 引入动机：成员添加 API 接受用户名，服务端负责解析为内部用户 ID。
	GetUserByUsername(ctx context.Context, username string) (*Member, error)

	// ListMemberCandidates 查询尚未加入指定 workspace 的用户候选。
	// 引入动机：成员管理页面需要按用户名搜索，且不得返回当前 workspace 已有成员。
	ListMemberCandidates(ctx context.Context, workspaceID, query string, limit int) ([]Member, error)

	// ListAllWorkspaces 查询全部 workspace 列表（分页），供 system_admin 使用。
	// filter 中零值字段不参与过滤，便于管理员按 status/name 收窄结果。
	ListAllWorkspaces(ctx context.Context, filter AdminWorkspaceFilter, limit, offset int) (*ListWorkspacesResult, error)

	// ListAllUsers 查询全部用户列表（分页），供 system_admin 使用。
	ListAllUsers(ctx context.Context, limit, offset int) (*ListMembersResult, error)

	// ListAllUsersWithSystemInfo 查询全部用户列表（含系统角色和 workspace:create 权限）。
	// filter 中零值字段不参与过滤，便于管理员按 system_role/username 收窄结果。
	ListAllUsersWithSystemInfo(ctx context.Context, filter AdminUserFilter, limit, offset int) (*ListAdminUsersResult, error)

	// UpdateUserSystemRole 更新用户的系统角色。
	UpdateUserSystemRole(ctx context.Context, userID, systemRole string) error

	// UpdateUserWorkspaceCreatePerm 更新用户的 workspace:create 权限。
	UpdateUserWorkspaceCreatePerm(ctx context.Context, userID string, perm bool) error

	// GetUserSystemInfo 查询用户的系统角色和 workspace:create 权限。
	GetUserSystemInfo(ctx context.Context, userID string) (systemRole string, workspaceCreatePerm bool, err error)

	// GetWorkspaceStats 查询 workspace 的文档/成员/近期 revision 统计。
	// 引入动机：Phase6 WP2 工作台首页需要聚合展示 workspace 核心指标。
	GetWorkspaceStats(ctx context.Context, workspaceID string) (*WorkspaceStats, error)
}

// TxJobEnqueuer 定义在 PG 事务内 enqueue job 的接口。
// 引入动机：C1 要求特殊文档初始化时在同一事务中 enqueue index job。
// workspace 包不能导入 document 包（循环依赖），因此在此独立定义。
// Go 接口是结构化的，document.PGRepository 的 EnqueueInTx 方法会同时满足此接口。
type TxJobEnqueuer interface {
	EnqueueInTx(ctx context.Context, tx *sql.Tx, jobType string, payload map[string]interface{}) (string, error)
}

// SpecialDocumentsInitializer 是 workspace 创建时初始化特殊文件的回调接口。
// 引入动机：design/00-MASTER.md §特殊文件 要求 workspace 创建时自动创建 PROJECT.md 和 AGENTS.md。
// 此接口允许 workspace 模块在不依赖 document 模块的前提下，
// 在 CreateWorkspace 事务中调用 document 模块的初始化逻辑，保证原子性。
// 传入的 tx 是 CreateWorkspace 已开启的事务，初始化失败将导致整个事务回滚。
// C1：jobEnq 非空时，在同一事务中为特殊文档 enqueue index job。
type SpecialDocumentsInitializer interface {
	// CreateSpecialDocuments 在给定事务中创建 PROJECT.md 和 AGENTS.md。
	// 引入动机：保证 workspace + owner member + 特殊文件初始化的原子性。
	CreateSpecialDocuments(ctx context.Context, tx *sql.Tx, workspaceID, createdBy string, jobEnq TxJobEnqueuer) error
}

// PGRepository 是 Repository 接口的 PostgreSQL 实现。
// 引入动机：使用 database/sql + pgx 驱动访问 PostgreSQL，
// 与 WP-1 建立的连接池架构一致。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
// 引入动机：handler 和 middleware 通过此构造函数注入数据库连接。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// CreateWorkspace 在事务中创建 workspace 并为创建者创建 owner 成员记录。
// 向后兼容：不初始化特殊文件。Phase 2 handler 应使用 CreateWorkspaceWithInitializer。
func (r *PGRepository) CreateWorkspace(ctx context.Context, name, displayName, description, createdBy string) (*Workspace, error) {
	return r.CreateWorkspaceWithInitializer(ctx, name, displayName, description, createdBy, nil, nil)
}

// CreateWorkspaceWithInitializer 在事务中创建 workspace、owner 成员记录和特殊文件。
// 引入动机：Phase 2 需要在 workspace 创建事务中原子地初始化 PROJECT.md 和 AGENTS.md。
// 如果 initializer 为 nil，则只创建 workspace 和 owner membership（向后兼容 Phase 1 测试）。
// C1：jobEnq 非空时，在同一事务中为特殊文档 enqueue index job。
func (r *PGRepository) CreateWorkspaceWithInitializer(ctx context.Context, name, displayName, description, createdBy string, initializer SpecialDocumentsInitializer, jobEnq TxJobEnqueuer) (*Workspace, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启创建 workspace 事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var ws Workspace
	err = tx.QueryRowContext(ctx,
		`INSERT INTO workspaces (name, display_name, description, created_by)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, name, display_name, description, status, revision_retention_days, revision_max_count, max_document_size_bytes, created_by`,
		name, displayName, description, createdBy,
	).Scan(
		&ws.ID, &ws.Name, &ws.DisplayName, &ws.Description,
		&ws.Status, &ws.RevisionRetentionDays, &ws.RevisionMaxCount, &ws.MaxDocumentSizeBytes, &ws.CreatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "创建 workspace")
	}

	// 在同一事务中为创建者创建 owner 成员记录
	_, err = tx.ExecContext(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		ws.ID, createdBy, RoleOwner,
	)
	if err != nil {
		return nil, mapDBError(err, "创建 owner 成员记录")
	}

	// 在同一事务中初始化特殊文件（PROJECT.md 和 AGENTS.md）
	// 引入动机：design/00-MASTER.md §特殊文件 要求 workspace 创建时自动创建特殊文件。
	// 在同一事务中执行，保证 workspace + owner member + 特殊文件初始化的原子性。
	if initializer != nil {
		if err := initializer.CreateSpecialDocuments(ctx, tx, ws.ID, createdBy, jobEnq); err != nil {
			return nil, fmt.Errorf("初始化特殊文件: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交创建 workspace 事务: %w", err)
	}

	slog.Info("workspace 创建成功", "workspace_id", ws.ID, "name", ws.Name, "created_by", createdBy)
	return &ws, nil
}

// GetWorkspaceByID 根据 ID 查询 workspace。
func (r *PGRepository) GetWorkspaceByID(ctx context.Context, id string) (*Workspace, error) {
	const q = `SELECT id, name, display_name, description, status, revision_retention_days, revision_max_count, max_document_size_bytes, created_by
	           FROM workspaces WHERE id = $1`

	var ws Workspace
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&ws.ID, &ws.Name, &ws.DisplayName, &ws.Description,
		&ws.Status, &ws.RevisionRetentionDays, &ws.RevisionMaxCount, &ws.MaxDocumentSizeBytes, &ws.CreatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "根据 ID 查询 workspace")
	}
	return &ws, nil
}

// ListWorkspacesByUser 查询用户作为成员的 workspace 列表（分页）。
func (r *PGRepository) ListWorkspacesByUser(ctx context.Context, userID string, limit, offset int) (*ListWorkspacesResult, error) {
	// 查询总数
	var total int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspaces w
		 INNER JOIN workspace_members wm ON w.id = wm.workspace_id
		 WHERE wm.user_id = $1`,
		userID,
	).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询用户 workspace 总数")
	}

	// 查询分页数据
	rows, err := r.db.QueryContext(ctx,
		`SELECT w.id, w.name, w.display_name, w.description, w.status, w.revision_retention_days, w.revision_max_count, w.max_document_size_bytes, w.created_by
		 FROM workspaces w
		 INNER JOIN workspace_members wm ON w.id = wm.workspace_id
		 WHERE wm.user_id = $1
		 ORDER BY w.created_at DESC
		 LIMIT $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
		return nil, mapDBError(err, "查询用户 workspace 列表")
	}
	defer rows.Close()

	var workspaces []Workspace
	for rows.Next() {
		var ws Workspace
		if err := rows.Scan(
			&ws.ID, &ws.Name, &ws.DisplayName, &ws.Description,
			&ws.Status, &ws.RevisionRetentionDays, &ws.RevisionMaxCount, &ws.MaxDocumentSizeBytes, &ws.CreatedBy,
		); err != nil {
			return nil, fmt.Errorf("扫描 workspace 行: %w", err)
		}
		workspaces = append(workspaces, ws)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 workspace 结果集: %w", err)
	}

	return &ListWorkspacesResult{Workspaces: workspaces, Total: total}, nil
}

// UpdateWorkspace 更新 workspace 的可修改字段。
// 引入动机：PUT/PATCH /api/workspaces/{id} 更新 display_name 和 description。
func (r *PGRepository) UpdateWorkspace(ctx context.Context, workspaceID, displayName, description string) (*Workspace, error) {
	var ws Workspace
	err := r.db.QueryRowContext(ctx,
		`UPDATE workspaces SET display_name = $1, description = $2, updated_at = now()
		 WHERE id = $3
		 RETURNING id, name, display_name, description, status, revision_retention_days, revision_max_count, max_document_size_bytes, created_by`,
		displayName, description, workspaceID,
	).Scan(
		&ws.ID, &ws.Name, &ws.DisplayName, &ws.Description,
		&ws.Status, &ws.RevisionRetentionDays, &ws.RevisionMaxCount, &ws.MaxDocumentSizeBytes, &ws.CreatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "更新 workspace")
	}
	return &ws, nil
}

// UpdateWorkspaceSettings 更新 workspace 设置，支持部分字段可选更新。
// 引入动机：Phase6 要求 workspace admin 安全更新 revision_retention_days、
// revision_max_count、max_document_size_bytes，同时保持 display_name/description 兼容。
// 参数中的 nil 指针表示保持不变，非 nil 表示更新为指针值。
func (r *PGRepository) UpdateWorkspaceSettings(ctx context.Context, workspaceID string, opts UpdateWorkspaceSettingsOptions) (*Workspace, error) {
	var displayName, description interface{}
	var revisionRetentionDays, revisionMaxCount, maxDocumentSizeBytes interface{}
	if opts.DisplayName != nil {
		displayName = *opts.DisplayName
	}
	if opts.Description != nil {
		description = *opts.Description
	}
	if opts.RevisionRetentionDays != nil {
		revisionRetentionDays = *opts.RevisionRetentionDays
	}
	if opts.RevisionMaxCount != nil {
		revisionMaxCount = *opts.RevisionMaxCount
	}
	if opts.MaxDocumentSizeBytes != nil {
		maxDocumentSizeBytes = *opts.MaxDocumentSizeBytes
	}

	var ws Workspace
	err := r.db.QueryRowContext(ctx,
		`UPDATE workspaces SET
			display_name = COALESCE($1, display_name),
			description = COALESCE($2, description),
			revision_retention_days = COALESCE($3, revision_retention_days),
			revision_max_count = COALESCE($4, revision_max_count),
			max_document_size_bytes = COALESCE($5, max_document_size_bytes),
			updated_at = now()
		 WHERE id = $6
		 RETURNING id, name, display_name, description, status, revision_retention_days, revision_max_count, max_document_size_bytes, created_by`,
		displayName, description, revisionRetentionDays, revisionMaxCount, maxDocumentSizeBytes, workspaceID,
	).Scan(
		&ws.ID, &ws.Name, &ws.DisplayName, &ws.Description,
		&ws.Status, &ws.RevisionRetentionDays, &ws.RevisionMaxCount, &ws.MaxDocumentSizeBytes, &ws.CreatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "更新 workspace 设置")
	}
	return &ws, nil
}

// ArchiveWorkspace 将 workspace 状态设置为 archived。
func (r *PGRepository) ArchiveWorkspace(ctx context.Context, workspaceID string) (*Workspace, error) {
	var ws Workspace
	err := r.db.QueryRowContext(ctx,
		`UPDATE workspaces SET status = $1, updated_at = now() WHERE id = $2
		 RETURNING id, name, display_name, description, status, revision_retention_days, revision_max_count, max_document_size_bytes, created_by`,
		StatusArchived, workspaceID,
	).Scan(
		&ws.ID, &ws.Name, &ws.DisplayName, &ws.Description,
		&ws.Status, &ws.RevisionRetentionDays, &ws.RevisionMaxCount, &ws.MaxDocumentSizeBytes, &ws.CreatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "归档 workspace")
	}
	return &ws, nil
}

// GetMemberRole 查询用户在指定 workspace 中的角色。
// 非成员返回 sql.ErrNoRows。
func (r *PGRepository) GetMemberRole(ctx context.Context, workspaceID, userID string) (string, error) {
	var role string
	err := r.db.QueryRowContext(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID,
	).Scan(&role)
	if err != nil {
		return "", mapDBError(err, "查询成员角色")
	}
	return role, nil
}

// ListMembers 查询指定 workspace 的成员列表（分页）。
func (r *PGRepository) ListMembers(ctx context.Context, workspaceID string, limit, offset int) (*ListMembersResult, error) {
	// 查询总数
	var total int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询成员总数")
	}

	// 查询分页数据
	rows, err := r.db.QueryContext(ctx,
		`SELECT wm.id, wm.workspace_id, wm.user_id, u.username, u.email, wm.role
		 FROM workspace_members wm
		 INNER JOIN users u ON wm.user_id = u.id
		 WHERE wm.workspace_id = $1
		 ORDER BY wm.created_at ASC
		 LIMIT $2 OFFSET $3`,
		workspaceID, limit, offset,
	)
	if err != nil {
		return nil, mapDBError(err, "查询成员列表")
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.UserID, &m.Username, &m.Email, &m.Role); err != nil {
			return nil, fmt.Errorf("扫描成员行: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历成员结果集: %w", err)
	}

	return &ListMembersResult{Members: members, Total: total}, nil
}

// AddMember 向指定 workspace 添加成员。
func (r *PGRepository) AddMember(ctx context.Context, workspaceID, userID, role string) (*Member, error) {
	var m Member
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role)
		 VALUES ($1, $2, $3)
		 RETURNING id, workspace_id, user_id`,
		workspaceID, userID, role,
	).Scan(&m.ID, &m.WorkspaceID, &m.UserID)
	if err != nil {
		return nil, mapDBError(err, "添加成员")
	}

	// 查询关联用户信息
	user, err := r.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("添加成员后查询用户信息: %w", err)
	}
	m.Username = user.Username
	m.Email = user.Email
	m.Role = role

	return &m, nil
}

// UpdateMemberRole 更新指定 workspace 中成员的角色。
func (r *PGRepository) UpdateMemberRole(ctx context.Context, workspaceID, userID, newRole string) (*Member, error) {
	var m Member
	err := r.db.QueryRowContext(ctx,
		`UPDATE workspace_members SET role = $1, updated_at = now()
		 WHERE workspace_id = $2 AND user_id = $3
		 RETURNING id, workspace_id, user_id, role`,
		newRole, workspaceID, userID,
	).Scan(&m.ID, &m.WorkspaceID, &m.UserID, &m.Role)
	if err != nil {
		return nil, mapDBError(err, "更新成员角色")
	}

	// 查询关联用户信息
	user, err := r.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("更新成员角色后查询用户信息: %w", err)
	}
	m.Username = user.Username
	m.Email = user.Email

	return &m, nil
}

// RemoveMember 从指定 workspace 中移除成员。
func (r *PGRepository) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID,
	)
	if err != nil {
		return mapDBError(err, "移除成员")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取移除成员影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountOwners 查询指定 workspace 中拥有 owner 角色的成员数量。
func (r *PGRepository) CountOwners(ctx context.Context, workspaceID string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = $1 AND role = $2`,
		workspaceID, RoleOwner,
	).Scan(&count)
	if err != nil {
		return 0, mapDBError(err, "查询 owner 数量")
	}
	return count, nil
}

// GetUserByID 根据 ID 查询用户基本信息（不含密码哈希）。
func (r *PGRepository) GetUserByID(ctx context.Context, userID string) (*Member, error) {
	var m Member
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, email FROM users WHERE id = $1`,
		userID,
	).Scan(&m.UserID, &m.Username, &m.Email)
	if err != nil {
		return nil, mapDBError(err, "根据 ID 查询用户")
	}
	return &m, nil
}

// GetUserByUsername 根据用户名查询用户基本信息。
func (r *PGRepository) GetUserByUsername(ctx context.Context, username string) (*Member, error) {
	var member Member
	err := r.db.QueryRowContext(ctx,
		`SELECT id, username, email FROM users WHERE username = $1`, username,
	).Scan(&member.UserID, &member.Username, &member.Email)
	if err != nil {
		return nil, mapDBError(err, "根据用户名查询用户")
	}
	return &member, nil
}

// ListMemberCandidates 查询尚未加入指定 workspace 的用户候选。
func (r *PGRepository) ListMemberCandidates(ctx context.Context, workspaceID, query string, limit int) ([]Member, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT u.id, u.username, u.email
		 FROM users u
		 WHERE (u.username ILIKE $1 OR u.email ILIKE $1)
		   AND NOT EXISTS (
				SELECT 1 FROM workspace_members wm
				WHERE wm.workspace_id = $2 AND wm.user_id = u.id
		   )
		 ORDER BY CASE WHEN lower(u.username) = lower($3) THEN 0 ELSE 1 END, u.username ASC
		 LIMIT $4`,
		"%"+query+"%", workspaceID, query, limit,
	)
	if err != nil {
		return nil, mapDBError(err, "查询成员候选")
	}
	defer rows.Close()

	candidates := make([]Member, 0)
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.UserID, &member.Username, &member.Email); err != nil {
			return nil, fmt.Errorf("扫描成员候选: %w", err)
		}
		candidates = append(candidates, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历成员候选: %w", err)
	}
	return candidates, nil
}

// ListAllWorkspaces 查询全部 workspace 列表（分页），供 system_admin 使用。
// filter 中零值字段不参与过滤；Status 精确匹配，Query 对 name/display_name 做 ILIKE 模糊匹配。
// WHERE 条件参数化拼接，COUNT 与 SELECT 共用同一组条件保证 total 与分页口径一致。
func (r *PGRepository) ListAllWorkspaces(ctx context.Context, filter AdminWorkspaceFilter, limit, offset int) (*ListWorkspacesResult, error) {
	conditions := []string{"1=1"}
	args := []interface{}{}
	argIdx := 1

	if filter.Status != "" {
		conditions = append(conditions, fmt.Sprintf("workspaces.status = $%d", argIdx))
		args = append(args, filter.Status)
		argIdx++
	}
	if filter.Query != "" {
		conditions = append(conditions, fmt.Sprintf("(workspaces.name ILIKE $%d OR workspaces.display_name ILIKE $%d)", argIdx, argIdx))
		args = append(args, "%"+filter.Query+"%")
		argIdx++
	}

	whereClause := joinConditions(conditions)

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM workspaces WHERE %s", whereClause)
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, mapDBError(err, "查询 workspace 总数")
	}

	// LEFT JOIN users 取创建者用户名，供 admin 列表直接显示可读名称；
	// 使用 LEFT JOIN 保证 created_by 指向已删除用户时 workspace 行不会丢失。
	listQuery := fmt.Sprintf(
		`SELECT workspaces.id, workspaces.name, workspaces.display_name, workspaces.description, workspaces.status, workspaces.revision_retention_days, workspaces.revision_max_count, workspaces.max_document_size_bytes, workspaces.created_by, COALESCE(u.username, '') AS created_by_username
		 FROM workspaces
		 LEFT JOIN users u ON u.id = workspaces.created_by
		 WHERE %s ORDER BY workspaces.created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询全部 workspace 列表")
	}
	defer rows.Close()

	var workspaces []Workspace
	for rows.Next() {
		var ws Workspace
		if err := rows.Scan(
			&ws.ID, &ws.Name, &ws.DisplayName, &ws.Description,
			&ws.Status, &ws.RevisionRetentionDays, &ws.RevisionMaxCount, &ws.MaxDocumentSizeBytes, &ws.CreatedBy,
			&ws.CreatedByUsername,
		); err != nil {
			return nil, fmt.Errorf("扫描 workspace 行: %w", err)
		}
		workspaces = append(workspaces, ws)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 workspace 结果集: %w", err)
	}

	return &ListWorkspacesResult{Workspaces: workspaces, Total: total}, nil
}

// ListAllUsers 查询全部用户列表（分页），供 system_admin 使用。
func (r *PGRepository) ListAllUsers(ctx context.Context, limit, offset int) (*ListMembersResult, error) {
	var total int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询用户总数")
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, username, email FROM users ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, mapDBError(err, "查询全部用户列表")
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username, &m.Email); err != nil {
			return nil, fmt.Errorf("扫描用户行: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户结果集: %w", err)
	}

	return &ListMembersResult{Members: members, Total: total}, nil
}

// ListAllUsersWithSystemInfo 查询全部用户列表（含系统角色和 workspace:create 权限），单条参数化 SQL。
// 引入动机：admin ListUsers API 需要返回用户基础信息和系统角色/权限，
// 逐用户查询系统信息造成 N+1，改为单条 SQL 一次性返回所有字段。
// filter 中零值字段不参与过滤；SystemRole 精确匹配，Query 对 username/email 做 ILIKE 模糊匹配。
func (r *PGRepository) ListAllUsersWithSystemInfo(ctx context.Context, filter AdminUserFilter, limit, offset int) (*ListAdminUsersResult, error) {
	conditions := []string{"1=1"}
	args := []interface{}{}
	argIdx := 1

	if filter.SystemRole != "" {
		conditions = append(conditions, fmt.Sprintf("system_role = $%d", argIdx))
		args = append(args, filter.SystemRole)
		argIdx++
	}
	if filter.Query != "" {
		conditions = append(conditions, fmt.Sprintf("(username ILIKE $%d OR email ILIKE $%d)", argIdx, argIdx))
		args = append(args, "%"+filter.Query+"%")
		argIdx++
	}

	whereClause := joinConditions(conditions)

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM users WHERE %s", whereClause)
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, mapDBError(err, "查询用户总数")
	}

	listQuery := fmt.Sprintf(
		`SELECT id, username, email, system_role, workspace_create_perm
		 FROM users WHERE %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询全部用户列表（含系统信息）")
	}
	defer rows.Close()

	var users []AdminUser
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.UserID, &u.Username, &u.Email, &u.SystemRole, &u.WorkspaceCreatePerm); err != nil {
			return nil, fmt.Errorf("扫描用户行（含系统信息）: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户结果集（含系统信息）: %w", err)
	}

	return &ListAdminUsersResult{Users: users, Total: total}, nil
}

// UpdateUserSystemRole 更新用户的系统角色。
func (r *PGRepository) UpdateUserSystemRole(ctx context.Context, userID, systemRole string) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE users SET system_role = $1, updated_at = now() WHERE id = $2`,
		systemRole, userID,
	)
	if err != nil {
		return mapDBError(err, "更新用户系统角色")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取更新用户系统角色影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateUserWorkspaceCreatePerm 更新用户的 workspace:create 权限。
func (r *PGRepository) UpdateUserWorkspaceCreatePerm(ctx context.Context, userID string, perm bool) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE users SET workspace_create_perm = $1, updated_at = now() WHERE id = $2`,
		perm, userID,
	)
	if err != nil {
		return mapDBError(err, "更新用户 workspace:create 权限")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取更新用户 workspace:create 权限影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetUserSystemInfo 查询用户的系统角色和 workspace:create 权限。
func (r *PGRepository) GetUserSystemInfo(ctx context.Context, userID string) (string, bool, error) {
	var systemRole string
	var workspaceCreatePerm bool
	err := r.db.QueryRowContext(ctx,
		`SELECT system_role, workspace_create_perm FROM users WHERE id = $1`,
		userID,
	).Scan(&systemRole, &workspaceCreatePerm)
	if err != nil {
		return "", false, mapDBError(err, "查询用户系统信息")
	}
	return systemRole, workspaceCreatePerm, nil
}

// GetWorkspaceStats 查询 workspace 的文档/成员/近期 revision 统计。
// 引入动机：Phase6 WP2 工作台首页需要聚合展示 workspace 核心指标。
func (r *PGRepository) GetWorkspaceStats(ctx context.Context, workspaceID string) (*WorkspaceStats, error) {
	stats := &WorkspaceStats{
		RecentRevisions: []RecentRevision{},
	}

	queries := []struct {
		dest  *int
		query string
		label string
	}{
		{&stats.TotalDocuments, `SELECT COUNT(*) FROM documents WHERE workspace_id = $1`, "total"},
		{&stats.ActiveDocuments, `SELECT COUNT(*) FROM documents WHERE workspace_id = $1 AND status = 'active'`, "active"},
		{&stats.DraftDocuments, `SELECT COUNT(*) FROM documents WHERE workspace_id = $1 AND status = 'draft'`, "draft"},
		{&stats.ArchivedDocuments, `SELECT COUNT(*) FROM documents WHERE workspace_id = $1 AND status = 'archived'`, "archived"},
		{&stats.MemberCount, `SELECT COUNT(*) FROM workspace_members WHERE workspace_id = $1`, "members"},
	}
	for _, q := range queries {
		if err := r.db.QueryRowContext(ctx, q.query, workspaceID).Scan(q.dest); err != nil {
			return nil, fmt.Errorf("查询 workspace %s 统计 (%s): %w", workspaceID, q.label, err)
		}
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT r.path, r.title, r.revision_number,
		        to_char(r.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM revisions r
		 JOIN documents d ON d.id = r.document_id
		 WHERE d.workspace_id = $1
		 ORDER BY r.created_at DESC
		 LIMIT 5`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("查询 workspace %s 近期 revisions: %w", workspaceID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var rev RecentRevision
		if err := rows.Scan(&rev.Path, &rev.Title, &rev.RevisionNumber, &rev.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描近期 revision: %w", err)
		}
		stats.RecentRevisions = append(stats.RecentRevisions, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历近期 revisions: %w", err)
	}

	return stats, nil
}

// mapDBError 将 database/sql 错误映射为带上下文的错误信息。
// 引入动机：统一处理 sql.ErrNoRows 和 pgconn.PgError，避免在每处调用重复判断。
func mapDBError(err error, context string) error {
	if err == sql.ErrNoRows {
		return err
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return fmt.Errorf("%s: PostgreSQL 错误 %s: %w", context, pgErr.Code, err)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// joinConditions 用 AND 连接 WHERE 条件。
// 引入动机：参数化拼接可选筛选条件时统一连接符，避免在每个分支重复处理 " AND "。
func joinConditions(conditions []string) string {
	result := ""
	for i, c := range conditions {
		if i > 0 {
			result += " AND "
		}
		result += c
	}
	return result
}
