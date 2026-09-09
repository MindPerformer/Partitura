// Package auth 实现认证与授权基础：用户凭据认证、Web session、CSRF 验证、
// device session/token 的授权、刷新与撤销，以及身份认证 middleware。
//
// 引入动机：Phase 1 WP-2 需要一个可运行的认证基础，供后续工作包的 API middleware 复用。
// 设计依据：design/04-WEB-API.md §Security（Argon2id、secure cookies、CSRF protection、
// token hashes only、ACL 后端强制）、§Auth/Device（login/session、device authorization、
// refresh/revoke）。
package auth

// AuthConfig 是认证模块的运行时配置。
// 引入动机：session/device token 的有效期、cookie 属性、Argon2id 参数需要可配置，
// 供 config.Load 填充后传入 auth 模块。
type AuthConfig struct {
	// SessionDuration 是 Web cookie session 的有效期。
	// 过期后 session 不可用，用户需重新登录。
	SessionDuration int64 // 秒

	// DeviceAccessTokenDuration 是 device session access token 的有效期。
	// 通常较短（如 15 分钟），过期后需用 refresh token 刷新。
	DeviceAccessTokenDuration int64 // 秒

	// DeviceRefreshTokenDuration 是 device session refresh token 的有效期。
	// 通常较长（如 30 天），过期后需重新执行 device authorization。
	DeviceRefreshTokenDuration int64 // 秒

	// CookieSecure 控制 session cookie 是否标记 Secure。
	// 生产环境必须为 true；仅开发环境可设 false 以允许 HTTP 测试。
	CookieSecure bool

	// CookieDomain 是 cookie 的 Domain 属性。
	// 空字符串表示不设置 Domain，cookie 仅对当前域生效。
	CookieDomain string

	// CookiePath 是 cookie 的 Path 属性，默认 "/"。
	CookiePath string

	// CookieName 是 session token cookie 的名称。
	CookieName string

	// CSRFCookieName 是 CSRF token cookie 的名称。
	// CSRF token 以非 HttpOnly cookie 提供给浏览器，前端 JS 可读取并以 header 回传。
	CSRFCookieName string

	// CSRFHeaderName 是 CSRF token 在请求头中的名称。
	CSRFHeaderName string

	// Argon2Memory 是 Argon2id 的内存参数，单位 KiB。
	Argon2Memory uint32

	// Argon2Iterations 是 Argon2id 的迭代次数。
	Argon2Iterations uint32

	// Argon2Parallelism 是 Argon2id 的并行度。
	Argon2Parallelism uint8

	// Argon2SaltLength 是 Argon2id 的盐长度，单位字节。
	Argon2SaltLength uint32

	// Argon2KeyLength 是 Argon2id 的输出密钥长度，单位字节。
	Argon2KeyLength uint32
}

// DefaultAuthConfig 返回生产环境安全的默认认证配置。
// 引入动机：config.Load 在未显式设置 auth 配置项时使用此默认值，
// 确保安全参数不会因遗漏而降级。
func DefaultAuthConfig() AuthConfig {
	return AuthConfig{
		SessionDuration:           86400,     // 24 小时
		DeviceAccessTokenDuration: 900,       // 15 分钟
		DeviceRefreshTokenDuration: 2592000,  // 30 天
		CookieSecure:              true,
		CookieDomain:              "",
		CookiePath:                "/",
		CookieName:                "session",
		CSRFCookieName:            "csrf",
		CSRFHeaderName:            "X-CSRF-Token",
		Argon2Memory:              64 * 1024, // 64 MiB
		Argon2Iterations:          3,
		Argon2Parallelism:         2,
		Argon2SaltLength:          16,
		Argon2KeyLength:           32,
	}
}

// Identity 是已认证身份信息，由 middleware 解析后放入 request context。
// 引入动机：后续 handler/middleware 需要从 request context 获取当前用户身份，
// 以执行 RBAC 检查和业务逻辑。
type Identity struct {
	// UserID 是已认证用户的 UUID 字符串。
	UserID string

	// Username 是已认证用户的用户名。
	Username string

	// SystemRole 是用户的系统角色：system_admin 或 user。
	SystemRole string

	// WorkspaceCreatePerm 表示用户是否拥有 workspace:create 独立权限。
	// 引入动机：design/04-WEB-API.md §RBAC 规定 workspace:create 是独立布尔权限，
	// workspace 创建 handler 需要检查此字段。
	WorkspaceCreatePerm bool

	// SessionID 是 Web cookie session 的 UUID。
	// 仅在通过 cookie session 认证时非空。
	SessionID string

	// DeviceSessionID 是 device session 的 UUID。
	// 仅在通过 bearer access token 认证时非空。
	DeviceSessionID string

	// AuthMethod 标识认证方式：cookie 或 bearer。
	AuthMethod string
}

// AuthMethod 常量定义认证方式标识。
const (
	AuthMethodCookie = "cookie"
	AuthMethodBearer = "bearer"
)

// contextKey 是 request context 中存储 Identity 的键类型。
// 引入动机：使用自定义类型避免 context key 冲突。
type contextKey struct{}

// ctxKey 是包级 context key 实例。
var ctxKey = contextKey{}
