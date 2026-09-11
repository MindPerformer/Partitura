// Package audit 实现审计日志的写入和查询。
//
// 引入动机：design/04-WEB-API.md §Admin 列出 audit API，
// design/06-IMPLEMENTATION.md Phase 1 要求 audit 日志记录。
// 每个成功或拒绝的安全敏感/状态变更操作应产生可追溯审计记录，
// 至少记 actor、workspace（适用时）、action、resource type/id、request ID、
// 结构化且不含 password/token 的 detail。
//
// 安全原则：
//   - 审计日志仅 system_admin 可读取
//   - detail 字段为 JSONB，写入前必须确保不含 password/token 等敏感信息
//   - 错误不吞没，通过 slog 记录
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Entry 是审计日志记录，对应 audit_logs 表的一行。
// 引入动机：handler 在执行安全敏感操作后需要写入审计记录，
// audit API 需要返回审计日志列表。
type Entry struct {
	ID           int64
	UserID       string
	WorkspaceID  string
	Action       string
	ResourceType string
	ResourceID   string
	Detail       json.RawMessage
	RequestID    string
	CreatedAt    string
}

// ListResult 是审计日志查询结果（分页）。
// 引入动机：audit API 需要返回分页数据和总数。
type ListResult struct {
	Entries []Entry
	Total   int
}

// ListFilter 是审计日志列表的可选筛选条件。
// 引入动机：前端审计发现 audit 日志缺少筛选能力——管理员需要按操作者、动作、
// 资源类型、workspace 或时间范围收窄结果，而不是只能全量翻页。
// 所有字段均为可选：零值表示该维度不参与过滤。
type ListFilter struct {
	// UserID 按操作者 UUID 精确匹配（audit_logs.user_id）。
	UserID string
	// Action 按动作标识精确匹配（如 "admin.user.create"）。
	Action string
	// ResourceType 按资源类型精确匹配（如 "workspace"）。
	ResourceType string
	// WorkspaceID 按关联 workspace UUID 精确匹配。
	WorkspaceID string
	// From 只返回 created_at >= From 的记录（RFC3339 解析后的时间）。
	From *time.Time
	// To 只返回 created_at <= To 的记录。
	To *time.Time
}

// Repository 定义审计日志的数据访问接口。
// 引入动机：领域逻辑和 HTTP handler 依赖此接口而非具体 PG 实现，
// 便于测试时注入 mock。
type Repository interface {
	// Record 写入一条审计日志。
	// 引入动机：每个安全敏感/状态变更操作需要产生审计记录。
	//
	// 参数：
	//   - ctx：请求 context
	//   - userID：操作者 UUID（可为空，如匿名请求被拒绝时）
	//   - workspaceID：关联 workspace UUID（可为空）
	//   - action：操作标识（如 "workspace.create", "workspace.member.add"）
	//   - resourceType：资源类型（如 "workspace", "workspace_member"）
	//   - resourceID：资源 UUID（可为空）
	//   - detail：结构化详情（JSONB，必须不含 password/token）
	//   - requestID：请求追踪 ID
	Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error

	// List 查询审计日志列表（分页），按创建时间降序，可按 filter 收窄。
	// 引入动机：audit API 供 system_admin 查看审计日志；
	// filter 中零值字段不参与过滤，nil filter 等价于无筛选。
	List(ctx context.Context, filter ListFilter, limit, offset int) (*ListResult, error)
}

// PGRepository 是 Repository 接口的 PostgreSQL 实现。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// Record 写入一条审计日志。
func (r *PGRepository) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	// 处理可空字段
	var userIDVal, workspaceIDVal, resourceIDVal interface{}
	if userID != "" {
		userIDVal = userID
	}
	if workspaceID != "" {
		workspaceIDVal = workspaceID
	}
	if resourceID != "" {
		resourceIDVal = resourceID
	}

	var detailVal interface{}
	if len(detail) > 0 {
		detailVal = []byte(detail)
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO audit_logs (user_id, workspace_id, action, resource_type, resource_id, detail, request_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		userIDVal, workspaceIDVal, action, resourceType, resourceIDVal, detailVal, requestID,
	)
	if err != nil {
		return mapDBError(err, "写入审计日志")
	}

	slog.Debug("审计日志已记录", "action", action, "resource_type", resourceType, "resource_id", resourceID)
	return nil
}

// List 查询审计日志列表（分页），按创建时间降序，可按 filter 收窄。
//
// WHERE 条件通过参数化占位符拼接（参照 evaluation 包的 conditions 模式），
// 所有用户可控值都走 $N 参数，杜绝 SQL 注入。COUNT 与 SELECT 使用同一组
// 条件与参数，保证 total 与当前页数据来自同一筛选口径。
func (r *PGRepository) List(ctx context.Context, filter ListFilter, limit, offset int) (*ListResult, error) {
	conditions := []string{"1=1"}
	args := []interface{}{}
	argIdx := 1

	if filter.UserID != "" {
		conditions = append(conditions, fmt.Sprintf("user_id = $%d", argIdx))
		args = append(args, filter.UserID)
		argIdx++
	}
	if filter.Action != "" {
		conditions = append(conditions, fmt.Sprintf("action = $%d", argIdx))
		args = append(args, filter.Action)
		argIdx++
	}
	if filter.ResourceType != "" {
		conditions = append(conditions, fmt.Sprintf("resource_type = $%d", argIdx))
		args = append(args, filter.ResourceType)
		argIdx++
	}
	if filter.WorkspaceID != "" {
		conditions = append(conditions, fmt.Sprintf("workspace_id = $%d", argIdx))
		args = append(args, filter.WorkspaceID)
		argIdx++
	}
	if filter.From != nil {
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", argIdx))
		args = append(args, *filter.From)
		argIdx++
	}
	if filter.To != nil {
		conditions = append(conditions, fmt.Sprintf("created_at <= $%d", argIdx))
		args = append(args, *filter.To)
		argIdx++
	}

	whereClause := joinConditions(conditions)

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM audit_logs WHERE %s", whereClause)
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, mapDBError(err, "查询审计日志总数")
	}

	listQuery := fmt.Sprintf(
		`SELECT id, COALESCE(user_id::text, ''), COALESCE(workspace_id::text, ''),
		        action, COALESCE(resource_type, ''), COALESCE(resource_id::text, ''),
		        COALESCE(detail::text, 'null'), COALESCE(request_id, ''),
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM audit_logs WHERE %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询审计日志列表")
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var e Entry
		var detailStr string
		if err := rows.Scan(&e.ID, &e.UserID, &e.WorkspaceID, &e.Action,
			&e.ResourceType, &e.ResourceID, &detailStr, &e.RequestID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描审计日志行: %w", err)
		}
		if detailStr != "" && detailStr != "null" {
			e.Detail = json.RawMessage(detailStr)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历审计日志结果集: %w", err)
	}

	return &ListResult{Entries: entries, Total: total}, nil
}

// joinConditions 用 AND 连接 WHERE 条件。
// 引入动机：参数化拼接筛选条件时统一连接符，避免在每个分支重复处理 " AND "。
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

// mapDBError 将 database/sql 错误映射为带上下文的错误信息。
func mapDBError(err error, context string) error {
	if err == sql.ErrNoRows {
		return err
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return fmt.Errorf("%s: PostgreSQL 错误 %s: %w", context, pgErr.Code, err)
	}
	return fmt.Errorf("%s: %w", context, err)
}
