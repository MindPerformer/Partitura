// deviceauth.go 实现 device authorization flow 的领域逻辑和 HTTP handler。
//
// 引入动机：design/02-MCP.md §登录 要求 device/browser authorization 风格登录。
// 现有 /api/auth/device/authorize 需要已认证的 cookie session + CSRF，
// 不适合 CLI 工具直接使用。新增 POST /api/auth/device/start 和 POST /api/auth/device/poll
// 支持 OAuth2 Device Authorization Grant 风格的流程。
//
// 流程：
//  1. CLI 调用 POST /api/auth/device/start，传入 device_name
//  2. Server 创建 device_authorization 记录，返回 device_code、user_code、verification_url
//  3. CLI 显示 user_code 和 verification_url，用户在浏览器中登录并输入 user_code
//  4. 用户通过 Web UI 批准授权（POST /api/auth/device/approve，需要 cookie session + CSRF）
//  5. CLI 轮询 POST /api/auth/device/poll，传入 device_code
//  6. 用户批准后返回 access_token + refresh_token
//
// 安全：
//   - device_code 和 user_code 使用高熵随机值
//   - device_code 有效期默认 15 分钟
//   - 授权批准后创建 device_session，与现有 device session 机制一致
//   - token 只存哈希
//   - 审计日志记录
package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"partitura/server/internal/audit"
	"partitura/server/internal/requestid"
)

// DeviceAuthConfig 是 device authorization 流程的配置。
// 引入动机：需要配置 device code 有效期和 poll 间隔。
type DeviceAuthConfig struct {
	// DeviceCodeExpiresIn 是 device code 的有效期（秒）。
	DeviceCodeExpiresIn int64

	// PollInterval 是客户端轮询间隔（秒）。
	PollInterval int64

	// UserCodeLength 是 user code 的字符数。
	UserCodeLength int

	// PublicOrigin 是可信的公开服务 origin，用于生成 verification_url。
	// 引入动机：禁止从请求 Host 推导 URL，避免 Host header 注入。
	// 缺失或非法时 device start fail closed。
	PublicOrigin string
}

// DefaultDeviceAuthConfig 返回默认的 device authorization 配置。
func DefaultDeviceAuthConfig() DeviceAuthConfig {
	return DeviceAuthConfig{
		DeviceCodeExpiresIn: 900,  // 15 分钟
		PollInterval:        5,    // 5 秒
		UserCodeLength:      8,    // 8 字符
	}
}

// DeviceAuthRepository 定义 device authorization 流程所需的数据访问接口。
// 引入动机：分离领域逻辑与数据访问，便于测试注入 mock。
type DeviceAuthRepository interface {
	// CreateDeviceAuthorization 创建 device authorization 记录。
	CreateDeviceAuthorization(ctx context.Context, deviceCode, userCode, deviceName string, expiresAt time.Time, pollInterval int) (id string, err error)

	// GetDeviceAuthorizationByDeviceCode 根据 device code 查询记录。
	GetDeviceAuthorizationByDeviceCode(ctx context.Context, deviceCode string) (*DeviceAuthorizationRecord, error)

	// GetDeviceAuthorizationByUserCode 根据 user code 查询 pending 记录。
	GetDeviceAuthorizationByUserCode(ctx context.Context, userCode string) (*DeviceAuthorizationRecord, error)

	// GetDeviceAuthorizationByUserCodeAny 根据 user code 查询任意状态记录。
	// 引入动机：device-authorize 页面需要向已登录用户展示安全元数据（状态、设备名、过期时间），
	// 不返回 device_code/token/user 身份。
	GetDeviceAuthorizationByUserCodeAny(ctx context.Context, userCode string) (*DeviceAuthorizationRecord, error)

	// ApproveDeviceAuthorization 设置 user_id 和 status=authorized。
	ApproveDeviceAuthorization(ctx context.Context, id, userID string) error

	// DenyDeviceAuthorization 设置 status=denied。
	DenyDeviceAuthorization(ctx context.Context, id string) error

	// ExpireDeviceAuthorizations 将过期的 pending 记录标记为 expired。
	ExpireDeviceAuthorizations(ctx context.Context) error

	// ExchangeDeviceAuthorization 原子地将 status=authorized 的授权转为 completed，
	// 并在同一事务中创建 hash-only device session。
	// 引入动机：Phase6 要求 token 一次性交换——任何并发或重复 poll
	// 不可再创建 session 或颁发 token。
	// 返回 (userID, deviceName, err)：非 authorized 状态时返回对应的 ErrDeviceAuth* 哨兵错误。
	ExchangeDeviceAuthorization(ctx context.Context, deviceCode, accessTokenHash, refreshTokenHash string, expiresAt, refreshExpiresAt time.Time) (userID, deviceName string, err error)
}

// device authorization 终端状态哨兵错误。
// 引入动机：ExchangeDeviceAuthorization 需要把"非 authorized"的各终端状态
// 以可判定的错误形式返回给 DeviceAuthPoll 映射为 poll 状态。
var (
	// ErrDeviceAuthPending 表示授权仍处于 pending。
	ErrDeviceAuthPending = errors.New("device authorization 仍在等待批准")
	// ErrDeviceAuthDenied 表示授权已被用户拒绝。
	ErrDeviceAuthDenied = errors.New("device authorization 已被拒绝")
	// ErrDeviceAuthExpired 表示授权已过期。
	ErrDeviceAuthExpired = errors.New("device authorization 已过期")
	// ErrDeviceAuthCompleted 表示授权已完成一次性交换，不可再颁发 token。
	ErrDeviceAuthCompleted = errors.New("device authorization 已完成交换")
	// ErrDeviceAuthNoUser 表示 authorized 状态但缺少 user_id 的数据异常。
	ErrDeviceAuthNoUser = errors.New("authorized 状态但 user_id 为空")
)

// DeviceAuthorizationRecord 是从数据库读取的 device authorization 记录。
type DeviceAuthorizationRecord struct {
	ID                 string
	DeviceCode         string
	UserCode           string
	DeviceName         string
	UserID             sql.NullString
	Status             string
	ExpiresAt          time.Time
	PollIntervalSeconds int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// generateDeviceCode 生成高熵随机 device code。
// 引入动机：device code 需要足够的熵以防止猜测攻击。
func generateDeviceCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 device code: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// generateUserCode 生成用户可读的 user code。
// 引入动机：user code 需要用户在浏览器中手动输入，使用大写字母和数字，排除易混淆字符。
func generateUserCode(length int) (string, error) {
	// 排除 0/O/1/I/L 等易混淆字符
	const charset = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, length)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("生成 user code: %w", err)
		}
		b[i] = charset[n.Int64()]
	}
	return string(b), nil
}

// ensureDeviceName 在 device name 为空时生成安全 fallback。
// 引入动机：MCP 与 Web device-sessions 均不要求用户输入 device_name；
// 服务端必须保证空值不会导致匿名/空白设备名，且不包含敏感信息。
// prefix 用于区分不同入口产生的 fallback（如 mcp-device- 或 device-）。
func ensureDeviceName(deviceName, prefix string) (string, error) {
	if strings.TrimSpace(deviceName) != "" {
		return strings.TrimSpace(deviceName), nil
	}
	suffix, err := generateUserCode(6)
	if err != nil {
		return "", fmt.Errorf("生成 fallback device name: %w", err)
	}
	return prefix + suffix, nil
}

// DeviceAuthStartResult 是 device authorization start 的结果。
type DeviceAuthStartResult struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	ExpiresIn       int64
	Interval        int64
}

// DeviceAuthStart 创建 device authorization 记录。
// 引入动机：CLI login 命令调用此逻辑启动授权流程。
//
// 参数：
//   - ctx：请求 context
//   - repo：device auth 数据访问接口
//   - cfg：device auth 配置
//   - deviceName：设备名称
//   - verificationURL：用户授权页面 URL 前缀
func DeviceAuthStart(ctx context.Context, repo DeviceAuthRepository, cfg DeviceAuthConfig, deviceName, verificationURLPrefix string) (*DeviceAuthStartResult, error) {
	// 空 device_name 安全 fallback：MCP 与 Web device-sessions 不要求输入设备名。
	// 若客户端未提供名称，server 生成随机非敏感 fallback，不包含敏感信息。
	deviceName, err := ensureDeviceName(deviceName, "mcp-device-")
	if err != nil {
		return nil, err
	}

	deviceCode, err := generateDeviceCode()
	if err != nil {
		return nil, fmt.Errorf("生成 device code: %w", err)
	}

	userCode, err := generateUserCode(cfg.UserCodeLength)
	if err != nil {
		return nil, fmt.Errorf("生成 user code: %w", err)
	}

	expiresAt := time.Now().Add(time.Duration(cfg.DeviceCodeExpiresIn) * time.Second)

	if _, err := repo.CreateDeviceAuthorization(ctx, deviceCode, userCode, deviceName, expiresAt, int(cfg.PollInterval)); err != nil {
		return nil, fmt.Errorf("创建 device authorization: %w", err)
	}

	// 审计/日志不得记录 user_code（代码是授权凭证的一部分，泄露后可被他人批准）。
	slog.Info("device authorization 创建", "device_name", deviceName)

	return &DeviceAuthStartResult{
		DeviceCode:      deviceCode,
		UserCode:        userCode,
		VerificationURL: verificationURLPrefix + "?code=" + userCode,
		ExpiresIn:       cfg.DeviceCodeExpiresIn,
		Interval:        cfg.PollInterval,
	}, nil
}

// DeviceAuthPollResult 是 device authorization poll 的结果。
type DeviceAuthPollResult struct {
	Status        string
	AccessToken   string
	RefreshToken  string
	TokenType     string
	ExpiresIn     int64
}

// DeviceAuthPoll 轮询 device authorization 状态。
// 引入动机：CLI login 命令轮询授权状态。
//
// 返回：
//   - pending：用户尚未批准
//   - authorized：首次轮询已完成一次性交换，包含 access_token 和 refresh_token
//   - denied：用户拒绝
//   - expired：device code 已过期
//   - completed：授权已一次性交换完成，任何后续/并发 poll 不再颁发 token
//
// 安全：token 交换通过 ExchangeDeviceAuthorization 在单事务内完成，
// 保证重复或并发 poll 不能重复创建 device session。
func DeviceAuthPoll(ctx context.Context, daRepo DeviceAuthRepository, authCfg AuthConfig, deviceCode string) (*DeviceAuthPollResult, error) {
	// 预生成 token（仅在 exchange 成功时使用；交换失败时安全丢弃，不落盘）。
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

	expiresAt := time.Now().Add(time.Duration(authCfg.DeviceAccessTokenDuration) * time.Second)
	refreshExpiresAt := time.Now().Add(time.Duration(authCfg.DeviceRefreshTokenDuration) * time.Second)

	userID, deviceName, err := daRepo.ExchangeDeviceAuthorization(ctx, deviceCode, accessTokenHash, refreshTokenHash, expiresAt, refreshExpiresAt)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("device code 不存在")
		case errors.Is(err, ErrDeviceAuthPending):
			return &DeviceAuthPollResult{Status: "pending"}, nil
		case errors.Is(err, ErrDeviceAuthDenied):
			return &DeviceAuthPollResult{Status: "denied"}, nil
		case errors.Is(err, ErrDeviceAuthExpired):
			return &DeviceAuthPollResult{Status: "expired"}, nil
		case errors.Is(err, ErrDeviceAuthCompleted):
			// 已完成一次性交换——不再颁发 token。
			return &DeviceAuthPollResult{Status: "completed"}, nil
		case errors.Is(err, ErrDeviceAuthNoUser):
			return nil, err
		default:
			return nil, fmt.Errorf("交换 device authorization: %w", err)
		}
	}

	// 日志不包含 code/token。
	slog.Info("device authorization 一次性交换完成", "user_id", userID, "device_name", deviceName)

	return &DeviceAuthPollResult{
		Status:       "authorized",
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    authCfg.DeviceAccessTokenDuration,
	}, nil
}

// DeviceAuthApprove 用户批准 device authorization。
// 引入动机：用户通过 Web UI 批准授权请求。
//
// 参数：
//   - ctx：请求 context
//   - repo：device auth 数据访问接口
//   - userCode：用户输入的 user code
//   - userID：已认证用户的 UUID
func DeviceAuthApprove(ctx context.Context, repo DeviceAuthRepository, userCode, userID string) error {
	record, err := repo.GetDeviceAuthorizationByUserCode(ctx, userCode)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("user code 不存在或已使用")
		}
		return fmt.Errorf("查询 device authorization: %w", err)
	}

	if record.Status != "pending" {
		return fmt.Errorf("device authorization 状态不是 pending: %s", record.Status)
	}

	if time.Now().After(record.ExpiresAt) {
		_ = repo.ExpireDeviceAuthorizations(ctx)
		return fmt.Errorf("device authorization 已过期")
	}

	if err := repo.ApproveDeviceAuthorization(ctx, record.ID, userID); err != nil {
		return fmt.Errorf("批准 device authorization: %w", err)
	}

	// 审计/日志不记录 user_code（一次性的用户授权凭证），仅记录操作者与状态。
	slog.Info("device authorization 已批准", "user_id", userID)
	return nil
}

// --- HTTP Handler ---

// DeviceAuthHandler 是 device authorization 的 HTTP handler。
// 引入动机：将 device authorization 的 HTTP 端点集中处理。
type DeviceAuthHandler struct {
	daRepo   DeviceAuthRepository
	authRepo Repository
	authCfg  AuthConfig
	daCfg    DeviceAuthConfig
	auditRepo audit.Repository
}

// NewDeviceAuthHandler 创建 device auth handler。
// auditRepo 用于 approve/deny/completed 的审计记录，缺失时仅记录 slog，不写入 audit_logs。
func NewDeviceAuthHandler(daRepo DeviceAuthRepository, authRepo Repository, authCfg AuthConfig, daCfg DeviceAuthConfig, auditRepo audit.Repository) *DeviceAuthHandler {
	return &DeviceAuthHandler{
		daRepo:    daRepo,
		authRepo:  authRepo,
		authCfg:   authCfg,
		daCfg:     daCfg,
		auditRepo: auditRepo,
	}
}

// verificationURLPrefix 生成 device authorize 页面的可信 URL 前缀。
// 引入动机：verification_url 只能来自明确配置的 PUBLIC_ORIGIN，禁止 Host header 注入。
// 生产缺失或非法时返回错误，调用方 fail closed。
func (h *DeviceAuthHandler) verificationURLPrefix() (string, error) {
	raw := strings.TrimSpace(h.daCfg.PublicOrigin)
	if raw == "" {
		return "", errors.New("PUBLIC_ORIGIN 未配置：无法生成可信的 device verification_url")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("PUBLIC_ORIGIN 非法值 %q：无法解析 (%v)", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("PUBLIC_ORIGIN 非法值 %q：scheme 必须为 http 或 https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("PUBLIC_ORIGIN 非法值 %q：必须包含 host", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("PUBLIC_ORIGIN 非法值 %q：不得包含 userinfo", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("PUBLIC_ORIGIN 非法值 %q：必须为 origin，不得包含路径", raw)
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("PUBLIC_ORIGIN 非法值 %q：不得包含 query 参数", raw)
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("PUBLIC_ORIGIN 非法值 %q：不得包含 fragment", raw)
	}

	return strings.TrimRight(raw, "/") + "/device-authorize", nil
}

// recordDeviceAuthAudit 写入 device auth 事件审计日志。
// 引入动机：approve/deny/completed 需要追踪操作者与结果，但不能泄露 user_code、device_code 或 token。
// detail 只能包含非敏感信息（例如 device_name、status）。
func (h *DeviceAuthHandler) recordDeviceAuthAudit(r *http.Request, userID, action, resourceID string, detail map[string]string) {
	if h.auditRepo == nil {
		return
	}

	var detailJSON json.RawMessage
	if detail != nil {
		data, err := json.Marshal(detail)
		if err != nil {
			slog.Error("序列化 device auth 审计 detail 失败", "error", err, "action", action)
			return
		}
		detailJSON = data
	}

	requestID := requestid.RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = r.Header.Get("X-Request-ID")
	}

	if err := h.auditRepo.Record(r.Context(), userID, "", action, "device_authorization", resourceID, detailJSON, requestID); err != nil {
		slog.Error("写入 device auth 审计日志失败", "error", err, "action", action, "resource_id", resourceID)
	}
}

// HandleStart 处理 POST /api/auth/device/start。
// 引入动机：CLI login 命令调用此端点启动 device authorization 流程。
// 此端点不需要认证（公开），因为用户尚未登录。
func (h *DeviceAuthHandler) HandleStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceName string `json:"device_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	// device_name 为空时由 DeviceAuthStart 生成安全 fallback（不含敏感信息）。

	// 构建 verification URL：只能使用配置中的可信 public origin，禁止从请求 Host 推导。
	// 生产缺少合法 origin 时 fail closed，避免 Host header 注入。
	verificationURLPrefix, err := h.verificationURLPrefix()
	if err != nil {
		slog.Error("device auth verification URL 配置错误", "error", err)
		writeError(w, http.StatusInternalServerError, "服务配置错误")
		return
	}

	result, err := DeviceAuthStart(r.Context(), h.daRepo, h.daCfg, req.DeviceName, verificationURLPrefix)
	if err != nil {
		slog.Error("device auth start 失败", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"device_code":      result.DeviceCode,
		"user_code":        result.UserCode,
		"verification_url": result.VerificationURL,
		"expires_in":       result.ExpiresIn,
		"interval":         result.Interval,
	})
}

// HandlePoll 处理 POST /api/auth/device/poll。
// 引入动机：CLI login 命令轮询此端点获取授权状态。
// 此端点不需要认证（公开）。
func (h *DeviceAuthHandler) HandlePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.DeviceCode == "" {
		writeError(w, http.StatusBadRequest, "device_code 不能为空")
		return
	}

	result, err := DeviceAuthPoll(r.Context(), h.daRepo, h.authCfg, req.DeviceCode)
	if err != nil {
		slog.Error("device auth poll 失败", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	switch result.Status {
	case "pending":
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
	case "authorized":
		// 写入 device.auth.completed 审计：记录授权完成事件，但绝不包含 code/token。
		record, recordErr := h.daRepo.GetDeviceAuthorizationByDeviceCode(r.Context(), req.DeviceCode)
		if recordErr != nil {
			slog.Error("查询 device authorization 失败", "error", recordErr)
		} else {
			uid := ""
			if record.UserID.Valid {
				uid = record.UserID.String
			}
			h.recordDeviceAuthAudit(r, uid, "device.auth.completed", record.ID, map[string]string{
				"device_name": record.DeviceName,
				"status":      "completed",
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":        "authorized",
			"access_token":  result.AccessToken,
			"refresh_token": result.RefreshToken,
			"token_type":    result.TokenType,
			"expires_in":    result.ExpiresIn,
		})
	case "denied":
		writeError(w, http.StatusForbidden, "用户拒绝了授权")
	case "expired":
		writeError(w, http.StatusGone, "device code 已过期")
	case "completed":
		// 授权已完成一次性交换，不再颁发 token。客户端应认为已成功完成。
		writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
	default:
		writeError(w, http.StatusInternalServerError, "未知状态")
	}
}

// HandleApprove 处理 POST /api/auth/device/approve。
// 引入动机：用户通过 Web UI 批准 device authorization。
// 此端点需要认证 + CSRF（cookie session 认证的状态变更请求）。
func (h *DeviceAuthHandler) HandleApprove(w http.ResponseWriter, r *http.Request) {
	id := IdentityFromContext(r.Context())
	if id == nil {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.UserCode == "" {
		writeError(w, http.StatusBadRequest, "user_code 不能为空")
		return
	}

	if err := DeviceAuthApprove(r.Context(), h.daRepo, req.UserCode, id.UserID); err != nil {
		slog.Info("device auth approve 失败", "error", err, "user_id", id.UserID)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 写入 device.auth.approve 审计：仅记录授权 ID 与非敏感信息，不包含 user_code/token。
	record, recordErr := h.daRepo.GetDeviceAuthorizationByUserCodeAny(r.Context(), req.UserCode)
	if recordErr != nil {
		slog.Error("查询已批准 device authorization 失败", "error", recordErr)
	} else {
		h.recordDeviceAuthAudit(r, id.UserID, "device.auth.approve", record.ID, map[string]string{
			"device_name": record.DeviceName,
			"status":      "authorized",
		})
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// HandleDeny 处理 POST /api/auth/device/deny。
// 引入动机：用户通过 Web UI 拒绝 device authorization。
// 此端点需要认证 + CSRF。
func (h *DeviceAuthHandler) HandleDeny(w http.ResponseWriter, r *http.Request) {
	id := IdentityFromContext(r.Context())
	if id == nil {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req struct {
		UserCode string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.UserCode == "" {
		writeError(w, http.StatusBadRequest, "user_code 不能为空")
		return
	}

	record, err := h.daRepo.GetDeviceAuthorizationByUserCode(r.Context(), req.UserCode)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "user code 不存在")
			return
		}
		slog.Error("查询 device authorization 失败", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	if record.Status != "pending" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("状态不是 pending: %s", record.Status))
		return
	}

	if err := h.daRepo.DenyDeviceAuthorization(r.Context(), record.ID); err != nil {
		slog.Error("拒绝 device authorization 失败", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 写入 device.auth.deny 审计：记录拒绝事件，绝不包含 user_code/token。
	h.recordDeviceAuthAudit(r, id.UserID, "device.auth.deny", record.ID, map[string]string{
		"device_name": record.DeviceName,
		"status":      "denied",
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
}

// HandleInfo 处理 GET /api/auth/device/info。
// 引入动机：device-authorize 页面需要向已登录用户展示授权安全元数据。
// 最小攻击面：仅返回状态、非敏感 device display name 和过期信息；
// 绝不返回 device_code、user_id、token 或 user 身份。
// 此端点需要认证（GET，无 CSRF），避免公开泄露 code 有效性。
func (h *DeviceAuthHandler) HandleInfo(w http.ResponseWriter, r *http.Request) {
	if IdentityFromContext(r.Context()) == nil {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}

	userCode := r.URL.Query().Get("code")
	if userCode == "" {
		writeError(w, http.StatusBadRequest, "code 不能为空")
		return
	}

	record, err := h.daRepo.GetDeviceAuthorizationByUserCodeAny(r.Context(), userCode)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "授权码不存在")
			return
		}
		slog.Error("查询 device authorization 信息失败", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 若仍 pending 且已过期，惰性标记过期，返回 expired 状态。
	if record.Status == "pending" && time.Now().After(record.ExpiresAt) {
		_ = h.daRepo.ExpireDeviceAuthorizations(r.Context())
		record.Status = "expired"
	}

	expiresIn := int64(time.Until(record.ExpiresAt).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      record.Status,
		"device_name": record.DeviceName,
		"expires_in":  expiresIn,
	})
}

// RegisterDeviceAuthRoutes 注册 device authorization 路由。
// 引入动机：将 device auth 路由注册集中在一处。
//
// 路由清单：
//   - POST /api/auth/device/start  — 公开，启动 device authorization
//   - POST /api/auth/device/poll   — 公开，轮询授权状态
//   - POST /api/auth/device/approve — 需认证 + CSRF，用户批准授权
//   - POST /api/auth/device/deny    — 需认证 + CSRF，用户拒绝授权
func RegisterDeviceAuthRoutes(mux *http.ServeMux, handler *DeviceAuthHandler, authRepo Repository, cfg AuthConfig) {
	// 公开端点
	mux.HandleFunc("POST /api/auth/device/start", handler.HandleStart)
	mux.HandleFunc("POST /api/auth/device/poll", handler.HandlePoll)

	// 需要认证 + CSRF 的端点
	protected := AuthMiddleware(authRepo, cfg)(RequireAuth(RequireCSRF(authRepo, cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/device/approve":
			handler.HandleApprove(w, r)
		case "/api/auth/device/deny":
			handler.HandleDeny(w, r)
		default:
			writeError(w, http.StatusNotFound, "未找到路由")
		}
	}))))

	mux.Handle("POST /api/auth/device/approve", protected)
	mux.Handle("POST /api/auth/device/deny", protected)

	// GET /api/auth/device/info — 已认证用户读取授权安全元数据（无 code/token/user）。
	authenticated := AuthMiddleware(authRepo, cfg)(RequireAuth(http.HandlerFunc(handler.HandleInfo)))
	mux.Handle("GET /api/auth/device/info", authenticated)
}

// PGDeviceAuthRepository 是 DeviceAuthRepository 的 PostgreSQL 实现。
// 引入动机：使用 database/sql + pgx 访问 device_authorizations 表。
type PGDeviceAuthRepository struct {
	db *sql.DB
}

// NewPGDeviceAuthRepository 创建 PGDeviceAuthRepository。
func NewPGDeviceAuthRepository(db *sql.DB) *PGDeviceAuthRepository {
	return &PGDeviceAuthRepository{db: db}
}

// CreateDeviceAuthorization 创建 device authorization 记录。
func (r *PGDeviceAuthRepository) CreateDeviceAuthorization(ctx context.Context, deviceCode, userCode, deviceName string, expiresAt time.Time, pollInterval int) (string, error) {
	const q = `INSERT INTO device_authorizations (device_code, user_code, device_name, expires_at, poll_interval_seconds) VALUES ($1, $2, $3, $4, $5) RETURNING id`

	var id string
	err := r.db.QueryRowContext(ctx, q, deviceCode, userCode, deviceName, expiresAt, pollInterval).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("创建 device authorization: %w", err)
	}
	return id, nil
}

// GetDeviceAuthorizationByDeviceCode 根据 device code 查询记录。
func (r *PGDeviceAuthRepository) GetDeviceAuthorizationByDeviceCode(ctx context.Context, deviceCode string) (*DeviceAuthorizationRecord, error) {
	const q = `SELECT id, device_code, user_code, device_name, user_id, status, expires_at, poll_interval_seconds, created_at, updated_at FROM device_authorizations WHERE device_code = $1`

	var rec DeviceAuthorizationRecord
	err := r.db.QueryRowContext(ctx, q, deviceCode).Scan(
		&rec.ID, &rec.DeviceCode, &rec.UserCode, &rec.DeviceName, &rec.UserID,
		&rec.Status, &rec.ExpiresAt, &rec.PollIntervalSeconds, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetDeviceAuthorizationByUserCode 根据 user code 查询 pending 记录。
func (r *PGDeviceAuthRepository) GetDeviceAuthorizationByUserCode(ctx context.Context, userCode string) (*DeviceAuthorizationRecord, error) {
	const q = `SELECT id, device_code, user_code, device_name, user_id, status, expires_at, poll_interval_seconds, created_at, updated_at FROM device_authorizations WHERE user_code = $1 AND status = 'pending'`

	var rec DeviceAuthorizationRecord
	err := r.db.QueryRowContext(ctx, q, userCode).Scan(
		&rec.ID, &rec.DeviceCode, &rec.UserCode, &rec.DeviceName, &rec.UserID,
		&rec.Status, &rec.ExpiresAt, &rec.PollIntervalSeconds, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// ApproveDeviceAuthorization 设置 user_id 和 status=authorized。
func (r *PGDeviceAuthRepository) ApproveDeviceAuthorization(ctx context.Context, id, userID string) error {
	const q = `UPDATE device_authorizations SET user_id = $1, status = 'authorized', updated_at = now() WHERE id = $2 AND status = 'pending'`

	result, err := r.db.ExecContext(ctx, q, userID, id)
	if err != nil {
		return fmt.Errorf("批准 device authorization: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取影响行数: %w", err)
	}
	if rows == 0 {
		return errors.New("device authorization 不存在或状态不是 pending")
	}
	return nil
}

// GetDeviceAuthorizationByUserCodeAny 根据 user code 查询任意状态记录。
// 引入动机：device-authorize 页面需要展示授权安全元数据，不限制 pending 状态。
// 不返回 device_code 等敏感凭证。
func (r *PGDeviceAuthRepository) GetDeviceAuthorizationByUserCodeAny(ctx context.Context, userCode string) (*DeviceAuthorizationRecord, error) {
	const q = `SELECT id, device_code, user_code, device_name, user_id, status, expires_at, poll_interval_seconds, created_at, updated_at FROM device_authorizations WHERE user_code = $1`

	var rec DeviceAuthorizationRecord
	err := r.db.QueryRowContext(ctx, q, userCode).Scan(
		&rec.ID, &rec.DeviceCode, &rec.UserCode, &rec.DeviceName, &rec.UserID,
		&rec.Status, &rec.ExpiresAt, &rec.PollIntervalSeconds, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// DenyDeviceAuthorization 设置 status=denied。
func (r *PGDeviceAuthRepository) DenyDeviceAuthorization(ctx context.Context, id string) error {
	const q = `UPDATE device_authorizations SET status = 'denied', updated_at = now() WHERE id = $1 AND status = 'pending'`

	result, err := r.db.ExecContext(ctx, q, id)
	if err != nil {
		return fmt.Errorf("拒绝 device authorization: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取影响行数: %w", err)
	}
	if rows == 0 {
		return errors.New("device authorization 不存在或状态不是 pending")
	}
	return nil
}

// ExpireDeviceAuthorizations 将过期的 pending 记录标记为 expired。
func (r *PGDeviceAuthRepository) ExpireDeviceAuthorizations(ctx context.Context) error {
	const q = `UPDATE device_authorizations SET status = 'expired', updated_at = now() WHERE status = 'pending' AND expires_at < now()`

	_, err := r.db.ExecContext(ctx, q)
	if err != nil {
		return fmt.Errorf("过期 device authorization: %w", err)
	}
	return nil
}

// ExchangeDeviceAuthorization 原子地将 status=authorized 的授权转为 completed，
// 并在同一事务中创建 hash-only device session。
// 引入动机：Phase6 要求 token 一次性交换——任何重复或并发 poll
// 不可再创建 session 或颁发 token。
//
// 返回：
//   - (userID, deviceName, nil)：成功完成交换
//   - ErrDeviceAuthPending/Denied/Expired/Completed：对应终端状态
//   - sql.ErrNoRows：device_code 不存在
func (r *PGDeviceAuthRepository) ExchangeDeviceAuthorization(
	ctx context.Context,
	deviceCode, accessTokenHash, refreshTokenHash string,
	expiresAt, refreshExpiresAt time.Time,
) (userID, deviceName string, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("开启交换事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// 锁定授权行，防止并发 poll 同时进入交换分支
	var (
		authID   string
		status   string
		userIDNS sql.NullString
		name     string
		expires  time.Time
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, user_id, device_name, status, expires_at
		 FROM device_authorizations WHERE device_code = $1 FOR UPDATE`,
		deviceCode,
	).Scan(&authID, &userIDNS, &name, &status, &expires)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", sql.ErrNoRows
		}
		return "", "", fmt.Errorf("锁定 device authorization: %w", err)
	}

	now := time.Now()
	switch {
	case status == "completed":
		return "", "", ErrDeviceAuthCompleted
	case status == "denied":
		return "", "", ErrDeviceAuthDenied
	case status == "expired":
		return "", "", ErrDeviceAuthExpired
	case status == "pending" && now.After(expires):
		return "", "", ErrDeviceAuthExpired
	case status != "authorized":
		return "", "", ErrDeviceAuthPending
	case !userIDNS.Valid:
		return "", "", ErrDeviceAuthNoUser
	}

	// 将 authorization 转为 completed（CAS），防止重复交换
	result, err := tx.ExecContext(ctx,
		`UPDATE device_authorizations SET status = 'completed', updated_at = now()
		 WHERE id = $1 AND status = 'authorized'`,
		authID,
	)
	if err != nil {
		return "", "", fmt.Errorf("更新 authorization 状态为 completed: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", "", fmt.Errorf("获取影响行数: %w", err)
	}
	if rows != 1 {
		return "", "", ErrDeviceAuthCompleted
	}

	// 创建 hash-only device session（与 auth.Repository 的字段一致）
	_, err = tx.ExecContext(ctx,
		`INSERT INTO device_sessions
		 (user_id, device_name, access_token_hash, refresh_token_hash, expires_at, refresh_expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		userIDNS.String, name, accessTokenHash, refreshTokenHash, expiresAt, refreshExpiresAt,
	)
	if err != nil {
		return "", "", fmt.Errorf("创建 device session: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return "", "", fmt.Errorf("提交交换事务: %w", err)
	}

	return userIDNS.String, name, nil
}
