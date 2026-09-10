// handler.go 实现 auth 模块的 HTTP handler。
//
// 引入动机：design/04-WEB-API.md §Auth/Device 要求 login/session、device authorization、
// refresh/revoke 三个最低端点能力。handler 只负责 HTTP 输入输出（解析请求、设置 cookie、
// 写入响应），认证领域逻辑委托给 authlogic.go 中的函数。
//
// 端点清单：
//   - POST /api/auth/login          — 公开，凭据认证，创建 Web session
//   - POST /api/auth/logout         — 需认证 + CSRF，撤销当前 session
//   - POST /api/auth/device/authorize — 需认证 + CSRF，创建 device session
//   - POST /api/auth/refresh        — 公开（需 refresh token），刷新 access token
//   - POST /api/auth/revoke         — 需认证 + CSRF，撤销指定 device session
package auth

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"partitura/server/internal/queryutil"
)

// Handler 是 auth 模块的 HTTP handler 集合。
// 引入动机：将 auth 相关的 HTTP handler 集中在一个结构体中，
// 通过依赖注入接收 repository 和配置，保持 handler 轻量。
type Handler struct {
	repo Repository
	cfg  AuthConfig
}

// NewHandler 创建 auth handler。
// 引入动机：main.go 通过此构造函数注入 repository 和配置。
func NewHandler(repo Repository, cfg AuthConfig) *Handler {
	return &Handler{repo: repo, cfg: cfg}
}

// loginRequest 是 login 端点的请求体。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse 是 login 端点的响应体。
type loginResponse struct {
	User struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		SystemRole string `json:"system_role"`
	} `json:"user"`
	CSRFToken string `json:"csrf_token"`
	ExpiresAt string `json:"expires_at"`
}

// Login 处理 POST /api/auth/login。
//
// 行为：
//  1. 解析 JSON 请求体获取 username 和 password
//  2. 调用 Login 领域逻辑验证凭据并创建 session
//  3. 设置 session cookie（HttpOnly, Secure, SameSite=Lax）
//  4. 设置 CSRF cookie（非 HttpOnly, Secure, SameSite=Lax）
//  5. 返回用户信息和 CSRF token
//
// 安全：
//   - 认证失败返回 401，错误信息不泄露用户是否存在
//   - 请求体解析失败返回 400
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "用户名和密码不能为空")
		return
	}

	result, err := Login(r.Context(), h.repo, h.cfg, req.Username, req.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "用户名或密码错误")
			return
		}
		slog.Error("login 内部错误", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 设置 session cookie（HttpOnly，前端 JS 不可读取）
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CookieName,
		Value:    result.SessionToken,
		Path:     h.cfg.CookiePath,
		Domain:   h.cfg.CookieDomain,
		MaxAge:   int(h.cfg.SessionDuration),
		Secure:   h.cfg.CookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// 设置 CSRF cookie（非 HttpOnly，前端 JS 可读取并以 header 回传）
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CSRFCookieName,
		Value:    result.CSRFToken,
		Path:     h.cfg.CookiePath,
		Domain:   h.cfg.CookieDomain,
		MaxAge:   int(h.cfg.SessionDuration),
		Secure:   h.cfg.CookieSecure,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})

	resp := loginResponse{
		CSRFToken: result.CSRFToken,
		ExpiresAt: result.ExpiresAt.UTC().Format(time.RFC3339),
	}
	resp.User.ID = result.UserID
	resp.User.Username = result.Username
	resp.User.SystemRole = result.SystemRole

	writeJSON(w, http.StatusOK, resp)
}

// Logout 处理 POST /api/auth/logout。
//
// 行为：
//  1. 从 cookie 中提取 session token
//  2. 调用 Logout 领域逻辑撤销 session
//  3. 清除 session cookie 和 CSRF cookie
//
// 此端点需要 CSRF 保护（状态变更 + cookie session 认证）。
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(h.cfg.CookieName)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}

	if err := Logout(r.Context(), h.repo, cookie.Value); err != nil {
		slog.Error("logout 内部错误", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	// 清除 session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CookieName,
		Value:    "",
		Path:     h.cfg.CookiePath,
		Domain:   h.cfg.CookieDomain,
		MaxAge:   -1,
		Secure:   h.cfg.CookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	// 清除 CSRF cookie
	http.SetCookie(w, &http.Cookie{
		Name:     h.cfg.CSRFCookieName,
		Value:    "",
		Path:     h.cfg.CookiePath,
		Domain:   h.cfg.CookieDomain,
		MaxAge:   -1,
		Secure:   h.cfg.CookieSecure,
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// deviceAuthorizeRequest 是 device authorization 端点的请求体。
type deviceAuthorizeRequest struct {
	DeviceName string `json:"device_name"`
}

// deviceAuthorizeResponse 是 device authorization 端点的响应体。
type deviceAuthorizeResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
}

// DeviceAuthorize 处理 POST /api/auth/device/authorize。
//
// 行为：
//  1. 从 request context 获取已认证身份
//  2. 解析 JSON 请求体获取 device_name
//  3. 调用 AuthorizeDevice 领域逻辑创建 device session
//  4. 返回 access token 和 refresh token
//
// 此端点需要认证 + CSRF 保护。
// token 仅在响应体中返回一次，数据库只存储哈希。
func (h *Handler) DeviceAuthorize(w http.ResponseWriter, r *http.Request) {
	id := IdentityFromContext(r.Context())
	if id == nil {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req deviceAuthorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	// device_name 可为空：服务端生成安全 fallback。
	result, err := AuthorizeDevice(r.Context(), h.repo, h.cfg, id.UserID, req.DeviceName)
	if err != nil {
		slog.Error("device authorize 内部错误", "error", err, "user_id", id.UserID)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := deviceAuthorizeResponse{
		AccessToken:      result.AccessToken,
		RefreshToken:     result.RefreshToken,
		TokenType:        "Bearer",
		ExpiresIn:        h.cfg.DeviceAccessTokenDuration,
		RefreshExpiresIn: h.cfg.DeviceRefreshTokenDuration,
	}

	writeJSON(w, http.StatusOK, resp)
}

// refreshRequest 是 token 刷新端点的请求体。
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse 是 token 刷新端点的响应体。
type refreshResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
}

// Refresh 处理 POST /api/auth/refresh。
//
// 行为：
//  1. 解析 JSON 请求体获取 refresh_token
//  2. 调用 RefreshToken 领域逻辑轮换 token
//  3. 返回新的 access token 和 refresh token
//
// 此端点不需要 CSRF 保护（使用 refresh token 而非 cookie session 认证）。
// 旧 token 在轮换后立即失效。
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}

	if req.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "refresh_token 不能为空")
		return
	}

	result, err := RefreshToken(r.Context(), h.repo, h.cfg, req.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrDeviceSessionRevoked) || errors.Is(err, ErrRefreshTokenExpired) {
			writeError(w, http.StatusUnauthorized, "refresh token 无效或已过期")
			return
		}
		slog.Error("refresh 内部错误", "error", err)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	resp := refreshResponse{
		AccessToken:      result.AccessToken,
		RefreshToken:     result.RefreshToken,
		TokenType:        "Bearer",
		ExpiresIn:        h.cfg.DeviceAccessTokenDuration,
		RefreshExpiresIn: h.cfg.DeviceRefreshTokenDuration,
	}

	writeJSON(w, http.StatusOK, resp)
}

// ListDeviceSessions 处理 GET /api/auth/device/sessions。
// 只返回当前登录用户自己的设备会话非敏感元数据，供设备管理页面展示。
func (h *Handler) ListDeviceSessions(w http.ResponseWriter, r *http.Request) {
	id := IdentityFromContext(r.Context())
	if id == nil {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}

	limit, offset, err := queryutil.ParsePagination(r)
	if err != nil {
		slog.Warn("解析设备会话分页参数失败", "error", err, "user_id", id.UserID)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.repo.ListDeviceSessions(r.Context(), id.UserID, limit, offset)
	if err != nil {
		slog.Error("查询设备会话列表失败", "error", err, "user_id", id.UserID)
		writeError(w, http.StatusInternalServerError, "查询设备会话失败")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions": result.Sessions,
		"total":    result.Total,
		"limit":    limit,
		"offset":   offset,
	})
}

// revokeRequest 是 device session 撤销端点的请求体。
type revokeRequest struct {
	DeviceSessionID string `json:"device_session_id"`
}

// Revoke 处理 POST /api/auth/revoke。
//
// 行为：
//  1. 从 request context 获取已认证身份
//  2. 解析 JSON 请求体获取 device_session_id（可选）
//     - 空请求体：撤销当前身份关联的 device session（bearer 认证时）
//     - 非空请求体：必须为合法 JSON，仅含 device_session_id 字段
//  3. 如果提供 device_session_id，撤销指定的 device session（需验证所有权）
//  4. 如果未提供，撤销当前身份关联的 device session（bearer 认证时）
//  5. 调用 RevokeDeviceSession 领域逻辑
//
// 此端点需要认证 + CSRF 保护（cookie session 认证时）。
// 安全：只允许用户撤销自己的 device session。
// 请求体安全：空 body 允许沿用默认撤销逻辑；畸形 JSON、未知字段、
// 多个 JSON 值均以 400 受控拒绝，不静默吞错。
func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	id := IdentityFromContext(r.Context())
	if id == nil {
		writeError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req revokeRequest

	// 读取并区分空 body 与非空 body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("读取 revoke 请求体失败", "error", err)
		writeError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	bodyBytes = bytes.TrimSpace(bodyBytes)

	if len(bodyBytes) > 0 {
		// 非空 body——严格解析，拒绝畸形 JSON、未知字段和多 JSON 值
		decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "请求体格式错误")
			return
		}
		// 检查是否存在额外的 JSON 值（如 `{}{}`）
		var extra json.RawMessage
		if err := decoder.Decode(&extra); err != io.EOF {
			writeError(w, http.StatusBadRequest, "请求体包含多个 JSON 值")
			return
		}
	}

	deviceSessionID := req.DeviceSessionID
	if deviceSessionID == "" {
		// 如果未提供 device_session_id，尝试撤销当前 bearer 认证的 device session
		if id.DeviceSessionID == "" {
			writeError(w, http.StatusBadRequest, "device_session_id 不能为空")
			return
		}
		deviceSessionID = id.DeviceSessionID
	} else {
		// 验证 device session 属于当前用户
		ds, err := h.repo.GetDeviceSessionByID(r.Context(), deviceSessionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "device session 不存在")
				return
			}
			slog.Error("查询 device session 失败", "error", err, "device_session_id", deviceSessionID)
			writeError(w, http.StatusInternalServerError, "内部错误")
			return
		}
		if ds.UserID != id.UserID {
			// 不允许撤销其他用户的 device session
			slog.Info("拒绝撤销非本人 device session", "user_id", id.UserID, "device_session_owner", ds.UserID)
			writeError(w, http.StatusForbidden, "无权撤销此 device session")
			return
		}
	}

	if err := RevokeDeviceSession(r.Context(), h.repo, deviceSessionID); err != nil {
		slog.Error("revoke 内部错误", "error", err, "device_session_id", deviceSessionID)
		writeError(w, http.StatusInternalServerError, "内部错误")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
