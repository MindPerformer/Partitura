// Package client 实现 Knowledge Server 的 HTTPS REST 客户端。
//
// 引入动机：design/02-MCP.md §架构 要求 MCP binary 通过 HTTPS REST 与 Knowledge Server 通信。
// MCP client 不直接访问 server 的 PostgreSQL 或 Elasticsearch，只通过 REST API。
//
// 设计原则：
//   - 所有请求使用 HTTPS（允许测试场景的自签证书）
//   - bearer token 认证（Authorization: Bearer <access_token>）
//   - token 过期时自动 refresh
//   - 网络错误、认证错误、协议错误受控返回，不 panic
//   - 日志仅输出到 stderr，不输出 token 或文档全文
//   - 不使用反射
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"crypto/tls"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CredentialStore 定义 client 需要的凭据存储接口。
// 引入动机：client 需要在 access token 过期时读取 refresh token 并刷新。
// 使用接口而非直接依赖 credential.Store，便于测试注入替身。
type CredentialStore interface {
	Load(serverURL string) (*CredentialTokens, error)
	Save(serverURL string, tokens *CredentialTokens) error
	Delete(serverURL string) error
}

// CredentialTokens 是 client 需要的凭据结构。
// 引入动机：与 credential.Tokens 对齐但独立定义，避免循环依赖。
type CredentialTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ServerURL    string `json:"server_url"`
}

// APIError 表示 server REST API 返回的错误。
// 引入动机：MCP 工具需要将 server 错误映射为结构化的 MCP 错误信息。
type APIError struct {
	// StatusCode 是 HTTP 状态码。
	StatusCode int

	// Message 是 server 返回的错误消息。
	Message string

	// Body 是 server 返回的原始响应体（用于 409 冲突等需要详细信息的场景）。
	Body string
}

// Error 实现 error 接口。
func (e *APIError) Error() string {
	return fmt.Sprintf("API 错误 %d: %s", e.StatusCode, e.Message)
}

// IsNotFound 判断是否为 404 错误。
func (e *APIError) IsNotFound() bool {
	return e.StatusCode == http.StatusNotFound
}

// IsConflict 判断是否为 409 冲突错误。
// 引入动机：document_patch 在 409 时尝试一次安全 rebase。
func (e *APIError) IsConflict() bool {
	return e.StatusCode == http.StatusConflict
}

// IsUnauthorized 判断是否为 401 未认证错误。
// 引入动机：access token 过期时 server 返回 401，client 应尝试 refresh。
func (e *APIError) IsUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized
}

// IsForbidden 判断是否为 403 权限不足错误。
func (e *APIError) IsForbidden() bool {
	return e.StatusCode == http.StatusForbidden
}

// Client 是 Knowledge Server 的 HTTPS REST 客户端。
// 引入动机：MCP 工具通过此客户端调用 server REST API。
type Client struct {
	// serverURL 是 server 的 base URL（如 "https://knowledge.company.com"）。
	serverURL string

	// httpClient 是底层 HTTP 客户端。
	httpClient *http.Client

	// credStore 是凭据存储接口。
	credStore CredentialStore

	// tokens 是当前内存中的 token。
	tokens *CredentialTokens
}

// NewClient 创建 REST 客户端。
// 引入动机：MCP 进程启动时创建 client，注入 server URL 和凭据存储。
//
// 参数：
//   - serverURL：server 的 HTTPS base URL
//   - credStore：凭据存储接口
//   - allowInsecureTLS：是否允许跳过 TLS 证书验证（仅测试场景）
func NewClient(serverURL string, credStore CredentialStore, allowInsecureTLS bool) *Client {
	serverURL = strings.TrimRight(serverURL, "/")

	transport := &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  false,
		MaxIdleConnsPerHost: 5,
	}

	if allowInsecureTLS {
		// 仅测试场景：允许自签证书
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Client{
		serverURL: serverURL,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   60 * time.Second,
		},
		credStore: credStore,
	}
}

// LoadTokens 从凭据存储加载已保存的 token。
// 引入动机：MCP 进程启动时需要加载已登录的 token。
func (c *Client) LoadTokens() error {
	tokens, err := c.credStore.Load(c.serverURL)
	if err != nil {
		return fmt.Errorf("加载凭据: %w", err)
	}
	if tokens != nil && tokens.AccessToken != "" {
		c.tokens = tokens
	}
	return nil
}

// IsLoggedIn 返回是否已加载有效 token。
func (c *Client) IsLoggedIn() bool {
	return c.tokens != nil && c.tokens.AccessToken != ""
}

// SetTokens 直接设置 token（用于 login 命令成功后）。
func (c *Client) SetTokens(tokens *CredentialTokens) {
	c.tokens = tokens
}

// SaveTokens 将当前 token 保存到凭据存储。
func (c *Client) SaveTokens() error {
	if c.tokens == nil {
		return fmt.Errorf("无 token 可保存")
	}
	return c.credStore.Save(c.serverURL, c.tokens)
}

// ClearTokens 清除内存和持久化的 token。
// 引入动机：logout 命令需要清除全部凭据。
func (c *Client) ClearTokens() error {
	c.tokens = nil
	return c.credStore.Delete(c.serverURL)
}

// doRequest 执行 HTTP 请求，处理认证和 token 刷新。
// 引入动机：所有 API 调用共用此方法，统一处理 bearer token 注入和 401 自动刷新。
func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, []byte, error) {
	if !c.IsLoggedIn() {
		return nil, nil, fmt.Errorf("未登录，请先执行 knowledge-mcp login")
	}

	resp, respBody, err := c.executeWithToken(ctx, method, path, body, c.tokens.AccessToken)
	if err != nil {
		return nil, nil, err
	}

	// 401 时尝试一次 token 刷新
	if resp.StatusCode == http.StatusUnauthorized {
		slog.Debug("access token 过期，尝试刷新")
		if err := c.refreshToken(ctx); err != nil {
			return nil, nil, fmt.Errorf("刷新 token 失败: %w", err)
		}
		// 用新 token 重试
		return c.executeWithToken(ctx, method, path, body, c.tokens.AccessToken)
	}

	return resp, respBody, nil
}

// executeWithToken 使用指定 access token 执行 HTTP 请求。
func (c *Client) executeWithToken(ctx context.Context, method, path string, body interface{}, accessToken string) (*http.Response, []byte, error) {
	fullURL := c.serverURL + path

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("序列化请求体: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, nil, fmt.Errorf("创建请求: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("读取响应体: %w", err)
	}

	return resp, respBody, nil
}

// refreshToken 使用 refresh token 获取新的 access token。
// 引入动机：access token 过期后自动刷新，不需要用户重新登录。
func (c *Client) refreshToken(ctx context.Context) error {
	if c.tokens == nil || c.tokens.RefreshToken == "" {
		return fmt.Errorf("无 refresh token 可用")
	}

	reqBody := map[string]string{"refresh_token": c.tokens.RefreshToken}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化 refresh 请求: %w", err)
	}

	fullURL := c.serverURL + "/api/auth/refresh"
	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("创建 refresh 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取 refresh 响应体: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("token 刷新失败", "status", resp.StatusCode)
		c.tokens = nil
		_ = c.credStore.Delete(c.serverURL)
		return fmt.Errorf("refresh 失败 (HTTP %d)", resp.StatusCode)
	}

	var refreshResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &refreshResp); err != nil {
		return fmt.Errorf("解析 refresh 响应: %w", err)
	}

	if refreshResp.AccessToken == "" {
		return fmt.Errorf("refresh 响应缺少 access_token")
	}

	c.tokens.AccessToken = refreshResp.AccessToken
	if refreshResp.RefreshToken != "" {
		c.tokens.RefreshToken = refreshResp.RefreshToken
	}

	// 持久化新 token
	if err := c.credStore.Save(c.serverURL, c.tokens); err != nil {
		slog.Error("持久化刷新后的 token 失败", "error", err)
		return fmt.Errorf("持久化 token: %w", err)
	}

	return nil
}

// DeviceAuthStart 发起 device authorization 流程。
// 引入动机：MCP CLI login 命令需要启动 device authorization 流程。
// 调用 server 的 POST /api/auth/device/start 端点。
func (c *Client) DeviceAuthStart(ctx context.Context, deviceName string) (*DeviceAuthStartResponse, error) {
	fullURL := c.serverURL + "/api/auth/device/start"

	reqBody := map[string]string{"device_name": deviceName}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("创建请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device auth start 失败 (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result DeviceAuthStartResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析响应: %w", err)
	}

	return &result, nil
}

// DeviceAuthStartResponse 是 device authorization start 端点的响应。
type DeviceAuthStartResponse struct {
	// DeviceCode 是设备授权码，用于 poll 端点。
	DeviceCode string `json:"device_code"`

	// UserCode 是用户授权码，用户在浏览器中输入此码完成授权。
	UserCode string `json:"user_code"`

	// VerificationURL 是用户在浏览器中访问的授权页面 URL。
	VerificationURL string `json:"verification_url"`

	// ExpiresIn 是 device code 的有效期（秒）。
	ExpiresIn int `json:"expires_in"`

	// Interval 是 poll 间隔（秒）。
	Interval int `json:"interval"`
}

// DeviceAuthPoll 轮询 device authorization 状态。
// 引入动机：MCP CLI login 命令需要轮询授权状态，直到用户批准或超时。
func (c *Client) DeviceAuthPoll(ctx context.Context, deviceCode string) (*DeviceAuthPollResponse, error) {
	fullURL := c.serverURL + "/api/auth/device/poll"

	reqBody := map[string]string{"device_code": deviceCode}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化请求: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("创建请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应体: %w", err)
	}

	var result DeviceAuthPollResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析响应: %w", err)
	}

	// 根据状态码判断结果
	switch resp.StatusCode {
	case http.StatusOK:
		if result.Status == "completed" {
			// 授权已一次性交换完成：本进程不能再获取 token。
			return &result, ErrAuthorizationCompleted
		}
		// 授权成功
		return &result, nil
	case statusPending:
		// 待授权 (HTTP 202)
		return &result, ErrPendingAuthorization
	case http.StatusForbidden:
		// 授权被拒绝
		return &result, ErrAuthorizationDenied
	case http.StatusGone:
		// device code 过期
		return &result, ErrDeviceCodeExpired
	default:
		return nil, fmt.Errorf("device auth poll 失败 (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
}

// DeviceAuthPollResponse 是 device authorization poll 端点的响应。
type DeviceAuthPollResponse struct {
	// Status 是授权状态：pending, authorized, completed, denied, expired。
	Status string `json:"status"`

	// AccessToken 是授权成功后的 access token（仅 status=authorized 时有值）。
	AccessToken string `json:"access_token,omitempty"`

	// RefreshToken 是授权成功后的 refresh token（仅 status=authorized 时有值）。
	RefreshToken string `json:"refresh_token,omitempty"`

	// TokenType 是 token 类型（如 "Bearer"）。
	TokenType string `json:"token_type,omitempty"`

	// ExpiresIn 是 access token 的有效期（秒）。
	ExpiresIn int `json:"expires_in,omitempty"`
}

// 授权状态错误
var (
	ErrPendingAuthorization  = fmt.Errorf("授权待批准")
	ErrAuthorizationDenied   = fmt.Errorf("用户拒绝了授权")
	ErrDeviceCodeExpired     = fmt.Errorf("device code 已过期")
	// ErrAuthorizationCompleted 表示授权已被一次性交换完成（另一 poll 已领取 token）。
	ErrAuthorizationCompleted = fmt.Errorf("device authorization 已完成交换")
)

// statusPending 是待处理的 HTTP 状态码（HTTP 202 Accepted）。
// 引入动机：device auth poll 端点在用户尚未批准时返回 202。
const statusPending = 202

// --- REST API 调用方法 ---

// Get 发送 GET 请求并解析 JSON 响应。
// 引入动机：document read/outline/section/lines/history/revision 等工具使用 GET。
func (c *Client) Get(ctx context.Context, path string, result interface{}) error {
	resp, body, err := c.doRequest(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	return parseResponse(resp, body, result)
}

// Post 发送 POST 请求并解析 JSON 响应。
// 引入动机：document create/move/archive/search 等工具使用 POST。
func (c *Client) Post(ctx context.Context, path string, body interface{}, result interface{}) error {
	resp, respBody, err := c.doRequest(ctx, "POST", path, body)
	if err != nil {
		return err
	}
	return parseResponse(resp, respBody, result)
}

// Put 发送 PUT 请求并解析 JSON 响应。
// 引入动机：document replace 工具使用 PUT。
func (c *Client) Put(ctx context.Context, path string, body interface{}, result interface{}) error {
	resp, respBody, err := c.doRequest(ctx, "PUT", path, body)
	if err != nil {
		return err
	}
	return parseResponse(resp, respBody, result)
}

// Patch 发送 PATCH 请求并解析 JSON 响应。
// 引入动机：document patch 工具使用 PATCH。
func (c *Client) Patch(ctx context.Context, path string, body interface{}, result interface{}) error {
	resp, respBody, err := c.doRequest(ctx, "PATCH", path, body)
	if err != nil {
		return err
	}
	return parseResponse(resp, respBody, result)
}

// Delete 发送 DELETE 请求。
// 引入动机：source delete 等工具使用 DELETE。
func (c *Client) Delete(ctx context.Context, path string) error {
	resp, respBody, err := c.doRequest(ctx, "DELETE", path, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	return nil
}

// RevokeSession 调用 server POST /api/auth/revoke 撤销当前 device session。
// 引入动机：design/02-MCP.md §登录 要求 logout 命令先尝试调用 server 撤销 session，
// 再删除本地凭据。使用空 body（bearer 认证时 server 自动撤销当前 device session）。
// 安全：不输出 token。
func (c *Client) RevokeSession(ctx context.Context) error {
	resp, respBody, err := c.doRequest(ctx, "POST", "/api/auth/revoke", nil)
	if err != nil {
		return fmt.Errorf("revoke 请求失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	return nil
}

// GetRaw 发送 GET 请求并返回原始响应体（不解析 JSON）。
// 引入动机：某些场景需要获取原始响应体（如 409 冲突时获取最新内容）。
func (c *Client) GetRaw(ctx context.Context, path string) (*http.Response, []byte, error) {
	return c.doRequest(ctx, "GET", path, nil)
}

// PostRaw 发送 POST 请求并返回原始响应体。
// 引入动机：某些场景需要获取原始响应体。
func (c *Client) PostRaw(ctx context.Context, path string, body interface{}) (*http.Response, []byte, error) {
	return c.doRequest(ctx, "POST", path, body)
}

// parseResponse 解析 HTTP 响应。
// 引入动机：统一处理响应状态码和 JSON 解析。
func parseResponse(resp *http.Response, body []byte, result interface{}) error {
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if unmarshalErr := json.Unmarshal(body, &errResp); unmarshalErr != nil {
			// error body 不是合法 JSON——记录 debug 级日志（不记录 body 正文/token）
			// 引入动机：design 要求不可静默丢错，但也不能在日志中泄露敏感信息。
			slog.Debug("error body 非 JSON，使用 fallback", "status_code", resp.StatusCode, "parse_error", unmarshalErr.Error())
			// 安全 fallback：使用 HTTP 状态码作为错误信息
			msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
			return &APIError{
				StatusCode: resp.StatusCode,
				Message:    msg,
				Body:       string(body),
			}
		}
		msg := errResp.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    msg,
			Body:       string(body),
		}
	}

	if result != nil && len(body) > 0 {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("解析响应 JSON: %w", err)
		}
	}

	return nil
}

// BuildQueryParams 构建 URL 查询参数字符串。
// 引入动机：document read/outline/section/lines 等工具需要构建查询参数。
func BuildQueryParams(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	return "?" + v.Encode()
}
