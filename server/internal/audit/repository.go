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

	// List 查询审计日志列表（分页），按创建时间降序。
	// 引入动机：audit API 供 system_admin 查看审计日志。
	List(ctx context.Context, limit, offset int) (*ListResult, error)
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

// List 查询审计日志列表（分页），按创建时间降序。
func (r *PGRepository) List(ctx context.Context, limit, offset int) (*ListResult, error) {
	var total int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询审计日志总数")
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, COALESCE(user_id::text, ''), COALESCE(workspace_id::text, ''),
		        action, COALESCE(resource_type, ''), COALESCE(resource_id::text, ''),
		        COALESCE(detail::text, 'null'), COALESCE(request_id, ''),
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM audit_logs ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
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
