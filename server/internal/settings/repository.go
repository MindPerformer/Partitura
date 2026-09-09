// Package settings 实现可安全持久化的业务服务配置管理。
//
// 引入动机：Phase6 WP3 要求 system_admin 通过 Web 管理业务配置，
// 同时禁止把基础设施/secret（DSN、DB 密码、API key、cookie/CSRF、Argon2、HTTP bind/Compose）
// 作为普通可读配置暴露。
//
// 设计原则：
//   - system_settings 表只存储业务运行参数，不存储 secret。
//   - repository 提供 allowlist 感知的基础 CRUD，业务验证由 logic 层完成。
//   - 更新必须记录 updated_by 和 updated_at，便于审计。
package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Record 是 system_settings 表的持久化记录。
type Record struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	ValueType   string `json:"value_type"`
	Category    string `json:"category"`
	Description string `json:"description"`
	IsSecret    bool   `json:"is_secret"`
	UpdatedBy   string `json:"updated_by"`
	UpdatedAt   string `json:"updated_at"`
}

// Repository 定义 settings 数据访问接口。
type Repository interface {
	// ListNonSecret 返回所有非 secret 业务配置和运行状态（不含 category='secret'）。
	ListNonSecret(ctx context.Context) ([]Record, error)

	// Get 查询单个 setting（任何 category）。
	Get(ctx context.Context, key string) (*Record, error)

	// Update 更新单个 setting 的值，并记录更新者/时间。
	Update(ctx context.Context, key, value, valueType, updatedBy string) error
}

// PGRepository 是 Repository 的 PostgreSQL 实现。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// ListNonSecret 返回所有非 secret 的业务配置和运行状态记录。
// 引入动机：GET /api/admin/config 只返回可展示、可编辑的业务配置，
// 不暴露 category='secret' 的记录。
func (r *PGRepository) ListNonSecret(ctx context.Context) ([]Record, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT key, value, value_type, category, description, is_secret, COALESCE(updated_by::text, ''), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM system_settings
		 WHERE category != 'secret'
		 ORDER BY category, key`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询 settings 列表: %w", err)
	}
	defer rows.Close()

	records := []Record{}
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.Key, &rec.Value, &rec.ValueType, &rec.Category, &rec.Description, &rec.IsSecret, &rec.UpdatedBy, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描 setting 行: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 settings 结果集: %w", err)
	}
	return records, nil
}

// Get 查询单个 setting。
func (r *PGRepository) Get(ctx context.Context, key string) (*Record, error) {
	var rec Record
	err := r.db.QueryRowContext(ctx,
		`SELECT key, value, value_type, category, description, is_secret, COALESCE(updated_by::text, ''), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM system_settings WHERE key = $1`,
		key,
	).Scan(&rec.Key, &rec.Value, &rec.ValueType, &rec.Category, &rec.Description, &rec.IsSecret, &rec.UpdatedBy, &rec.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("查询 setting %s: %w", key, err)
	}
	return &rec, nil
}

// Update 更新单个 setting 的值和元数据（使用乐观 category 检查，避免误改 secret）。
func (r *PGRepository) Update(ctx context.Context, key, value, valueType, updatedBy string) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE system_settings
		 SET value = $1, value_type = $2, updated_by = $3, updated_at = now()
		 WHERE key = $4 AND category != 'secret'`,
		value, valueType, updatedBy, key,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return fmt.Errorf("更新 setting %s: PostgreSQL 错误 %s: %w", key, pgErr.Code, err)
		}
		return fmt.Errorf("更新 setting %s: %w", key, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}
