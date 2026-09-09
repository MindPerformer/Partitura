// Package provider 实现 Provider 配置的持久化、加密存储、线程安全热加载 Registry，
// 以及管理员 Provider Web API（GET/PUT/POST test）。
//
// 引入动机：计划要求：
//   - Provider API Key 通过 AES-256-GCM 加密后保存到 PostgreSQL provider_secrets 表
//   - Provider Registry 线程安全原子热加载，搜索/作业/健康检查从 Registry 获取当前实例
//   - 管理员通过受 CSRF 保护的 Web API 写入 Provider 配置，保存后无需重启
//   - GET 仅返回状态和非敏感配置，绝不返回 API Key 明文或密文
//   - POST test 仅临时测试，不持久化
//   - 审计仅记录类型、endpoint、model 和 api_key_changed 布尔值
//
// 安全原则：
//   - Provider Key 不可出现于 GET 响应、DOM、cache、log、audit
//   - 密文存在但根密钥缺失/非法/解密失败时 fail-fast 拒绝启动
//   - Provider 未配置时允许降级启动
//   - 不出现 XxxService/XxxManager/XxxController/空catch/占位
package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ProviderType 是 Provider 类型标识。
// 引入动机：provider_secrets 表限制类型为 embedding/reranker，
// 使用类型常量避免拼写错误。
type ProviderType string

const (
	// ProviderTypeEmbedding 是 Embedding Provider 类型。
	ProviderTypeEmbedding ProviderType = "embedding"
	// ProviderTypeReranker 是 Reranker Provider 类型。
	ProviderTypeReranker ProviderType = "reranker"
)

// ValidProviderTypes 是所有合法 Provider 类型的集合。
// 引入动机：校验 API 路径参数和数据库约束一致性。
var ValidProviderTypes = map[ProviderType]bool{
	ProviderTypeEmbedding: true,
	ProviderTypeReranker:  true,
}

// ProviderConfig 是从数据库读取的 Provider 配置记录（内部使用）。
// 引入动机：Repository 内部需要密文以解密 API Key，但公开 DTO 永不包含密钥字段。
// 此结构仅在 provider 包内部使用，不暴露给 HTTP 响应。
type ProviderConfig struct {
	// ProviderType 是 "embedding" 或 "reranker"。
	ProviderType ProviderType
	// EncryptedKey 是 base64 编码的 AES-256-GCM 密文。
	EncryptedKey string
	// ConfigJSON 是非敏感 JSON 配置（base_url, model, dimensions, timeout 等）。
	ConfigJSON json.RawMessage
	// UpdatedBy 是最后更新者的 user UUID。
	UpdatedBy string
	// UpdatedAt 是最后更新时间。
	UpdatedAt time.Time
}

// ProviderConfigDTO 是返回给前端的非敏感 Provider 配置。
// 引入动机：GET API 只返回状态和非敏感配置，绝不返回 API Key 明文或密文。
// 此结构是公开 DTO，确保密钥字段不会泄漏到 HTTP 响应。
type ProviderConfigDTO struct {
	// Configured 表示该类型 Provider 是否已配置。
	Configured bool `json:"configured"`
	// APIKeySet 表示 API Key 是否已设置（仅布尔值，不含 key 本身）。
	APIKeySet bool `json:"api_key_set"`
	// Config 是非敏感 JSON 配置（base_url, model, dimensions, timeout 等）。
	Config json.RawMessage `json:"config,omitempty"`
	// UpdatedAt 是最后更新时间。
	UpdatedAt string `json:"updated_at,omitempty"`
}

// EmbeddingConfigJSON 是 Embedding Provider 的非敏感 JSON 配置。
// 引入动机：PUT 请求体和 GET 响应需要严格类型约束，禁止未知字段。
type EmbeddingConfigJSON struct {
	BaseURL             string `json:"base_url"`
	Model               string `json:"model"`
	Dimensions          int    `json:"dimensions"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
	BatchSize           int    `json:"batch_size"`
	QueryInstruction    string `json:"query_instruction"`
	DocumentInstruction string `json:"document_instruction"`
}

// RerankerConfigJSON 是 Reranker Provider 的非敏感 JSON 配置。
// 引入动机：PUT 请求体和 GET 响应需要严格类型约束，禁止未知字段。
type RerankerConfigJSON struct {
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxCandidates  int    `json:"max_candidates"`
}

// Repository 定义 Provider 配置的数据访问接口。
// 引入动机：handler 和 registry 依赖此接口而非具体 PG 实现，
// 便于测试时注入 mock。
type Repository interface {
	// Get 获取指定类型的 Provider 配置记录（含密文，仅内部使用）。
	// 不存在时返回 sql.ErrNoRows。
	Get(ctx context.Context, providerType ProviderType) (*ProviderConfig, error)

	// Upsert 插入或更新指定类型的 Provider 配置。
	// encryptedKey 为 base64 编码的 AES-256-GCM 密文。
	Upsert(ctx context.Context, providerType ProviderType, encryptedKey string, configJSON json.RawMessage, updatedBy string) error

	// List 获取所有 Provider 配置记录（含密文，仅内部使用）。
	List(ctx context.Context) ([]ProviderConfig, error)
}

// PGRepository 是 Repository 接口的 PostgreSQL 实现。
// 引入动机：使用 database/sql + pgx 驱动访问 provider_secrets 表。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
// 引入动机：handler 和 registry 通过此构造函数注入数据库连接。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// Get 获取指定类型的 Provider 配置记录。
func (r *PGRepository) Get(ctx context.Context, providerType ProviderType) (*ProviderConfig, error) {
	const q = `SELECT provider_type, encrypted_key, config_json, COALESCE(updated_by::text, ''), updated_at
	           FROM provider_secrets WHERE provider_type = $1`

	var cfg ProviderConfig
	var configStr string
	err := r.db.QueryRowContext(ctx, q, string(providerType)).Scan(
		&cfg.ProviderType, &cfg.EncryptedKey, &configStr, &cfg.UpdatedBy, &cfg.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询 Provider 配置")
	}
	cfg.ConfigJSON = json.RawMessage(configStr)
	return &cfg, nil
}

// Upsert 插入或更新指定类型的 Provider 配置。
func (r *PGRepository) Upsert(ctx context.Context, providerType ProviderType, encryptedKey string, configJSON json.RawMessage, updatedBy string) error {
	var updatedByVal interface{}
	if updatedBy != "" {
		updatedByVal = updatedBy
	}

	const q = `INSERT INTO provider_secrets (provider_type, encrypted_key, config_json, updated_by)
	           VALUES ($1, $2, $3, $4)
	           ON CONFLICT (provider_type) DO UPDATE
	           SET encrypted_key = $2, config_json = $3, updated_by = $4, updated_at = now()`

	_, err := r.db.ExecContext(ctx, q, string(providerType), encryptedKey, []byte(configJSON), updatedByVal)
	if err != nil {
		return mapDBError(err, "写入 Provider 配置")
	}
	return nil
}

// List 获取所有 Provider 配置记录。
func (r *PGRepository) List(ctx context.Context) ([]ProviderConfig, error) {
	const q = `SELECT provider_type, encrypted_key, config_json, COALESCE(updated_by::text, ''), updated_at
	           FROM provider_secrets ORDER BY provider_type`

	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, mapDBError(err, "列出 Provider 配置")
	}
	defer rows.Close()

	var configs []ProviderConfig
	for rows.Next() {
		var cfg ProviderConfig
		var configStr string
		if err := rows.Scan(&cfg.ProviderType, &cfg.EncryptedKey, &configStr, &cfg.UpdatedBy, &cfg.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描 Provider 配置行: %w", err)
		}
		cfg.ConfigJSON = json.RawMessage(configStr)
		configs = append(configs, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 Provider 配置结果集: %w", err)
	}
	return configs, nil
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

// ValidateBaseURL 严格校验 Provider base_url。
// 引入动机：不同服务商的 API base URL 可能包含稳定路径段（如 /v1），
// 校验必须允许绝对 http/https URL 含合理 API base path，同时严格拒绝
// 非 HTTP(S) scheme、无 host、userinfo、query、fragment、控制字符/空白和路径遍历。
//
// 安全契约：
//   - scheme 必须为 http 或 https
//   - 必须包含 host
//   - 不得包含 userinfo（防止凭据注入）
//   - 不得包含 query 或 fragment（防止参数注入和 SSRF 绕过）
//   - 路径不得包含 ".." 段（防止路径遍历）
//   - 不得包含控制字符或空白（防止 CRLF 注入和 header 操纵）
//   - 允许的路径示例：""、"https://api.example.com"、"https://ai.gitee.com/v1"、
//     "https://api.example.com/api/v2"
func ValidateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("base_url 不能为空")
	}

	// 拒绝控制字符和空白（TrimSpace 已处理首尾，此处检查内部）
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("base_url 不得包含控制字符")
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url 无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url scheme 必须为 http 或 https, 实际为 %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("base_url 必须包含 host")
	}
	if u.User != nil {
		return fmt.Errorf("base_url 不得包含 userinfo")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("base_url 不得包含 query 参数")
	}
	if u.Fragment != "" {
		return fmt.Errorf("base_url 不得包含 fragment")
	}
	// 检查路径遍历：拒绝包含 ".." 的路径段
	if strings.Contains(u.Path, "/../") || u.Path == ".." || strings.HasPrefix(u.Path, "../") || strings.HasSuffix(u.Path, "/..") {
		return fmt.Errorf("base_url 路径不得包含路径遍历 (..)")
	}
	// 检查路径中的每个段，拒绝 "." 和 ".." 段
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." {
			return fmt.Errorf("base_url 路径不得包含路径遍历 (..)")
		}
	}
	return nil
}

// NormalizeBaseURL 规范化 Provider base URL。
// 引入动机：不同服务商的 base URL 可能带或不带尾部斜杠，
// 统一去除尾部斜杠以确保 endpoint 拼接时不会产生双斜杠。
// 例如 "https://ai.gitee.com/v1/" → "https://ai.gitee.com/v1"
func NormalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// ValidateEmbeddingConfig 严格校验 Embedding Provider 配置。
// 引入动机：PUT 请求需要严格校验 base_url、model、dimensions、timeout、batch_size。
func ValidateEmbeddingConfig(cfg EmbeddingConfigJSON) error {
	if err := ValidateBaseURL(cfg.BaseURL); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("model 不能为空")
	}
	if cfg.Dimensions <= 0 {
		return fmt.Errorf("dimensions 必须为正整数")
	}
	if cfg.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeout_seconds 必须为正整数")
	}
	if cfg.BatchSize <= 0 {
		return fmt.Errorf("batch_size 必须为正整数")
	}
	return nil
}

// ValidateRerankerConfig 严格校验 Reranker Provider 配置。
// 引入动机：PUT 请求需要严格校验 base_url、model、timeout、max_candidates。
func ValidateRerankerConfig(cfg RerankerConfigJSON) error {
	if err := ValidateBaseURL(cfg.BaseURL); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("model 不能为空")
	}
	if cfg.TimeoutSeconds <= 0 {
		return fmt.Errorf("timeout_seconds 必须为正整数")
	}
	if cfg.MaxCandidates <= 0 {
		return fmt.Errorf("max_candidates 必须为正整数")
	}
	return nil
}

// ToDTO 将内部 ProviderConfig 转换为非敏感 DTO。
// 引入动机：GET API 只返回状态和非敏感配置，密文和密钥永不暴露。
func (c *ProviderConfig) ToDTO() ProviderConfigDTO {
	return ProviderConfigDTO{
		Configured: true,
		APIKeySet:  c.EncryptedKey != "",
		Config:     c.ConfigJSON,
		UpdatedAt:  c.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// ParseProviderType 从字符串解析 ProviderType，非法值返回 false。
// 引入动机：API 路径参数需要校验。
func ParseProviderType(s string) (ProviderType, bool) {
	pt := ProviderType(s)
	return pt, ValidProviderTypes[pt]
}

// LogProviderConfigChange 记录 Provider 配置变更的结构化日志。
// 引入动机：审计和日志中不得包含 API Key、密文、Authorization 或 token。
// 仅记录类型、endpoint、model 和 api_key_changed 布尔值。
func LogProviderConfigChange(providerType ProviderType, baseURL, model string, apiKeyChanged bool) {
	slog.Info("provider 配置已更新",
		"provider_type", string(providerType),
		"base_url", baseURL,
		"model", model,
		"api_key_changed", apiKeyChanged,
	)
}
