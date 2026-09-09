// repository.go 实现 auth 模块的 PostgreSQL 数据访问层。
//
// 引入动机：认证领域逻辑需要访问 users、sessions、device_sessions 三张表。
// 将数据访问集中到 repository，使领域逻辑和 HTTP handler 不直接依赖 SQL，
// 便于测试和未来替换存储实现。
//
// 设计原则：
//   - 接口定义与实现分离，领域逻辑依赖接口而非具体 PG 实现
//   - 所有查询使用 context 传递超时
//   - 错误包装具体上下文，不吞错
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// User 是从数据库读取的用户记录。
// 引入动机：login 流程需要根据用户名查询用户及其密码哈希；
// middleware 需要根据 user_id 查询用户基本信息以构建 Identity。
type User struct {
	ID                  string
	Username            string
	Email               string
	PasswordHash        string
	SystemRole          string
	WorkspaceCreatePerm bool
}

// SessionRecord 是从数据库读取的 session 记录。
// 引入动机：middleware 验证 session 时需要读取 token_hash 对应的 session 信息，
// 包括过期时间和撤销状态。
type SessionRecord struct {
	ID            string
	UserID        string
	TokenHash     string
	CSRFTokenHash string
	ExpiresAt     time.Time
	RevokedAt     sql.NullTime
	CreatedAt     time.Time
}

// DeviceSessionRecord 是从数据库读取的 device session 记录。
// 引入动机：middleware 验证 bearer access token 和 refresh token 流程需要读取
// device session 信息，包括过期时间和撤销状态。
type DeviceSessionRecord struct {
	ID                string
	UserID            string
	DeviceName        string
	AccessTokenHash   string
	RefreshTokenHash  string
	ExpiresAt         time.Time
	RefreshExpiresAt  time.Time
	RevokedAt         sql.NullTime
	CreatedAt         time.Time
}

// ErrUsersAlreadyExist 表示 users 表中已存在用户，Bootstrap 不可用。
// 引入动机：CreateUserIfNoneExist 在 users 表非空时返回此错误，
// 替代原先 CountUsers + CreateUser 的两步操作，消除 TOCTOU 竞态。
var ErrUsersAlreadyExist = errors.New("users 表已存在用户，Bootstrap 不可用")

// Repository 定义 auth 模块所需的数据访问接口。
// 引入动机：领域逻辑和 HTTP handler 依赖此接口而非具体 PG 实现，
// 便于测试时注入 mock 或使用替代存储。
type Repository interface {
	// GetUserByUsername 根据用户名查询用户。用户不存在返回 sql.ErrNoRows。
	GetUserByUsername(ctx context.Context, username string) (*User, error)

	// GetUserByID 根据 ID 查询用户。用户不存在返回 sql.ErrNoRows。
	GetUserByID(ctx context.Context, id string) (*User, error)

	// CreateSession 插入一条新的 session 记录。
	CreateSession(ctx context.Context, userID, tokenHash, csrfTokenHash string, expiresAt time.Time) (sessionID string, err error)

	// GetSessionByTokenHash 根据 token 哈希查询 session。不存在返回 sql.ErrNoRows。
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*SessionRecord, error)

	// RevokeSession 根据 session ID 将 session 标记为已撤销（设置 revoked_at）。
	RevokeSession(ctx context.Context, sessionID string) error

	// CreateDeviceSession 插入一条新的 device session 记录。
	CreateDeviceSession(ctx context.Context, userID, deviceName, accessTokenHash, refreshTokenHash string, expiresAt, refreshExpiresAt time.Time) (deviceSessionID string, err error)

	// GetDeviceSessionByAccessTokenHash 根据 access token 哈希查询 device session。
	// 不存在返回 sql.ErrNoRows。
	GetDeviceSessionByAccessTokenHash(ctx context.Context, accessTokenHash string) (*DeviceSessionRecord, error)

	// GetDeviceSessionByRefreshTokenHash 根据 refresh token 哈希查询 device session。
	// 不存在返回 sql.ErrNoRows。
	GetDeviceSessionByRefreshTokenHash(ctx context.Context, refreshTokenHash string) (*DeviceSessionRecord, error)

	// GetDeviceSessionByID 根据 device session ID 查询 device session。
	// 引入动机：revoke 端点需要根据 ID 查询 device session 以验证所有权。
	// 不存在返回 sql.ErrNoRows。
	GetDeviceSessionByID(ctx context.Context, deviceSessionID string) (*DeviceSessionRecord, error)

	// UpdateDeviceSessionTokens 更新 device session 的 access token 和 refresh token 哈希及过期时间。
	// 引入动机：refresh 流程需要轮换两个 token 的哈希值。
	UpdateDeviceSessionTokens(ctx context.Context, deviceSessionID, newAccessTokenHash, newRefreshTokenHash string, newExpiresAt, newRefreshExpiresAt time.Time) error

	// RevokeDeviceSession 根据 device session ID 将其标记为已撤销。
	RevokeDeviceSession(ctx context.Context, deviceSessionID string) error

	// CreateUser 创建新用户，返回用户 UUID。
	// 引入动机：Bootstrap 需要创建第一个 system_admin 用户。
	CreateUser(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error)

	// CountUsers 返回 users 表中的用户总数。
	// 引入动机：Bootstrap 需要判断 users 表是否为空。
	CountUsers(ctx context.Context) (int, error)

	// CreateUserIfNoneExist 原子地检查 users 表是否为空，若为空则插入用户并返回其 UUID。
	// 若 users 表非空，返回 ErrUsersAlreadyExist。
	// 引入动机：消除 Bootstrap 中 CountUsers + CreateUser 两步操作的 TOCTOU 竞态。
	// 实现必须保证在并发调用下，只有第一个调用成功插入，后续调用返回 ErrUsersAlreadyExist。
	CreateUserIfNoneExist(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error)
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

// GetUserByUsername 根据用户名查询用户。
func (r *PGRepository) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	const q = `SELECT id, username, email, password_hash, system_role, workspace_create_perm FROM users WHERE username = $1`

	var u User
	err := r.db.QueryRowContext(ctx, q, username).Scan(
		&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.SystemRole, &u.WorkspaceCreatePerm,
	)
	if err != nil {
		return nil, mapDBError(err, "根据用户名查询用户")
	}
	return &u, nil
}

// GetUserByID 根据 ID 查询用户。
func (r *PGRepository) GetUserByID(ctx context.Context, id string) (*User, error) {
	const q = `SELECT id, username, email, password_hash, system_role, workspace_create_perm FROM users WHERE id = $1`

	var u User
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.SystemRole, &u.WorkspaceCreatePerm,
	)
	if err != nil {
		return nil, mapDBError(err, "根据 ID 查询用户")
	}
	return &u, nil
}

// CreateSession 插入一条新的 session 记录。
func (r *PGRepository) CreateSession(ctx context.Context, userID, tokenHash, csrfTokenHash string, expiresAt time.Time) (string, error) {
	const q = `INSERT INTO sessions (user_id, token_hash, csrf_token_hash, expires_at) VALUES ($1, $2, $3, $4) RETURNING id`

	var sessionID string
	err := r.db.QueryRowContext(ctx, q, userID, tokenHash, csrfTokenHash, expiresAt).Scan(&sessionID)
	if err != nil {
		return "", mapDBError(err, "创建 session")
	}
	return sessionID, nil
}

// GetSessionByTokenHash 根据 token 哈希查询 session。
func (r *PGRepository) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*SessionRecord, error) {
	const q = `SELECT id, user_id, token_hash, csrf_token_hash, expires_at, revoked_at, created_at FROM sessions WHERE token_hash = $1`

	var s SessionRecord
	err := r.db.QueryRowContext(ctx, q, tokenHash).Scan(
		&s.ID, &s.UserID, &s.TokenHash, &s.CSRFTokenHash, &s.ExpiresAt, &s.RevokedAt, &s.CreatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "根据 token 哈希查询 session")
	}
	return &s, nil
}

// RevokeSession 根据 session ID 将 session 标记为已撤销。
func (r *PGRepository) RevokeSession(ctx context.Context, sessionID string) error {
	const q = `UPDATE sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`

	result, err := r.db.ExecContext(ctx, q, sessionID)
	if err != nil {
		return mapDBError(err, "撤销 session")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取撤销 session 影响行数: %w", err)
	}
	if rows == 0 {
		slog.Warn("撤销 session 未影响任何行——可能已撤销或不存在", "session_id", sessionID)
	}
	return nil
}

// CreateDeviceSession 插入一条新的 device session 记录。
func (r *PGRepository) CreateDeviceSession(ctx context.Context, userID, deviceName, accessTokenHash, refreshTokenHash string, expiresAt, refreshExpiresAt time.Time) (string, error) {
	const q = `INSERT INTO device_sessions (user_id, device_name, access_token_hash, refresh_token_hash, expires_at, refresh_expires_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`

	var deviceSessionID string
	err := r.db.QueryRowContext(ctx, q, userID, deviceName, accessTokenHash, refreshTokenHash, expiresAt, refreshExpiresAt).Scan(&deviceSessionID)
	if err != nil {
		return "", mapDBError(err, "创建 device session")
	}
	return deviceSessionID, nil
}

// GetDeviceSessionByAccessTokenHash 根据 access token 哈希查询 device session。
func (r *PGRepository) GetDeviceSessionByAccessTokenHash(ctx context.Context, accessTokenHash string) (*DeviceSessionRecord, error) {
	const q = `SELECT id, user_id, device_name, access_token_hash, refresh_token_hash, expires_at, refresh_expires_at, revoked_at, created_at FROM device_sessions WHERE access_token_hash = $1`

	var d DeviceSessionRecord
	err := r.db.QueryRowContext(ctx, q, accessTokenHash).Scan(
		&d.ID, &d.UserID, &d.DeviceName, &d.AccessTokenHash, &d.RefreshTokenHash,
		&d.ExpiresAt, &d.RefreshExpiresAt, &d.RevokedAt, &d.CreatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "根据 access token 哈希查询 device session")
	}
	return &d, nil
}

// GetDeviceSessionByRefreshTokenHash 根据 refresh token 哈希查询 device session。
func (r *PGRepository) GetDeviceSessionByRefreshTokenHash(ctx context.Context, refreshTokenHash string) (*DeviceSessionRecord, error) {
	const q = `SELECT id, user_id, device_name, access_token_hash, refresh_token_hash, expires_at, refresh_expires_at, revoked_at, created_at FROM device_sessions WHERE refresh_token_hash = $1`

	var d DeviceSessionRecord
	err := r.db.QueryRowContext(ctx, q, refreshTokenHash).Scan(
		&d.ID, &d.UserID, &d.DeviceName, &d.AccessTokenHash, &d.RefreshTokenHash,
		&d.ExpiresAt, &d.RefreshExpiresAt, &d.RevokedAt, &d.CreatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "根据 refresh token 哈希查询 device session")
	}
	return &d, nil
}

// GetDeviceSessionByID 根据 device session ID 查询 device session。
func (r *PGRepository) GetDeviceSessionByID(ctx context.Context, deviceSessionID string) (*DeviceSessionRecord, error) {
	const q = `SELECT id, user_id, device_name, access_token_hash, refresh_token_hash, expires_at, refresh_expires_at, revoked_at, created_at FROM device_sessions WHERE id = $1`

	var d DeviceSessionRecord
	err := r.db.QueryRowContext(ctx, q, deviceSessionID).Scan(
		&d.ID, &d.UserID, &d.DeviceName, &d.AccessTokenHash, &d.RefreshTokenHash,
		&d.ExpiresAt, &d.RefreshExpiresAt, &d.RevokedAt, &d.CreatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "根据 ID 查询 device session")
	}
	return &d, nil
}

// UpdateDeviceSessionTokens 更新 device session 的 token 哈希及过期时间。
func (r *PGRepository) UpdateDeviceSessionTokens(ctx context.Context, deviceSessionID, newAccessTokenHash, newRefreshTokenHash string, newExpiresAt, newRefreshExpiresAt time.Time) error {
	const q = `UPDATE device_sessions SET access_token_hash = $1, refresh_token_hash = $2, expires_at = $3, refresh_expires_at = $4 WHERE id = $5 AND revoked_at IS NULL`

	result, err := r.db.ExecContext(ctx, q, newAccessTokenHash, newRefreshTokenHash, newExpiresAt, newRefreshExpiresAt, deviceSessionID)
	if err != nil {
		return mapDBError(err, "更新 device session token")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取更新 device session token 影响行数: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("更新 device session token 未影响任何行——可能已撤销或不存在: device_session_id=%s", deviceSessionID)
	}
	return nil
}

// RevokeDeviceSession 根据 device session ID 将其标记为已撤销。
func (r *PGRepository) RevokeDeviceSession(ctx context.Context, deviceSessionID string) error {
	const q = `UPDATE device_sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`

	result, err := r.db.ExecContext(ctx, q, deviceSessionID)
	if err != nil {
		return mapDBError(err, "撤销 device session")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取撤销 device session 影响行数: %w", err)
	}
	if rows == 0 {
		slog.Warn("撤销 device session 未影响任何行——可能已撤销或不存在", "device_session_id", deviceSessionID)
	}
	return nil
}

// CreateUser 创建新用户，返回用户 UUID。
// 引入动机：Bootstrap 需要创建第一个 system_admin 用户，
// admin 用户管理需要创建/更新用户。密码必须已通过 HashPassword 哈希。
func (r *PGRepository) CreateUser(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	const q = `INSERT INTO users (username, email, password_hash, system_role, workspace_create_perm)
	           VALUES ($1, $2, $3, $4, $5) RETURNING id`

	var userID string
	err := r.db.QueryRowContext(ctx, q, username, email, passwordHash, systemRole, workspaceCreatePerm).Scan(&userID)
	if err != nil {
		return "", mapDBError(err, "创建用户")
	}
	return userID, nil
}

// CountUsers 返回 users 表中的用户总数。
// 引入动机：Bootstrap 需要判断 users 表是否为空以决定是否允许创建首个管理员。
func (r *PGRepository) CountUsers(ctx context.Context) (int, error) {
	const q = `SELECT COUNT(*) FROM users`
	var count int
	err := r.db.QueryRowContext(ctx, q).Scan(&count)
	if err != nil {
		return 0, mapDBError(err, "查询用户总数")
	}
	return count, nil
}

// CreateUserIfNoneExist 原子地检查 users 表是否为空，若为空则插入用户。
// 引入动机：消除 Bootstrap 中 CountUsers + CreateUser 两步操作的 TOCTOU 竞态。
//
// 原子性保证：
//   - 使用显式事务 + LOCK TABLE users IN ACCESS EXCLUSIVE MODE
//   - ACCESS EXCLUSIVE 锁阻止所有并发读写，确保同一时刻只有一个事务能操作 users 表
//   - 事务内先 COUNT 再 INSERT，两步之间不存在竞态窗口
//   - 并发调用方被 LOCK TABLE 阻塞，逐个进入事务，第一个成功后后续返回 ErrUsersAlreadyExist
func (r *PGRepository) CreateUserIfNoneExist(ctx context.Context, username, email, passwordHash, systemRole string, workspaceCreatePerm bool) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开始 Bootstrap 事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 锁定 users 表，阻止所有并发访问直到事务结束。
	// 这是消除 TOCTOU 竞态的核心：LOCK TABLE 序列化所有 CreateUserIfNoneExist 调用。
	if _, err := tx.ExecContext(ctx, `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		return "", fmt.Errorf("锁定 users 表: %w", err)
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return "", fmt.Errorf("查询用户总数: %w", err)
	}
	if count > 0 {
		return "", ErrUsersAlreadyExist
	}

	const insertQ = `INSERT INTO users (username, email, password_hash, system_role, workspace_create_perm)
	                 VALUES ($1, $2, $3, $4, $5) RETURNING id`
	var userID string
	if err := tx.QueryRowContext(ctx, insertQ, username, email, passwordHash, systemRole, workspaceCreatePerm).Scan(&userID); err != nil {
		return "", mapDBError(err, "创建首个用户")
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("提交 Bootstrap 事务: %w", err)
	}
	return userID, nil
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
