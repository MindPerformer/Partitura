// handler.go 实现管理员 Provider Web API handler。
//
// 引入动机：计划要求 system_admin 通过受 CSRF 保护的 Web API 管理 Provider 配置：
//   - GET /api/admin/providers：返回所有 Provider 状态和非敏感配置
//   - GET /api/admin/providers/{type}：返回单个 Provider 状态和非敏感配置
//   - PUT /api/admin/providers/{type}：CSRF + RBAC，加密写入，审计脱敏
//   - POST /api/admin/providers/{type}/test：CSRF + RBAC，仅临时测试，不持久化
//
// 安全契约：
//   - GET 响应绝不包含 API Key 明文或密文
//   - PUT 请求的 api_key 字段不进入审计日志、不进入 GET 响应、不进入缓存
//   - POST test 不持久化任何数据
//   - 审计仅记录类型、endpoint、model 和 api_key_changed 布尔值
//   - 所有错误可观察，不静默吞错
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"database/sql"

	"partitura/server/internal/auth"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/search/embedding"
	"partitura/server/internal/search/reranker"
	types "partitura/server/internal/search/types"
	"partitura/server/internal/workspace"
)

// AuditRepository 定义审计记录接口。
// 引入动机：Provider 配置变更需要记录审计日志，但审计中不得包含 API Key。
type AuditRepository interface {
	Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error
}

// Handler 是管理员 Provider 配置 HTTP handler。
// 引入动机：将 Provider 管理 API 的 HTTP 解析、校验、持久化和响应集中在此 handler。
type Handler struct {
	registry   *Registry
	repo       Repository
	auditRepo  AuditRepository
}

// NewHandler 创建 Provider 管理 handler。
// 引入动机：main.go 注入 Registry、Repository 和审计仓储。
func NewHandler(registry *Registry, repo Repository, auditRepo AuditRepository) *Handler {
	return &Handler{
		registry:  registry,
		repo:      repo,
		auditRepo: auditRepo,
	}
}

// putProviderRequest 是 PUT /api/admin/providers/{type} 的请求体。
// 引入动机：严格 JSON 解码，api_key 仅用于写入，不进入 GET 响应或审计。
type putProviderRequest struct {
	APIKey string          `json:"api_key"`
	Config json.RawMessage `json:"config"`
}

// testProviderRequest 是 POST /api/admin/providers/{type}/test 的请求体。
// 引入动机：测试请求包含临时凭据，不持久化。
type testProviderRequest struct {
	APIKey string          `json:"api_key"`
	Config json.RawMessage `json:"config"`
}

// testProviderResponse 是 POST test 的响应体。
type testProviderResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ListProviders 处理 GET /api/admin/providers。
// 引入动机：返回所有 Provider 状态和非敏感配置，绝不返回 API Key。
func (h *Handler) ListProviders(w http.ResponseWriter, r *http.Request) {
	configs, err := h.repo.List(r.Context())
	if err != nil {
		slog.Error("列出 Provider 配置失败", "error", err)
		writeProviderError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	dtoMap := map[string]ProviderConfigDTO{
		"embedding": {Configured: false, APIKeySet: false},
		"reranker":  {Configured: false, APIKeySet: false},
	}

	for _, cfg := range configs {
		dto := cfg.ToDTO()
		dtoMap[string(cfg.ProviderType)] = dto
	}

	writeProviderJSON(w, http.StatusOK, map[string]interface{}{
		"providers": dtoMap,
	})
}

// GetProvider 处理 GET /api/admin/providers/{type}。
// 引入动机：返回单个 Provider 状态和非敏感配置，绝不返回 API Key。
func (h *Handler) GetProvider(w http.ResponseWriter, r *http.Request) {
	providerTypeStr := r.PathValue("type")
	pt, ok := ParseProviderType(providerTypeStr)
	if !ok {
		writeProviderError(w, http.StatusBadRequest, "未知 Provider 类型")
		return
	}

	cfg, err := h.repo.Get(r.Context(), pt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProviderJSON(w, http.StatusOK, ProviderConfigDTO{
				Configured: false,
				APIKeySet:  false,
			})
			return
		}
		slog.Error("查询 Provider 配置失败", "error", err, "provider_type", string(pt))
		writeProviderError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	writeProviderJSON(w, http.StatusOK, cfg.ToDTO())
}

// PutProvider 处理 PUT /api/admin/providers/{type}。
// 引入动机：CSRF + RBAC 保护，加密写入 Provider 配置，审计脱敏。
func (h *Handler) PutProvider(w http.ResponseWriter, r *http.Request) {
	identity := auth.IdentityFromContext(r.Context())
	if identity == nil {
		writeProviderError(w, http.StatusUnauthorized, "未认证")
		return
	}

	providerTypeStr := r.PathValue("type")
	pt, ok := ParseProviderType(providerTypeStr)
	if !ok {
		writeProviderError(w, http.StatusBadRequest, "未知 Provider 类型")
		return
	}

	var req putProviderRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeProviderError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	if req.APIKey == "" {
		writeProviderError(w, http.StatusBadRequest, "api_key 不能为空")
		return
	}

	if len(req.Config) == 0 {
		writeProviderError(w, http.StatusBadRequest, "config 不能为空")
		return
	}

	// 严格校验配置
	var baseURL, model string
	switch pt {
	case ProviderTypeEmbedding:
		var embCfg EmbeddingConfigJSON
		if err := json.Unmarshal(req.Config, &embCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, fmt.Sprintf("config 解析失败: %v", err))
			return
		}
		if err := ValidateEmbeddingConfig(embCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 规范化 base URL：统一去除尾部斜杠
		embCfg.BaseURL = NormalizeBaseURL(embCfg.BaseURL)
		baseURL = embCfg.BaseURL
		model = embCfg.Model
		// 重新序列化规范化后的配置
		req.Config, _ = json.Marshal(embCfg)
	case ProviderTypeReranker:
		var rrCfg RerankerConfigJSON
		if err := json.Unmarshal(req.Config, &rrCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, fmt.Sprintf("config 解析失败: %v", err))
			return
		}
		if err := ValidateRerankerConfig(rrCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 规范化 base URL：统一去除尾部斜杠
		rrCfg.BaseURL = NormalizeBaseURL(rrCfg.BaseURL)
		baseURL = rrCfg.BaseURL
		model = rrCfg.Model
		// 重新序列化规范化后的配置
		req.Config, _ = json.Marshal(rrCfg)
	}

	// 保存并热加载
	if err := h.registry.SaveAndReload(r.Context(), pt, req.APIKey, req.Config, identity.UserID); err != nil {
		slog.Error("保存 Provider 配置失败", "error", err, "provider_type", string(pt))
		writeProviderError(w, http.StatusInternalServerError, fmt.Sprintf("保存失败: %v", err))
		return
	}

	// 记录审计：仅类型、endpoint、model 和 api_key_changed，不含 key
	h.recordProviderAudit(r, identity.UserID, pt, baseURL, model, true)

	// 记录结构化日志：同样不含 key
	LogProviderConfigChange(pt, baseURL, model, true)

	writeProviderJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "saved",
		"message": "Provider 配置已保存并热加载",
	})
}

// TestProvider 处理 POST /api/admin/providers/{type}/test。
// 引入动机：仅临时测试 Provider 连通性，不持久化。
func (h *Handler) TestProvider(w http.ResponseWriter, r *http.Request) {
	identity := auth.IdentityFromContext(r.Context())
	if identity == nil {
		writeProviderError(w, http.StatusUnauthorized, "未认证")
		return
	}

	providerTypeStr := r.PathValue("type")
	pt, ok := ParseProviderType(providerTypeStr)
	if !ok {
		writeProviderError(w, http.StatusBadRequest, "未知 Provider 类型")
		return
	}

	var req testProviderRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeProviderError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	if req.APIKey == "" {
		writeProviderError(w, http.StatusBadRequest, "api_key 不能为空")
		return
	}

	if len(req.Config) == 0 {
		writeProviderError(w, http.StatusBadRequest, "config 不能为空")
		return
	}

	// 严格校验配置
	switch pt {
	case ProviderTypeEmbedding:
		var embCfg EmbeddingConfigJSON
		if err := json.Unmarshal(req.Config, &embCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, fmt.Sprintf("config 解析失败: %v", err))
			return
		}
		if err := ValidateEmbeddingConfig(embCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 规范化 base URL
		embCfg.BaseURL = NormalizeBaseURL(embCfg.BaseURL)
		req.Config, _ = json.Marshal(embCfg)
	case ProviderTypeReranker:
		var rrCfg RerankerConfigJSON
		if err := json.Unmarshal(req.Config, &rrCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, fmt.Sprintf("config 解析失败: %v", err))
			return
		}
		if err := ValidateRerankerConfig(rrCfg); err != nil {
			writeProviderError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 规范化 base URL
		rrCfg.BaseURL = NormalizeBaseURL(rrCfg.BaseURL)
		req.Config, _ = json.Marshal(rrCfg)
	}

	// 构造临时 Provider 实例
	testProvider, err := h.registry.CreateTestProvider(pt, req.APIKey, req.Config)
	if err != nil {
		writeProviderError(w, http.StatusInternalServerError, fmt.Sprintf("构造测试 Provider 失败: %v", err))
		return
	}

	// 执行连通性测试
	ctx := r.Context()
	switch pt {
	case ProviderTypeEmbedding:
		embProvider, ok := testProvider.(embedding.Provider)
		if !ok {
			writeProviderError(w, http.StatusInternalServerError, "内部错误：Provider 类型不匹配")
			return
		}
		if !embProvider.Available(ctx) {
			writeProviderJSON(w, http.StatusOK, testProviderResponse{
				Status:  "unavailable",
				Message: "Embedding Provider 不可用（配置不完整）",
			})
			return
		}
		// 发送最小 embedding 请求验证连通性
		_, err := embProvider.Embed(ctx, []string{"test"})
		if err != nil {
			slog.Info("Provider 测试失败", "provider_type", string(pt), "error", err)
			writeProviderJSON(w, http.StatusOK, testProviderResponse{
				Status:  "failed",
				Message: fmt.Sprintf("连通性测试失败: %v", err),
			})
			return
		}
		writeProviderJSON(w, http.StatusOK, testProviderResponse{
			Status:  "ok",
			Message: "Embedding Provider 连通性测试成功",
		})

	case ProviderTypeReranker:
		rrProvider, ok := testProvider.(reranker.Provider)
		if !ok {
			writeProviderError(w, http.StatusInternalServerError, "内部错误：Provider 类型不匹配")
			return
		}
		if !rrProvider.Available(ctx) {
			writeProviderJSON(w, http.StatusOK, testProviderResponse{
				Status:  "unavailable",
				Message: "Reranker Provider 不可用（配置不完整）",
			})
			return
		}
		// Reranker 测试：发送一个最小请求
		_, err := rrProvider.Rerank(ctx, "test", []types.RerankerCandidate{{Text: "test", Index: 0}})
		if err != nil {
			slog.Info("Provider 测试失败", "provider_type", string(pt), "error", err)
			writeProviderJSON(w, http.StatusOK, testProviderResponse{
				Status:  "failed",
				Message: fmt.Sprintf("连通性测试失败: %v", err),
			})
			return
		}
		writeProviderJSON(w, http.StatusOK, testProviderResponse{
			Status:  "ok",
			Message: "Reranker Provider 连通性测试成功",
		})
	}
}

// recordProviderAudit 记录 Provider 配置变更审计。
// 引入动机：审计仅记录类型、endpoint、model 和 api_key_changed 布尔值，
// 禁止包含 key、密文、请求 body、Authorization 或 token。
func (h *Handler) recordProviderAudit(r *http.Request, userID string, pt ProviderType, baseURL, model string, apiKeyChanged bool) {
	if h.auditRepo == nil {
		return
	}
	detail := map[string]interface{}{
		"provider_type":    string(pt),
		"base_url":         baseURL,
		"model":            model,
		"api_key_changed":  apiKeyChanged,
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		slog.Error("序列化 Provider 审计 detail 失败", "error", err, "provider_type", string(pt))
		return
	}
	requestID := httpmw.RequestIDFromContext(r.Context())
	// resource_id 为 NULL——provider type 不是 UUID，审计通过 resource_type 和 detail 标识
	if err := h.auditRepo.Record(r.Context(), userID, "", "admin.provider.update", "provider", "", detailJSON, requestID); err != nil {
		slog.Error("写入 Provider 审计日志失败", "error", err, "provider_type", string(pt))
	}
}

// RegisterRoutes 注册 Provider 管理 API 路由。
//
// 引入动机：main.go 调用此函数完成 Provider 管理路由注册。
// 所有端点仅 system_admin 可访问。
//
// 路由清单：
//   - GET  /api/admin/providers — system_admin，所有 Provider 状态
//   - GET  /api/admin/providers/{type} — system_admin，单个 Provider 状态
//   - PUT  /api/admin/providers/{type} — system_admin + CSRF，加密写入
//   - POST /api/admin/providers/{type}/test — system_admin + CSRF，临时测试
func RegisterRoutes(
	mux *http.ServeMux,
	handler *Handler,
	authRepo auth.Repository,
	authCfg auth.AuthConfig,
) {
	// GET /api/admin/providers — system_admin
	mux.Handle("GET /api/admin/providers",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListProviders),
				),
			),
		),
	)

	// GET /api/admin/providers/{type} — system_admin
	mux.Handle("GET /api/admin/providers/{type}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.GetProvider),
				),
			),
		),
	)

	// PUT /api/admin/providers/{type} — system_admin + CSRF
	mux.Handle("PUT /api/admin/providers/{type}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.PutProvider),
					),
				),
			),
		),
	)

	// POST /api/admin/providers/{type}/test — system_admin + CSRF
	mux.Handle("POST /api/admin/providers/{type}/test",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.TestProvider),
					),
				),
			),
		),
	)
}

// writeProviderError 写入统一 JSON 错误响应。
func writeProviderError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		slog.Error("写入错误响应失败", "error", err)
	}
}

// writeProviderJSON 写入 JSON 响应。
func writeProviderJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("序列化 JSON 响应失败", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"内部错误"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// decodeJSONStrict 严格解码 JSON 请求体。
func decodeJSONStrict(r *http.Request, dst interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体包含多个 JSON 值")
	}
	return nil
}
