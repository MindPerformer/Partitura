// handler_test.go 实现 Provider 管理 API 的 HTTP handler 级别测试。
//
// 引入动机：计划要求真实行为测试——RBAC、CSRF、no-secret GET、审计脱敏。
// 所有测试通过 httptest 调用真实 handler 逻辑验证，不使用反射。
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"partitura/server/internal/auth"
	"partitura/server/internal/crypto"
)

// mockAuditRepoForHandler 是 handler 测试用的审计 mock。
// 引入动机：handler 测试需要验证审计记录不含 API Key。
type mockAuditRepoForHandler struct {
	mu      sync.Mutex
	entries []mockAuditEntryForHandler
}

type mockAuditEntryForHandler struct {
	userID      string
	action      string
	detail      json.RawMessage
}

func newMockAuditRepoForHandler() *mockAuditRepoForHandler {
	return &mockAuditRepoForHandler{}
}

func (m *mockAuditRepoForHandler) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, mockAuditEntryForHandler{
		userID: userID,
		action: action,
		detail: detail,
	})
	return nil
}

func generateTestRootKeyForHandler(t *testing.T) []byte {
	t.Helper()
	raw := make([]byte, crypto.KeyLength)
	for i := range raw {
		raw[i] = byte(i)
	}
	return raw
}

// setupHandler 创建测试用 Handler 和 Registry。
// 引入动机：handler 测试需要完整的 Registry + Repository + Audit 组合。
func setupHandler(t *testing.T) (*Handler, *Registry, *mockRepo, *mockAuditRepoForHandler) {
	t.Helper()
	repo := newMockRepo()
	rootKey := generateTestRootKeyForHandler(t)
	registry := NewRegistry(repo, rootKey)
	auditRepo := newMockAuditRepoForHandler()
	handler := NewHandler(registry, repo, auditRepo)
	return handler, registry, repo, auditRepo
}

func TestHandler_ListProviders_NoSecretInResponse(t *testing.T) {
	handler, _, repo, _ := setupHandler(t)

	// 预填充一条有密文的配置
	encryptedKey, err := crypto.Encrypt(generateTestRootKeyForHandler(t), []byte("sk-secret-key-12345"))
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	embCfg := EmbeddingConfigJSON{
		BaseURL:        "https://api.example.com",
		Model:          "test-model",
		Dimensions:     1024,
		TimeoutSeconds: 30,
		BatchSize:      32,
	}
	cfgJSON, _ := json.Marshal(embCfg)
	repo.setConfig(ProviderTypeEmbedding, encryptedKey, cfgJSON)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers", nil)
	w := httptest.NewRecorder()
	handler.ListProviders(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	body := w.Body.String()

	// 验证响应中不包含明文 API Key
	if strings.Contains(body, "sk-secret-key-12345") {
		t.Fatal("GET 响应不应包含明文 API Key")
	}

	// 验证响应中不包含密文
	if strings.Contains(body, encryptedKey) {
		t.Fatal("GET 响应不应包含密文")
	}

	// 验证响应中不包含 encrypted_key 字段名
	if strings.Contains(body, "encrypted_key") {
		t.Fatal("GET 响应不应包含 encrypted_key 字段名")
	}

	// 验证响应包含 api_key_set 布尔值
	if !strings.Contains(body, "api_key_set") {
		t.Fatal("GET 响应应包含 api_key_set 字段")
	}

	// 验证响应包含 configured 布尔值
	if !strings.Contains(body, "configured") {
		t.Fatal("GET 响应应包含 configured 字段")
	}
}

func TestHandler_GetProvider_NoSecretInResponse(t *testing.T) {
	handler, _, repo, _ := setupHandler(t)

	encryptedKey, _ := crypto.Encrypt(generateTestRootKeyForHandler(t), []byte("sk-secret-key"))
	embCfg := EmbeddingConfigJSON{BaseURL: "https://api.example.com", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON, _ := json.Marshal(embCfg)
	repo.setConfig(ProviderTypeEmbedding, encryptedKey, cfgJSON)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers/embedding", nil)
	req.SetPathValue("type", "embedding")
	w := httptest.NewRecorder()
	handler.GetProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusOK)
	}

	body := w.Body.String()
	if strings.Contains(body, "sk-secret-key") {
		t.Fatal("GET 响应不应包含明文 API Key")
	}
	if strings.Contains(body, encryptedKey) {
		t.Fatal("GET 响应不应包含密文")
	}
}

func TestHandler_GetProvider_NotConfigured(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers/reranker", nil)
	req.SetPathValue("type", "reranker")
	w := httptest.NewRecorder()
	handler.GetProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusOK)
	}

	var dto ProviderConfigDTO
	if err := json.NewDecoder(w.Body).Decode(&dto); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if dto.Configured {
		t.Fatal("未配置的 Provider Configured 应为 false")
	}
	if dto.APIKeySet {
		t.Fatal("未配置的 Provider APIKeySet 应为 false")
	}
}

func TestHandler_PutProvider_AuditNoSecret(t *testing.T) {
	handler, _, _, auditRepo := setupHandler(t)

	// 构造 PUT 请求
	reqBody := `{
		"api_key": "sk-super-secret-key-99999",
		"config": {
			"base_url": "https://api.example.com",
			"model": "test-model",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	// 注入认证身份——模拟 auth middleware 已通过
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	// 模拟认证身份注入 context
	ctx := context.WithValue(req.Context(), "user_id", "test-user-id")
	req = req.WithContext(ctx)

	handler.PutProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// 验证审计记录已写入
	if len(auditRepo.entries) != 1 {
		t.Fatalf("审计记录数 = %d, want 1", len(auditRepo.entries))
	}

	entry := auditRepo.entries[0]
	if entry.action != "admin.provider.update" {
		t.Fatalf("审计 action = %q, want %q", entry.action, "admin.provider.update")
	}

	// 验证审计 detail 不包含 API Key
	detailStr := string(entry.detail)
	if strings.Contains(detailStr, "sk-super-secret-key-99999") {
		t.Fatal("审计 detail 不应包含 API Key 明文")
	}

	// 验证审计 detail 包含必要字段
	var detail map[string]interface{}
	if err := json.Unmarshal(entry.detail, &detail); err != nil {
		t.Fatalf("解析审计 detail 失败: %v", err)
	}
	if detail["provider_type"] != "embedding" {
		t.Fatalf("审计 provider_type = %v, want embedding", detail["provider_type"])
	}
	if detail["api_key_changed"] != true {
		t.Fatal("审计 api_key_changed 应为 true")
	}
	if _, hasKey := detail["api_key"]; hasKey {
		t.Fatal("审计 detail 不应包含 api_key 字段")
	}
	if _, hasKey := detail["encrypted_key"]; hasKey {
		t.Fatal("审计 detail 不应包含 encrypted_key 字段")
	}
}

func TestHandler_PutProvider_EmptyAPIKey(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	reqBody := `{
		"api_key": "",
		"config": {
			"base_url": "https://api.example.com",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 api_key 状态码 = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandler_PutProvider_InvalidProviderType(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	reqBody := `{"api_key":"sk-test","config":{"base_url":"https://api.example.com","model":"test","dimensions":1024,"timeout_seconds":30,"batch_size":32,"query_instruction":"","document_instruction":""}}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/invalid", strings.NewReader(reqBody))
	req.SetPathValue("type", "invalid")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 provider type 状态码 = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandler_PutProvider_StrictJSON(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	// 包含未知字段的请求体应被拒绝
	reqBody := `{
		"api_key": "sk-test",
		"config": {
			"base_url": "https://api.example.com",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		},
		"unknown_field": "should-be-rejected"
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("包含未知字段的请求体应被拒绝, 状态码 = %d, want %d, body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandler_PutProvider_HotReload(t *testing.T) {
	handler, registry, repo, _ := setupHandler(t)

	// 初始状态 Provider 为 nil
	if registry.GetEmbeddingProvider() != nil {
		t.Fatal("初始状态 Embedding Provider 应为 nil")
	}

	reqBody := `{
		"api_key": "sk-test-hot-reload",
		"config": {
			"base_url": "https://api.example.com",
			"model": "test-model",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// 验证 Provider 已热更新
	if registry.GetEmbeddingProvider() == nil {
		t.Fatal("PUT 后 Embedding Provider 不应为 nil（热更新）")
	}

	// 验证数据库中有密文记录
	cfg, err := repo.Get(context.Background(), ProviderTypeEmbedding)
	if err != nil {
		t.Fatalf("查询配置失败: %v", err)
	}
	if cfg.EncryptedKey == "" {
		t.Fatal("数据库中密文不应为空")
	}
	if cfg.EncryptedKey == "sk-test-hot-reload" {
		t.Fatal("数据库中密文不应等于明文")
	}
}

func TestHandler_TestProvider_NoPersistence(t *testing.T) {
	handler, _, repo, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test-temp",
		"config": {
			"base_url": "https://api.example.com",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers/embedding/test", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.TestProvider(w, req)

	// 测试请求可能因网络不通而返回 ok/failed/unavailable，但不持久化
	// 验证数据库中没有记录
	configs, _ := repo.List(context.Background())
	if len(configs) != 0 {
		t.Fatalf("测试请求不应持久化, got %d configs", len(configs))
	}
}

func TestHandler_TestProvider_EmptyAPIKey(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	reqBody := `{"api_key":"","config":{"base_url":"https://api.example.com","model":"test","dimensions":1024,"timeout_seconds":30,"batch_size":32,"query_instruction":"","document_instruction":""}}`

	req := httptest.NewRequest(http.MethodPost, "/api/admin/providers/embedding/test", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.TestProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("空 api_key 状态码 = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandler_PutReranker_Success(t *testing.T) {
	handler, registry, _, auditRepo := setupHandler(t)

	reqBody := `{
		"api_key": "sk-reranker-key",
		"config": {
			"base_url": "https://api.example.com",
			"model": "reranker-model",
			"timeout_seconds": 10,
			"max_candidates": 20
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/reranker", strings.NewReader(reqBody))
	req.SetPathValue("type", "reranker")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// 验证 Reranker Provider 已热更新
	if registry.GetRerankerProvider() == nil {
		t.Fatal("PUT 后 Reranker Provider 不应为 nil")
	}

	// 验证审计记录
	if len(auditRepo.entries) != 1 {
		t.Fatalf("审计记录数 = %d, want 1", len(auditRepo.entries))
	}

	// 验证审计不含 API Key
	detailStr := string(auditRepo.entries[0].detail)
	if strings.Contains(detailStr, "sk-reranker-key") {
		t.Fatal("审计 detail 不应包含 API Key")
	}
}

// TestHandler_GetProvider_RepoError_Returns500 验证当 repository 返回非 ErrNoRows 错误时，
// handler 返回 500 而非伪装成未配置的 200。
// 引入动机：fail-fast 原则要求所有 repository 错误必须可观察，不能静默降级。
func TestHandler_GetProvider_RepoError_Returns500(t *testing.T) {
	handler, _, repo, _ := setupHandler(t)

	// 注入一个非 ErrNoRows 的 repository 错误
	repo.getErr = errors.New("数据库连接断开")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers/embedding", nil)
	req.SetPathValue("type", "embedding")
	w := httptest.NewRecorder()
	handler.GetProvider(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("repo 错误时状态码 = %d, want %d, body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}

	// 验证响应不是未配置的 200——不应包含 configured:false
	body := w.Body.String()
	if strings.Contains(body, `"configured":false`) {
		t.Fatal("repo 错误不应伪装成未配置的 200 响应")
	}

	// 验证响应是标准错误格式
	var errResp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if _, hasError := errResp["error"]; !hasError {
		t.Fatal("500 响应应包含 error 字段")
	}
}

// TestHandler_GetProvider_RepoError_NoSecretLeak 验证 repo 错误的 500 响应不泄露 secret。
// 引入动机：错误响应中不得包含 API Key、密文等敏感信息。
func TestHandler_GetProvider_RepoError_NoSecretLeak(t *testing.T) {
	handler, _, repo, _ := setupHandler(t)

	// 预填充一条有密文的配置，确保即使 repo 出错也不泄漏
	encryptedKey, _ := crypto.Encrypt(generateTestRootKeyForHandler(t), []byte("sk-leak-test-key"))
	embCfg := EmbeddingConfigJSON{BaseURL: "https://api.example.com", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON, _ := json.Marshal(embCfg)
	repo.setConfig(ProviderTypeEmbedding, encryptedKey, cfgJSON)

	// 注入错误使 Get 返回非 ErrNoRows 错误
	repo.getErr = errors.New("连接池耗尽")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers/embedding", nil)
	req.SetPathValue("type", "embedding")
	w := httptest.NewRecorder()
	handler.GetProvider(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want %d", w.Code, http.StatusInternalServerError)
	}

	body := w.Body.String()
	if strings.Contains(body, "sk-leak-test-key") {
		t.Fatal("500 错误响应不应包含明文 API Key")
	}
	if strings.Contains(body, encryptedKey) {
		t.Fatal("500 错误响应不应包含密文")
	}
}

// TestHandler_ListProviders_RepoError_Returns500 验证 ListProviders 在 repository 错误时返回 500。
// 引入动机：ListProviders 不应有 io.EOF 豁免，所有 repository 错误必须返回 500。
func TestHandler_ListProviders_RepoError_Returns500(t *testing.T) {
	handler, _, repo, _ := setupHandler(t)

	// 注入一个 repository 错误
	repo.listErr = errors.New("数据库不可用")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/providers", nil)
	w := httptest.NewRecorder()
	handler.ListProviders(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("repo 错误时状态码 = %d, want %d, body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}

	// 验证响应是标准错误格式
	var errResp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if _, hasError := errResp["error"]; !hasError {
		t.Fatal("500 响应应包含 error 字段")
	}
}

// TestHandler_PutProvider_NormalizesBaseURL 验证 PUT 请求规范化 base URL 尾部斜杠。
// 引入动机：不同服务商的 base URL 可能带或不带尾部斜杠，
// handler 必须统一去除尾部斜杠以确保 endpoint 拼接正确。
func TestHandler_PutProvider_NormalizesBaseURL(t *testing.T) {
	handler, _, repo, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test-normalize",
		"config": {
			"base_url": "https://ai.gitee.com/v1/",
			"model": "test-model",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	// 验证数据库中保存的 base_url 不含尾部斜杠
	cfg, err := repo.Get(context.Background(), ProviderTypeEmbedding)
	if err != nil {
		t.Fatalf("查询配置失败: %v", err)
	}
	var savedCfg EmbeddingConfigJSON
	if err := json.Unmarshal(cfg.ConfigJSON, &savedCfg); err != nil {
		t.Fatalf("解析保存的配置失败: %v", err)
	}
	if savedCfg.BaseURL != "https://ai.gitee.com/v1" {
		t.Fatalf("保存的 base_url = %q, want %q (尾部斜杠应被去除)", savedCfg.BaseURL, "https://ai.gitee.com/v1")
	}
}

// TestHandler_PutProvider_AcceptsV1Path 验证 PUT 请求接受含 /v1 路径的 base URL。
// 引入动机：用户明确要求不同服务商的 Base URL 可用不同格式，
// 如 https://ai.gitee.com/v1 必须被接受。
func TestHandler_PutProvider_AcceptsV1Path(t *testing.T) {
	handler, registry, _, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test-v1",
		"config": {
			"base_url": "https://ai.gitee.com/v1",
			"model": "Qwen3-Embedding-8B",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("含 /v1 路径的 base_url 应被接受, 状态码 = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	if registry.GetEmbeddingProvider() == nil {
		t.Fatal("PUT 后 Embedding Provider 不应为 nil")
	}
}

// TestHandler_PutProvider_RejectsUserinfoInURL 验证 PUT 请求拒绝含 userinfo 的 base URL。
func TestHandler_PutProvider_RejectsUserinfoInURL(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test",
		"config": {
			"base_url": "https://user:pass@ai.gitee.com/v1",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("含 userinfo 的 base_url 应被拒绝, 状态码 = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandler_PutProvider_RejectsQueryInURL 验证 PUT 请求拒绝含 query 参数的 base URL。
func TestHandler_PutProvider_RejectsQueryInURL(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test",
		"config": {
			"base_url": "https://ai.gitee.com/v1?q=1",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("含 query 的 base_url 应被拒绝, 状态码 = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandler_PutProvider_RejectsPathTraversal 验证 PUT 请求拒绝含路径遍历的 base URL。
func TestHandler_PutProvider_RejectsPathTraversal(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test",
		"config": {
			"base_url": "https://ai.gitee.com/../etc/passwd",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("含路径遍历的 base_url 应被拒绝, 状态码 = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandler_PutProvider_RejectsNonHTTPScheme 验证 PUT 请求拒绝非 HTTP(S) scheme。
func TestHandler_PutProvider_RejectsNonHTTPScheme(t *testing.T) {
	handler, _, _, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test",
		"config": {
			"base_url": "ftp://ai.gitee.com/v1",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("非 HTTP(S) scheme 的 base_url 应被拒绝, 状态码 = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// TestHandler_PutProvider_AcceptsMultiLevelPath 验证 PUT 请求接受多层 API 路径。
func TestHandler_PutProvider_AcceptsMultiLevelPath(t *testing.T) {
	handler, registry, _, _ := setupHandler(t)

	reqBody := `{
		"api_key": "sk-test-multi",
		"config": {
			"base_url": "https://api.example.com/api/v2",
			"model": "test",
			"dimensions": 1024,
			"timeout_seconds": 30,
			"batch_size": 32,
			"query_instruction": "",
			"document_instruction": ""
		}
	}`

	req := httptest.NewRequest(http.MethodPut, "/api/admin/providers/embedding", strings.NewReader(reqBody))
	req.SetPathValue("type", "embedding")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "test-user-id", Username: "admin"}))
	w := httptest.NewRecorder()

	handler.PutProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("多层 API 路径应被接受, 状态码 = %d, want %d, body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	if registry.GetEmbeddingProvider() == nil {
		t.Fatal("PUT 后 Embedding Provider 不应为 nil")
	}
}
