// authlogic.go 实现认证领域逻辑：login、logout、device authorize、refresh、revoke。
//
// 引入动机：design/04-WEB-API.md §Auth/Device 要求 login/session、device authorization、
// refresh/revoke 三个最低端点能力。HTTP handler 只负责输入输出，
// 认证流程逻辑（凭据校验、session 创建、token 生成与哈希、token 轮换、撤销）在此层实现。
//
// 安全原则：
//   - 认证失败不泄露用户是否存在（统一返回 ErrInvalidCredentials）
//   - 数据库只存 token 哈希，不存明文
//   - token 轮换时旧 token 哈希被覆盖，旧 token 立即失效
package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
)

// ErrInvalidCredentials 表示用户名或密码错误。
// 引入动机：login 失败时不区分"用户不存在"和"密码错误"，防止账户枚举攻击。
var ErrInvalidCredentials = errors.New("用户名或密码错误")

// ErrSessionExpired 表示 session 已过期。
var ErrSessionExpired = errors.New("session 已过期")

// ErrSessionRevoked 表示 session 已被撤销。
var ErrSessionRevoked = errors.New("session 已被撤销")

// ErrDeviceSessionExpired 表示 device session 的 access token 已过期。
var ErrDeviceSessionExpired = errors.New("device access token 已过期")

// ErrDeviceSessionRevoked 表示 device session 已被撤销。
var ErrDeviceSessionRevoked = errors.New("device session 已被撤销")

// ErrRefreshTokenExpired 表示 refresh token 已过期。
var ErrRefreshTokenExpired = errors.New("refresh token 已过期")

// 账户设置错误用于在 handler 层映射为稳定的 HTTP 响应，避免泄露内部细节。
var (
	ErrCurrentPasswordInvalid = errors.New("当前密码错误")
	ErrEmailInvalid           = errors.New("邮箱格式不正确")
	ErrEmailAlreadyExists     = errors.New("邮箱已存在")
	ErrNewPasswordTooShort    = errors.New("新密码长度不足")
)

// LoginResult 是 login 成功后返回的结果。
// 引入动机：handler 需要设置 cookie 和响应体，这些数据由领域逻辑层生成。
type LoginResult struct {
	// SessionToken 是明文 session token，仅返回给 handler 用于设置 cookie。
	// 数据库中只存储其哈希。
	SessionToken string

	// CSRFToken 是明文 CSRF token，仅返回给 handler 用于设置 cookie 和响应体。
	// 数据库中只存储其哈希。
	CSRFToken string

	// SessionID 是 session 记录的 UUID。
	SessionID string

	// UserID 是用户 UUID。
	UserID string

	// Username 是用户名。
	Username string

	// SystemRole 是用户系统角色。
	SystemRole string

	// ExpiresAt 是 session 过期时间。
	ExpiresAt time.Time
}

// Login 执行用户凭据认证并创建 Web session。
//
// 参数：
//   - ctx：请求 context
//   - repo：数据访问接口
//   - cfg：认证配置
//   - username：用户名
//   - password：明文密码
//
// 成功返回 LoginResult，失败返回 ErrInvalidCredentials（不泄露用户是否存在）。
// 其他内部错误（如 DB 故障）原样返回。
func Login(ctx context.Context, repo Repository, cfg AuthConfig, username, password string) (*LoginResult, error) {
	user, err := repo.GetUserByUsername(ctx, username)
	if err != nil {
		if err == sql.ErrNoRows {
			// 用户不存在——与密码错误返回相同错误，防止账户枚举
			slog.Info("登录失败：用户不存在", "username", username)
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("查询用户: %w", err)
	}

	if err := VerifyPassword(user.PasswordHash, password); err != nil {
		if err == ErrPasswordVerificationFailed {
			slog.Info("登录失败：密码不匹配", "username", username)
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("校验密码: %w", err)
	}

	// 生成 session token 和 CSRF token
	sessionToken, err := GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("生成 session token: %w", err)
	}

	csrfToken, err := GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("生成 CSRF token: %w", err)
	}

	// 只将哈希写入数据库
	sessionTokenHash := HashToken(sessionToken)
	csrfTokenHash := HashToken(csrfToken)

	expiresAt := time.Now().Add(time.Duration(cfg.SessionDuration) * time.Second)

	sessionID, err := repo.CreateSession(ctx, user.ID, sessionTokenHash, csrfTokenHash, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("创建 session: %w", err)
	}

	slog.Info("登录成功", "user_id", user.ID, "username", user.Username, "session_id", sessionID)

	return &LoginResult{
		SessionToken: sessionToken,
		CSRFToken:    csrfToken,
		SessionID:    sessionID,
		UserID:       user.ID,
		Username:     user.Username,
		SystemRole:   user.SystemRole,
		ExpiresAt:    expiresAt,
	}, nil
}

// Logout 撤销当前 Web session。
//
// 参数：
//   - ctx：请求 context
//   - repo：数据访问接口
//   - sessionToken：明文 session token（从 cookie 中提取）
//
// 如果 session 不存在或已撤销，不返回错误（幂等操作）。
func Logout(ctx context.Context, repo Repository, sessionToken string) error {
	tokenHash := HashToken(sessionToken)

	session, err := repo.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if err == sql.ErrNoRows {
			// session 不存在——幂等处理，不报错
			slog.Info("logout: session 不存在，幂等处理")
			return nil
		}
		return fmt.Errorf("查询 session: %w", err)
	}

	if err := repo.RevokeSession(ctx, session.ID); err != nil {
		return fmt.Errorf("撤销 session: %w", err)
	}

	slog.Info("logout 成功", "session_id", session.ID)
	return nil
}

// DeviceAuthorizeResult 是 device authorization 成功后返回的结果。
type DeviceAuthorizeResult struct {
	// AccessToken 是明文 access token，仅返回给 handler 用于响应体。
	// 数据库中只存储其哈希。
	AccessToken string

	// RefreshToken 是明文 refresh token，仅返回给 handler 用于响应体。
	// 数据库中只存储其哈希。
	RefreshToken string

	// DeviceSessionID 是 device session 记录的 UUID。
	DeviceSessionID string

	// ExpiresAt 是 access token 的过期时间。
	ExpiresAt time.Time

	// RefreshExpiresAt 是 refresh token 的过期时间。
	RefreshExpiresAt time.Time
}

// AuthorizeDevice 为已认证用户创建 device session，返回 access token 和 refresh token。
//
// 参数：
//   - ctx：请求 context
//   - repo：数据访问接口
//   - cfg：认证配置
//   - userID：已认证用户的 UUID
//   - deviceName：设备名称（由客户端提供，用于标识设备）
//
// 成功返回 DeviceAuthorizeResult，失败返回错误。
func AuthorizeDevice(ctx context.Context, repo Repository, cfg AuthConfig, userID, deviceName string) (*DeviceAuthorizeResult, error) {
	// 空 device_name 安全 fallback：Web device-sessions 不再要求输入设备名。
	// 服务端生成随机非敏感 fallback，不包含用户名/路径/IP/token 等敏感信息。
	deviceName, err := ensureDeviceName(deviceName, "device-")
	if err != nil {
		return nil, err
	}

	accessToken, err := GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("生成 access token: %w", err)
	}

	refreshToken, err := GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("生成 refresh token: %w", err)
	}

	accessTokenHash := HashToken(accessToken)
	refreshTokenHash := HashToken(refreshToken)

	expiresAt := time.Now().Add(time.Duration(cfg.DeviceAccessTokenDuration) * time.Second)
	refreshExpiresAt := time.Now().Add(time.Duration(cfg.DeviceRefreshTokenDuration) * time.Second)

	deviceSessionID, err := repo.CreateDeviceSession(ctx, userID, deviceName, accessTokenHash, refreshTokenHash, expiresAt, refreshExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("创建 device session: %w", err)
	}

	slog.Info("device authorization 成功", "user_id", userID, "device_session_id", deviceSessionID, "device_name", deviceName)

	return &DeviceAuthorizeResult{
		AccessToken:      accessToken,
		RefreshToken:     refreshToken,
		DeviceSessionID:  deviceSessionID,
		ExpiresAt:        expiresAt,
		RefreshExpiresAt: refreshExpiresAt,
	}, nil
}

// RefreshResult 是 token 刷新成功后返回的结果。
type RefreshResult struct {
	// AccessToken 是新的明文 access token。
	AccessToken string

	// RefreshToken 是新的明文 refresh token（轮换后旧 token 失效）。
	RefreshToken string

	// ExpiresAt 是新 access token 的过期时间。
	ExpiresAt time.Time

	// RefreshExpiresAt 是新 refresh token 的过期时间。
	RefreshExpiresAt time.Time
}

// RefreshToken 使用 refresh token 刷新 device session，轮换 access token 和 refresh token。
//
// 参数：
//   - ctx：请求 context
//   - repo：数据访问接口
//   - cfg：认证配置
//   - refreshToken：明文 refresh token
//
// 安全行为：
//   - refresh token 哈希在数据库中被覆盖（轮换），旧 refresh token 立即失效
//   - access token 哈希也被覆盖，旧 access token 立即失效
//   - 如果 device session 已撤销或 refresh token 已过期，拒绝刷新
func RefreshToken(ctx context.Context, repo Repository, cfg AuthConfig, refreshToken string) (*RefreshResult, error) {
	refreshTokenHash := HashToken(refreshToken)

	deviceSession, err := repo.GetDeviceSessionByRefreshTokenHash(ctx, refreshTokenHash)
	if err != nil {
		if err == sql.ErrNoRows {
			slog.Info("refresh token 刷新失败：device session 不存在")
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("查询 device session: %w", err)
	}

	if deviceSession.RevokedAt.Valid {
		slog.Info("refresh token 刷新失败：device session 已撤销", "device_session_id", deviceSession.ID)
		return nil, ErrDeviceSessionRevoked
	}

	if time.Now().After(deviceSession.RefreshExpiresAt) {
		slog.Info("refresh token 刷新失败：refresh token 已过期", "device_session_id", deviceSession.ID)
		return nil, ErrRefreshTokenExpired
	}

	// 生成新的 access token 和 refresh token
	newAccessToken, err := GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("生成新 access token: %w", err)
	}

	newRefreshToken, err := GenerateToken()
	if err != nil {
		return nil, fmt.Errorf("生成新 refresh token: %w", err)
	}

	newAccessTokenHash := HashToken(newAccessToken)
	newRefreshTokenHash := HashToken(newRefreshToken)

	newExpiresAt := time.Now().Add(time.Duration(cfg.DeviceAccessTokenDuration) * time.Second)
	newRefreshExpiresAt := time.Now().Add(time.Duration(cfg.DeviceRefreshTokenDuration) * time.Second)

	if err := repo.UpdateDeviceSessionTokens(ctx, deviceSession.ID, newAccessTokenHash, newRefreshTokenHash, newExpiresAt, newRefreshExpiresAt); err != nil {
		return nil, fmt.Errorf("更新 device session token: %w", err)
	}

	slog.Info("token 刷新成功", "device_session_id", deviceSession.ID)

	return &RefreshResult{
		AccessToken:      newAccessToken,
		RefreshToken:     newRefreshToken,
		ExpiresAt:        newExpiresAt,
		RefreshExpiresAt: newRefreshExpiresAt,
	}, nil
}

// RevokeDeviceSession 撤销指定的 device session。
//
// 参数：
//   - ctx：请求 context
//   - repo：数据访问接口
//   - deviceSessionID：要撤销的 device session UUID
//
// 幂等操作：如果 device session 不存在或已撤销，不返回错误。
func RevokeDeviceSession(ctx context.Context, repo Repository, deviceSessionID string) error {
	if err := repo.RevokeDeviceSession(ctx, deviceSessionID); err != nil {
		return fmt.Errorf("撤销 device session: %w", err)
	}

	slog.Info("device session 撤销成功", "device_session_id", deviceSessionID)
	return nil
}

const minAccountPasswordLength = 12

// GetCurrentUser 返回当前认证用户的非敏感资料。
// 引入动机：账户页需要从服务端读取权威资料，不能依赖客户端可修改的缓存状态。
func GetCurrentUser(ctx context.Context, repo Repository, userID string) (*User, error) {
	user, err := repo.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("查询当前用户: %w", err)
	}
	return user, nil
}

// UpdateEmail 验证当前密码后更新当前用户邮箱。
// 引入动机：邮箱属于账户资料，必须由后端绑定当前身份并要求当前密码确认。
func UpdateEmail(ctx context.Context, repo Repository, cfg AuthConfig, userID, currentPassword, email string) (*User, error) {
	user, err := repo.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("查询当前用户: %w", err)
	}
	if err := VerifyPassword(user.PasswordHash, currentPassword); err != nil {
		if errors.Is(err, ErrPasswordVerificationFailed) {
			return nil, ErrCurrentPasswordInvalid
		}
		return nil, fmt.Errorf("校验当前密码: %w", err)
	}

	email = strings.TrimSpace(email)
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if err := repo.UpdateUserEmail(ctx, userID, email); err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrEmailAlreadyExists
		}
		return nil, fmt.Errorf("更新用户邮箱: %w", err)
	}
	user.Email = email
	return user, nil
}

// UpdatePassword 验证当前密码并写入新的 Argon2id 密码哈希。
// 引入动机：改密必须由服务端完成哈希和持久化，明文只在当前请求内存在。
func UpdatePassword(ctx context.Context, repo Repository, cfg AuthConfig, userID, currentPassword, newPassword string) error {
	user, err := repo.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("查询当前用户: %w", err)
	}
	if err := VerifyPassword(user.PasswordHash, currentPassword); err != nil {
		if errors.Is(err, ErrPasswordVerificationFailed) {
			return ErrCurrentPasswordInvalid
		}
		return fmt.Errorf("校验当前密码: %w", err)
	}
	if len([]rune(newPassword)) < minAccountPasswordLength {
		return ErrNewPasswordTooShort
	}

	passwordHash, err := HashPassword(newPassword, cfg)
	if err != nil {
		return fmt.Errorf("生成新密码哈希: %w", err)
	}
	if err := repo.UpdateUserPasswordHash(ctx, userID, passwordHash); err != nil {
		return fmt.Errorf("更新用户密码哈希: %w", err)
	}
	return nil
}

func validateEmail(email string) error {
	if email == "" || len([]rune(email)) > 255 {
		return ErrEmailInvalid
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || !strings.Contains(email, "@") {
		return ErrEmailInvalid
	}
	return nil
}

// ValidateSession 验证 session token 并返回 Identity。
// 引入动机：middleware 需要验证 cookie session 并构建 Identity 放入 request context。
//
// 验证步骤：
//  1. 根据 token 哈希查询 session
//  2. 检查 session 是否已撤销
//  3. 检查 session 是否已过期
//  4. 查询关联用户信息
func ValidateSession(ctx context.Context, repo Repository, sessionToken string) (*Identity, error) {
	tokenHash := HashToken(sessionToken)

	session, err := repo.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("查询 session: %w", err)
	}

	if session.RevokedAt.Valid {
		return nil, ErrSessionRevoked
	}

	if time.Now().After(session.ExpiresAt) {
		return nil, ErrSessionExpired
	}

	user, err := repo.GetUserByID(ctx, session.UserID)
	if err != nil {
		return nil, fmt.Errorf("查询 session 关联用户: %w", err)
	}

	return &Identity{
		UserID:              user.ID,
		Username:            user.Username,
		SystemRole:          user.SystemRole,
		WorkspaceCreatePerm: user.WorkspaceCreatePerm,
		SessionID:           session.ID,
		AuthMethod:          AuthMethodCookie,
	}, nil
}

// ValidateAccessToken 验证 bearer access token 并返回 Identity。
// 引入动机：middleware 需要验证 bearer token 并构建 Identity 放入 request context。
//
// 验证步骤：
//  1. 根据 access token 哈希查询 device session
//  2. 检查 device session 是否已撤销
//  3. 检查 access token 是否已过期
//  4. 查询关联用户信息
func ValidateAccessToken(ctx context.Context, repo Repository, accessToken string) (*Identity, error) {
	tokenHash := HashToken(accessToken)

	deviceSession, err := repo.GetDeviceSessionByAccessTokenHash(ctx, tokenHash)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("查询 device session: %w", err)
	}

	if deviceSession.RevokedAt.Valid {
		return nil, ErrDeviceSessionRevoked
	}

	if time.Now().After(deviceSession.ExpiresAt) {
		return nil, ErrDeviceSessionExpired
	}

	user, err := repo.GetUserByID(ctx, deviceSession.UserID)
	if err != nil {
		return nil, fmt.Errorf("查询 device session 关联用户: %w", err)
	}

	return &Identity{
		UserID:              user.ID,
		Username:            user.Username,
		SystemRole:          user.SystemRole,
		WorkspaceCreatePerm: user.WorkspaceCreatePerm,
		DeviceSessionID:     deviceSession.ID,
		AuthMethod:          AuthMethodBearer,
	}, nil
}

// ValidateCSRF 验证请求中的 CSRF token 是否与 session 关联的 CSRF token 匹配。
// 引入动机：design/04-WEB-API.md §Security 要求 CSRF protection。
//
// 验证逻辑：
//  1. 从 session token 查询 session 记录
//  2. 将请求中的 CSRF token 哈希后与数据库中存储的 csrf_token_hash 比较
//  3. 使用 subtle.ConstantTimeCompare 防止时序攻击
//
// 参数：
//   - ctx：请求 context
//   - repo：数据访问接口
//   - sessionToken：从 cookie 中提取的明文 session token
//   - csrfToken：从请求头中提取的明文 CSRF token
func ValidateCSRF(ctx context.Context, repo Repository, sessionToken, csrfToken string) error {
	tokenHash := HashToken(sessionToken)

	session, err := repo.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		if err == sql.ErrNoRows {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("查询 session for CSRF: %w", err)
	}

	providedCSRFHash := HashToken(csrfToken)
	if subtle.ConstantTimeCompare([]byte(session.CSRFTokenHash), []byte(providedCSRFHash)) != 1 {
		return ErrInvalidCredentials
	}

	return nil
}
