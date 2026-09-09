// Package workspace 实现 Workspace 管理、成员管理、后端 Workspace RBAC 强制。
//
// 引入动机：design/00-MASTER.md §Workspace 要求一个 Workspace = 一个独立项目，
// 禁止跨 Workspace 访问。design/04-WEB-API.md §RBAC 定义了系统角色（system_admin / user）
// 和 Workspace 角色（owner / admin / editor / viewer）及其能力矩阵。
// 本包提供 Workspace CRUD、成员管理、基于服务端 membership 的权限判断，
// 以及供下一阶段（Document/Search）复用的 RequireWorkspacePermission 边界。
//
// 设计原则：
//   - HTTP handler 仅做解析/响应，领域规则和 SQL 访问分离
//   - 所有 workspace-scoped 查询和变更在 SQL 级按 workspace_id 限定
//   - 权限判断基于经认证 identity + 服务端 membership repository，不接受客户端 role
//   - 不出现 XxxService / XxxManager / XxxController
package workspace

// WorkspaceRole 是 Workspace 内角色常量。
// 引入动机：design/04-WEB-API.md §RBAC 定义四种 Workspace 角色，
// 需要统一常量供权限判断和成员管理使用。
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// SystemRole 是系统级角色常量。
// 引入动机：design/04-WEB-API.md §RBAC 定义两种系统角色，
// admin API 仅 system_admin 可访问。
const (
	SystemRoleAdmin = "system_admin"
	SystemRoleUser  = "user"
)

// WorkspaceStatus 是 Workspace 状态常量。
// 引入动机：workspaces 表 status 字段 CHECK 约束为 active / archived。
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// --- workspace settings 安全边界 ---
// 引入动机：Phase6 要求 workspace admin 更新 revision_retention_days、revision_max_count、
// max_document_size_bytes 时必须在服务端校验可选与范围。
const (
	// MinRevisionRetentionDays 是最小允许值，至少保留 1 天。
	MinRevisionRetentionDays = 1
	// MaxRevisionRetentionDays 是最大允许值，最多保留 10 年。
	MaxRevisionRetentionDays = 3650

	// MinRevisionMaxCount 是最小允许版本数，至少保留 1 个版本。
	MinRevisionMaxCount = 1
	// MaxRevisionMaxCount 是最大允许版本数，防止无意义的大值。
	MaxRevisionMaxCount = 10000

	// MinDocumentSizeBytes 是最小允许文档大小，1KB。
	MinDocumentSizeBytes = 1024
	// MaxDocumentSizeBytes 是最大允许文档大小，100MB。
	MaxDocumentSizeBytes = 104857600
)

// UpdateWorkspaceSettingsOptions 是安全更新 workspace 设置的参数。
// 引入动机：display_name、description 与三项 settings 均需要 optional，
// 服务端根据非 nil 字段执行部分更新。
// 所有指针字段缺失时保持原值，显式 null 时视为空值（description 允许为空）。
type UpdateWorkspaceSettingsOptions struct {
	DisplayName           *string
	Description           *string
	RevisionRetentionDays *int
	RevisionMaxCount      *int
	MaxDocumentSizeBytes  *int
}

// Workspace 是从数据库读取的 Workspace 记录。
// 引入动机：list/get/create/archive 等 API 需要返回 Workspace 元数据，
// repository 需要结构体映射数据库行。
type Workspace struct {
	ID                     string
	Name                   string
	DisplayName            string
	Description            string
	Status                 string
	RevisionRetentionDays  int
	RevisionMaxCount       int
	MaxDocumentSizeBytes   int
	CreatedBy              string
}

// AdminUser 是 admin 用户列表返回的完整用户信息（含系统角色和 workspace:create 权限）。
// 引入动机：admin ListUsers API 需要返回用户基础信息和系统角色/权限，
// 使用单条 SQL 查询获取全部字段，避免逐用户 N+1 查询系统信息。
type AdminUser struct {
	UserID              string
	Username            string
	Email               string
	SystemRole          string
	WorkspaceCreatePerm bool
}

// ListAdminUsersResult 是 admin 用户列表查询结果（含系统信息）。
// 引入动机：admin ListUsers API 需要返回分页数据和总数，
// 且每条记录包含系统角色和权限信息，由单条参数化 SQL 一次性返回。
type ListAdminUsersResult struct {
	Users []AdminUser
	Total int
}

// Member 是从数据库读取的 workspace_members 记录，关联用户基本信息。
// 引入动机：成员列表 API 需要返回成员的用户信息和角色；
// 权限判断需要根据 user_id + workspace_id 查询成员角色。
type Member struct {
	ID        string
	WorkspaceID string
	UserID    string
	Username  string
	Email     string
	Role      string
}

// Permission 是 Workspace 级权限能力常量。
// 引入动机：design/04-WEB-API.md §RBAC 定义了 viewer/editor/admin/owner 的能力矩阵，
// 需要统一的权限标识供 RequireWorkspacePermission 和下一阶段 Document/Search 使用。
// 本阶段尚无 Document/Search endpoint，但这些常量已暴露供下一阶段调用。
const (
	// PermRead 允许读取文档（viewer+）。
	PermRead = "read"
	// PermSearch 允许搜索（viewer+）。
	PermSearch = "search"
	// PermHistory 允许查看 revision 历史（viewer+）。
	PermHistory = "history"
	// PermCreate 允许创建文档（editor+）。
	PermCreate = "create"
	// PermUpdate 允许更新文档（editor+）。
	PermUpdate = "update"
	// PermMove 允许移动文档（editor+）。
	PermMove = "move"
	// PermArchive 允许归档文档（admin+）。
	PermArchive = "archive"
	// PermMemberManage 允许成员管理（admin+）。
	PermMemberManage = "member_manage"
	// PermSettings 允许修改 Workspace 设置（admin+）。
	PermSettings = "settings"
	// PermTransfer 允许转移 Workspace 所有权（owner only）。
	PermTransfer = "transfer"
	// PermPurge 允许永久删除 Workspace（owner only）。
	PermPurge = "purge"
)

// rolePermissionMap 定义每种 Workspace 角色拥有的权限集合。
// 引入动机：design/04-WEB-API.md §RBAC 明确定义了角色能力矩阵，
// 需要一个确定性的映射供 HasPermission 查询。
//
// 能力层级（上层包含下层全部权限）：
//   viewer:  read, search, history
//   editor:  viewer + create, update, move
//   admin:   editor + archive, member_manage, settings
//   owner:   admin + transfer, purge
var rolePermissionMap = map[string]map[string]bool{
	RoleViewer: {
		PermRead: true, PermSearch: true, PermHistory: true,
	},
	RoleEditor: {
		PermRead: true, PermSearch: true, PermHistory: true,
		PermCreate: true, PermUpdate: true, PermMove: true,
	},
	RoleAdmin: {
		PermRead: true, PermSearch: true, PermHistory: true,
		PermCreate: true, PermUpdate: true, PermMove: true,
		PermArchive: true, PermMemberManage: true, PermSettings: true,
	},
	RoleOwner: {
		PermRead: true, PermSearch: true, PermHistory: true,
		PermCreate: true, PermUpdate: true, PermMove: true,
		PermArchive: true, PermMemberManage: true, PermSettings: true,
		PermTransfer: true, PermPurge: true,
	},
}

// HasPermission 判断给定角色是否拥有指定权限。
// 引入动机：RequireWorkspacePermission 和 handler 需要根据服务端查询到的
// 成员角色判断是否有权执行操作。不接受客户端声明的角色。
//
// 参数：
//   - role：从服务端 membership repository 查询到的角色
//   - permission：要检查的权限常量
//
// 返回 true 表示角色拥有该权限。
func HasPermission(role, permission string) bool {
	perms, ok := rolePermissionMap[role]
	if !ok {
		return false
	}
	return perms[permission]
}

// IsValidRole 判断给定字符串是否为合法的 Workspace 角色。
// 引入动机：添加/修改成员时需要验证请求中的 role 字段。
func IsValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleEditor, RoleViewer:
		return true
	default:
		return false
	}
}

// IsValidWorkspaceStatus 判断给定字符串是否为合法的 Workspace 状态。
// 引入动机：更新 workspace 时需要验证 status 字段（如果允许修改）。
func IsValidWorkspaceStatus(status string) bool {
	return status == StatusActive || status == StatusArchived
}
