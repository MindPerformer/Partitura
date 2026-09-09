// Package provider 的测试覆盖 Registry 热更新、fail-fast、Repository 逻辑、
// base_url 校验、DTO 脱敏。
//
// 引入动机：计划要求真实行为测试——Registry 原子热更新、Provider 不可用与 fail-fast、
// secret GET 脱敏、审计脱敏。
package provider

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"partitura/server/internal/crypto"
)

// generateTestRootKey 生成一个合法的 32 字节根密钥。
func generateTestRootKey(t *testing.T) []byte {
	t.Helper()
	raw := make([]byte, crypto.KeyLength)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}
	return raw
}

// mockRepo 是 Repository 接口的内存 mock。
type mockRepo struct {
	mu        sync.Mutex
	configs   map[ProviderType]*ProviderConfig
	upsertErr error
	getErr    error
	listErr   error
}

func newMockRepo() *mockRepo {
	return &mockRepo{configs: make(map[ProviderType]*ProviderConfig)}
}

func (m *mockRepo) Get(ctx context.Context, pt ProviderType) (*ProviderConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	cfg, ok := m.configs[pt]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return cfg, nil
}

func (m *mockRepo) Upsert(ctx context.Context, pt ProviderType, encryptedKey string, configJSON json.RawMessage, updatedBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upsertErr != nil {
		return m.upsertErr
	}
	m.configs[pt] = &ProviderConfig{
		ProviderType:  pt,
		EncryptedKey:  encryptedKey,
		ConfigJSON:    configJSON,
		UpdatedBy:     updatedBy,
		UpdatedAt:     time.Now(),
	}
	return nil
}

func (m *mockRepo) List(ctx context.Context) ([]ProviderConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	var result []ProviderConfig
	for _, cfg := range m.configs {
		result = append(result, *cfg)
	}
	return result, nil
}

func (m *mockRepo) setConfig(pt ProviderType, encryptedKey string, configJSON json.RawMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[pt] = &ProviderConfig{
		ProviderType:  pt,
		EncryptedKey:  encryptedKey,
		ConfigJSON:    configJSON,
		UpdatedAt:     time.Now(),
	}
}

func TestRegistry_LoadFromDB_NoConfigs(t *testing.T) {
	repo := newMockRepo()
	rootKey := generateTestRootKey(t)
	registry := NewRegistry(repo, rootKey)

	if err := registry.LoadFromDB(context.Background()); err != nil {
		t.Fatalf("无配置时应成功: %v", err)
	}

	if registry.GetEmbeddingProvider() != nil {
		t.Fatal("无配置时 Embedding Provider 应为 nil")
	}
	if registry.GetRerankerProvider() != nil {
		t.Fatal("无配置时 Reranker Provider 应为 nil")
	}
}

func TestRegistry_LoadFromDB_WithConfigs(t *testing.T) {
	repo := newMockRepo()
	rootKey := generateTestRootKey(t)

	// 加密一个 API Key
	encryptedKey, err := crypto.Encrypt(rootKey, []byte("sk-test-key"))
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

	registry := NewRegistry(repo, rootKey)
	if err := registry.LoadFromDB(context.Background()); err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if registry.GetEmbeddingProvider() == nil {
		t.Fatal("有配置时 Embedding Provider 不应为 nil")
	}
}

func TestRegistry_LoadFromDB_CiphertextButNoRootKey(t *testing.T) {
	repo := newMockRepo()

	encryptedKey, _ := crypto.Encrypt(generateTestRootKey(t), []byte("sk-test"))
	embCfg := EmbeddingConfigJSON{BaseURL: "https://api.example.com", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON, _ := json.Marshal(embCfg)
	repo.setConfig(ProviderTypeEmbedding, encryptedKey, cfgJSON)

	// 根密钥为 nil
	registry := NewRegistry(repo, nil)
	err := registry.LoadFromDB(context.Background())
	if err == nil {
		t.Fatal("密文存在但根密钥缺失应 fail-fast")
	}
}

func TestRegistry_LoadFromDB_DecryptionFailure(t *testing.T) {
	repo := newMockRepo()
	rootKey := generateTestRootKey(t)

	// 使用不同的密钥加密
	otherKey := generateTestRootKey(t)
	encryptedKey, _ := crypto.Encrypt(otherKey, []byte("sk-test"))
	embCfg := EmbeddingConfigJSON{BaseURL: "https://api.example.com", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON, _ := json.Marshal(embCfg)
	repo.setConfig(ProviderTypeEmbedding, encryptedKey, cfgJSON)

	registry := NewRegistry(repo, rootKey)
	err := registry.LoadFromDB(context.Background())
	if err == nil {
		t.Fatal("解密失败应返回错误")
	}
}

func TestRegistry_SaveAndReload(t *testing.T) {
	repo := newMockRepo()
	rootKey := generateTestRootKey(t)
	registry := NewRegistry(repo, rootKey)

	embCfg := EmbeddingConfigJSON{
		BaseURL:        "https://api.example.com",
		Model:          "test-model",
		Dimensions:     1024,
		TimeoutSeconds: 30,
		BatchSize:      32,
	}
	cfgJSON, _ := json.Marshal(embCfg)

	if err := registry.SaveAndReload(context.Background(), ProviderTypeEmbedding, "sk-new-key", cfgJSON, "user-1"); err != nil {
		t.Fatalf("SaveAndReload 失败: %v", err)
	}

	// 验证 Provider 已热更新
	if registry.GetEmbeddingProvider() == nil {
		t.Fatal("SaveAndReload 后 Provider 不应为 nil")
	}

	// 验证数据库中有密文
	cfg, err := repo.Get(context.Background(), ProviderTypeEmbedding)
	if err != nil {
		t.Fatalf("查询配置失败: %v", err)
	}
	if cfg.EncryptedKey == "" {
		t.Fatal("密文不应为空")
	}
	if cfg.EncryptedKey == "sk-new-key" {
		t.Fatal("密文不应等于明文")
	}
}

func TestRegistry_SaveAndReload_NoRootKey(t *testing.T) {
	repo := newMockRepo()
	registry := NewRegistry(repo, nil)

	embCfg := EmbeddingConfigJSON{BaseURL: "https://api.example.com", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON, _ := json.Marshal(embCfg)

	err := registry.SaveAndReload(context.Background(), ProviderTypeEmbedding, "sk-key", cfgJSON, "user-1")
	if err == nil {
		t.Fatal("无根密钥时 SaveAndReload 应失败")
	}
}

func TestRegistry_HotReload_ReplacesProvider(t *testing.T) {
	repo := newMockRepo()
	rootKey := generateTestRootKey(t)
	registry := NewRegistry(repo, rootKey)

	// 第一次保存
	embCfg1 := EmbeddingConfigJSON{BaseURL: "https://api1.example.com", Model: "model-v1", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON1, _ := json.Marshal(embCfg1)
	_ = registry.SaveAndReload(context.Background(), ProviderTypeEmbedding, "sk-key-1", cfgJSON1, "user-1")
	provider1 := registry.GetEmbeddingProvider()

	// 第二次保存（热更新）
	embCfg2 := EmbeddingConfigJSON{BaseURL: "https://api2.example.com", Model: "model-v2", Dimensions: 2048, TimeoutSeconds: 60, BatchSize: 64}
	cfgJSON2, _ := json.Marshal(embCfg2)
	_ = registry.SaveAndReload(context.Background(), ProviderTypeEmbedding, "sk-key-2", cfgJSON2, "user-1")
	provider2 := registry.GetEmbeddingProvider()

	// 验证 Provider 实例已被替换
	if provider1 == provider2 {
		t.Fatal("热更新后 Provider 实例应被替换")
	}
}

func TestRegistry_CreateTestProvider(t *testing.T) {
	repo := newMockRepo()
	rootKey := generateTestRootKey(t)
	registry := NewRegistry(repo, rootKey)

	embCfg := EmbeddingConfigJSON{BaseURL: "https://api.example.com", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON, _ := json.Marshal(embCfg)

	// CreateTestProvider 不应持久化
	_, err := registry.CreateTestProvider(ProviderTypeEmbedding, "sk-test", cfgJSON)
	if err != nil {
		t.Fatalf("CreateTestProvider 失败: %v", err)
	}

	// 验证数据库中没有记录
	configs, _ := repo.List(context.Background())
	if len(configs) != 0 {
		t.Fatalf("CreateTestProvider 不应持久化, got %d configs", len(configs))
	}
}

func TestRegistry_HasRootKey(t *testing.T) {
	rootKey := generateTestRootKey(t)

	r1 := NewRegistry(newMockRepo(), rootKey)
	if !r1.HasRootKey() {
		t.Fatal("有根密钥时 HasRootKey 应返回 true")
	}

	r2 := NewRegistry(newMockRepo(), nil)
	if r2.HasRootKey() {
		t.Fatal("无根密钥时 HasRootKey 应返回 false")
	}
}

func TestCheckExistingSecrets(t *testing.T) {
	repo := newMockRepo()
	rootKey := generateTestRootKey(t)

	// 无密文
	has, err := CheckExistingSecrets(context.Background(), repo)
	if err != nil {
		t.Fatalf("CheckExistingSecrets 失败: %v", err)
	}
	if has {
		t.Fatal("无密文时应返回 false")
	}

	// 有密文
	encryptedKey, _ := crypto.Encrypt(rootKey, []byte("sk-test"))
	embCfg := EmbeddingConfigJSON{BaseURL: "https://api.example.com", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	cfgJSON, _ := json.Marshal(embCfg)
	repo.setConfig(ProviderTypeEmbedding, encryptedKey, cfgJSON)

	has, err = CheckExistingSecrets(context.Background(), repo)
	if err != nil {
		t.Fatalf("CheckExistingSecrets 失败: %v", err)
	}
	if !has {
		t.Fatal("有密文时应返回 true")
	}
}

func TestValidateBaseURL(t *testing.T) {
	tests := []struct {
		url     string
		wantErr bool
	}{
		// 合法：origin 和带 API base path 的绝对 URL
		{"https://api.example.com", false},
		{"http://localhost:8080", false},
		{"https://ai.gitee.com/v1", false},
		{"https://api.example.com/api/v2", false},
		{"https://api.example.com/", false},
		{"http://localhost:8080/v1", false},
		{"https://api.example.com/v1/embeddings", false},
		// 非法：空、非 URL、非 HTTP(S)
		{"", true},
		{"not-a-url", true},
		{"ftp://api.example.com", true},
		// 非法：userinfo
		{"https://user:pass@api.example.com", true},
		{"https://user@api.example.com/v1", true},
		// 非法：query 和 fragment
		{"https://api.example.com?q=1", true},
		{"https://api.example.com#frag", true},
		{"https://api.example.com/v1?q=1", true},
		{"https://api.example.com/v1#frag", true},
		// 非法：路径遍历
		{"https://api.example.com/../etc/passwd", true},
		{"https://api.example.com/v1/../../etc", true},
		{"https://api.example.com/..", true},
		// 非法：控制字符
		{"https://api.example.com\n/v1", true},
		{"https://api.example.com\r/v1", true},
		{"https://api.example.com\t/v1", true},
		// 非法：无 host
		{"https://", true},
		{"https:///v1", true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			err := ValidateBaseURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateBaseURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://api.example.com", "https://api.example.com"},
		{"https://api.example.com/", "https://api.example.com"},
		{"https://ai.gitee.com/v1/", "https://ai.gitee.com/v1"},
		{"https://ai.gitee.com/v1", "https://ai.gitee.com/v1"},
		{"  https://api.example.com/v1/  ", "https://api.example.com/v1"},
		{"https://api.example.com//v1//", "https://api.example.com//v1"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := NormalizeBaseURL(tt.input)
			if got != tt.want {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateEmbeddingConfig(t *testing.T) {
	valid := EmbeddingConfigJSON{BaseURL: "https://ai.gitee.com/v1", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(valid); err != nil {
		t.Fatalf("合法配置（含 /v1 路径）应通过校验: %v", err)
	}

	validMultiPath := EmbeddingConfigJSON{BaseURL: "https://api.example.com/api/v2", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(validMultiPath); err != nil {
		t.Fatalf("合法配置（含多层路径）应通过校验: %v", err)
	}

	invalid := EmbeddingConfigJSON{BaseURL: "", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(invalid); err == nil {
		t.Fatal("空 base_url 应校验失败")
	}

	invalidDim := EmbeddingConfigJSON{BaseURL: "https://api.example.com/v1", Model: "test", Dimensions: 0, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(invalidDim); err == nil {
		t.Fatal("维度 0 应校验失败")
	}

	// 非法 URL：包含 userinfo
	invalidUserinfo := EmbeddingConfigJSON{BaseURL: "https://user:pass@api.example.com/v1", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(invalidUserinfo); err == nil {
		t.Fatal("包含 userinfo 的 base_url 应校验失败")
	}

	// 非法 URL：包含 query
	invalidQuery := EmbeddingConfigJSON{BaseURL: "https://api.example.com/v1?q=1", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(invalidQuery); err == nil {
		t.Fatal("包含 query 的 base_url 应校验失败")
	}

	// 非法 URL：包含 fragment
	invalidFragment := EmbeddingConfigJSON{BaseURL: "https://api.example.com/v1#frag", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(invalidFragment); err == nil {
		t.Fatal("包含 fragment 的 base_url 应校验失败")
	}

	// 非法 URL：路径遍历
	invalidTraversal := EmbeddingConfigJSON{BaseURL: "https://api.example.com/../etc", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(invalidTraversal); err == nil {
		t.Fatal("包含路径遍历的 base_url 应校验失败")
	}

	// 非法 URL：非 HTTP(S)
	invalidScheme := EmbeddingConfigJSON{BaseURL: "ftp://api.example.com/v1", Model: "test", Dimensions: 1024, TimeoutSeconds: 30, BatchSize: 32}
	if err := ValidateEmbeddingConfig(invalidScheme); err == nil {
		t.Fatal("非 HTTP(S) scheme 的 base_url 应校验失败")
	}
}

func TestValidateRerankerConfig(t *testing.T) {
	valid := RerankerConfigJSON{BaseURL: "https://ai.gitee.com/v1", Model: "test", TimeoutSeconds: 10, MaxCandidates: 20}
	if err := ValidateRerankerConfig(valid); err != nil {
		t.Fatalf("合法配置（含 /v1 路径）应通过校验: %v", err)
	}

	invalid := RerankerConfigJSON{BaseURL: "https://api.example.com/v1", Model: "", TimeoutSeconds: 10, MaxCandidates: 20}
	if err := ValidateRerankerConfig(invalid); err == nil {
		t.Fatal("空 model 应校验失败")
	}

	// 非法 URL：包含 userinfo
	invalidUserinfo := RerankerConfigJSON{BaseURL: "https://user:pass@api.example.com/v1", Model: "test", TimeoutSeconds: 10, MaxCandidates: 20}
	if err := ValidateRerankerConfig(invalidUserinfo); err == nil {
		t.Fatal("包含 userinfo 的 base_url 应校验失败")
	}

	// 非法 URL：包含 query
	invalidQuery := RerankerConfigJSON{BaseURL: "https://api.example.com/v1?q=1", Model: "test", TimeoutSeconds: 10, MaxCandidates: 20}
	if err := ValidateRerankerConfig(invalidQuery); err == nil {
		t.Fatal("包含 query 的 base_url 应校验失败")
	}

	// 非法 URL：路径遍历
	invalidTraversal := RerankerConfigJSON{BaseURL: "https://api.example.com/../etc", Model: "test", TimeoutSeconds: 10, MaxCandidates: 20}
	if err := ValidateRerankerConfig(invalidTraversal); err == nil {
		t.Fatal("包含路径遍历的 base_url 应校验失败")
	}
}

func TestProviderConfig_ToDTO(t *testing.T) {
	cfg := ProviderConfig{
		ProviderType:  ProviderTypeEmbedding,
		EncryptedKey:  "some-encrypted-data",
		ConfigJSON:    json.RawMessage(`{"base_url":"https://api.example.com"}`),
		UpdatedAt:     time.Now(),
	}

	dto := cfg.ToDTO()

	if !dto.Configured {
		t.Fatal("Configured 应为 true")
	}
	if !dto.APIKeySet {
		t.Fatal("APIKeySet 应为 true")
	}
	// 验证 DTO 不包含密文
	dtoBytes, _ := json.Marshal(dto)
	dtoStr := string(dtoBytes)
	if contains(dtoStr, "some-encrypted-data") {
		t.Fatal("DTO 不应包含密文")
	}
}

func TestParseProviderType(t *testing.T) {
	tests := []struct {
		input   string
		want    ProviderType
		wantOk  bool
	}{
		{"embedding", ProviderTypeEmbedding, true},
		{"reranker", ProviderTypeReranker, true},
		{"invalid", "invalid", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			pt, ok := ParseProviderType(tt.input)
			if pt != tt.want || ok != tt.wantOk {
				t.Fatalf("ParseProviderType(%q) = (%q, %v), want (%q, %v)", tt.input, pt, ok, tt.want, tt.wantOk)
			}
		})
	}
}

// contains 是简单的子串检查。
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		match := true
		for j := 0; j < len(substr); j++ {
			if s[i+j] != substr[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// 确保 base64 包被使用（用于 generateTestRootKey 中的 base64 编码）
var _ = base64.StdEncoding
