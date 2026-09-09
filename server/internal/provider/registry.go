// registry.go 实现线程安全的 Provider Registry，支持原子热加载。
//
// 引入动机：计划要求管理员保存 Provider 配置后无需重启服务，
// 搜索 pipeline、索引作业和健康检查从 Registry 获取当前 Provider 实例。
// Registry 使用 sync.RWMutex 保证并发安全，原子替换运行时实例。
//
// 设计原则：
//   - 不使用反射
//   - 不出现 XxxService/XxxManager/XxxController
//   - 保存前先校验并构造新 Provider，再原子替换
//   - Provider 未配置时返回 nil，调用方据此降级
//   - 密文存在但根密钥缺失/非法/解密失败时 fail-fast
package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"partitura/server/internal/crypto"
	"partitura/server/internal/search/embedding"
	"partitura/server/internal/search/reranker"
	types "partitura/server/internal/search/types"
)

// Registry 是线程安全的 Provider 注册中心。
// 引入动机：计划要求 Provider 配置保存后无需重启，
// 搜索/作业/健康检查每次操作经 Registry 取得当前实例。
// 使用 RWMutex 保证并发读安全，原子写替换。
type Registry struct {
	mu          sync.RWMutex
	repo        Repository
	rootKey     []byte
	embProvider embedding.Provider
	rrProvider  reranker.Provider
}

// NewRegistry 创建 Provider Registry。
//
// 引入动机：main.go 在启动阶段创建 Registry，注入仓储和根密钥。
// 根密钥可为 nil——表示尚未配置，此时若数据库有密文则 fail-fast。
//
// 参数：
//   - repo：Provider 配置仓储
//   - rootKey：32 字节根密钥（由 crypto.ParseRootKey 解析得到），可为 nil
func NewRegistry(repo Repository, rootKey []byte) *Registry {
	return &Registry{
		repo:    repo,
		rootKey: rootKey,
	}
}

// LoadFromDB 从数据库加载 Provider 配置，解密 API Key，构造 Provider 实例。
//
// 引入动机：启动阶段调用此方法从持久化配置初始化 Provider。
// 若数据库有密文但根密钥缺失/非法/解密失败，返回 error 实现 fail-fast。
// Provider 未配置（无记录）时不返回错误，允许降级启动。
func (r *Registry) LoadFromDB(ctx context.Context) error {
	configs, err := r.repo.List(ctx)
	if err != nil {
		return fmt.Errorf("从数据库加载 Provider 配置: %w", err)
	}

	for _, cfg := range configs {
		if cfg.EncryptedKey == "" {
			// 密文为空——仅配置但无 key，跳过
			slog.Warn("Provider 配置存在但密文为空，跳过", "provider_type", string(cfg.ProviderType))
			continue
		}

		if r.rootKey == nil {
			return fmt.Errorf("数据库存在 %s Provider 密文但根密钥未配置 (MASTER_ENCRYPTION_KEY)，拒绝启动", string(cfg.ProviderType))
		}

		apiKey, err := crypto.Decrypt(r.rootKey, cfg.EncryptedKey)
		if err != nil {
			return fmt.Errorf("解密 %s Provider API Key 失败: %w — 请检查 MASTER_ENCRYPTION_KEY 是否正确", string(cfg.ProviderType), err)
		}

		switch cfg.ProviderType {
		case ProviderTypeEmbedding:
			var embCfg EmbeddingConfigJSON
			if err := json.Unmarshal(cfg.ConfigJSON, &embCfg); err != nil {
				return fmt.Errorf("解析 embedding Provider 配置 JSON: %w", err)
			}
			r.mu.Lock()
			r.embProvider = embedding.NewOpenAICompatibleProvider(types.EmbeddingConfig{
				BaseURL:             embCfg.BaseURL,
				APIKey:              string(apiKey),
				Model:               embCfg.Model,
				Dimensions:          embCfg.Dimensions,
				TimeoutSeconds:      embCfg.TimeoutSeconds,
				BatchSize:           embCfg.BatchSize,
				QueryInstruction:    embCfg.QueryInstruction,
				DocumentInstruction: embCfg.DocumentInstruction,
			})
			r.mu.Unlock()
			slog.Info("Embedding Provider 已从数据库加载", "model", embCfg.Model, "dimensions", embCfg.Dimensions)

		case ProviderTypeReranker:
			var rrCfg RerankerConfigJSON
			if err := json.Unmarshal(cfg.ConfigJSON, &rrCfg); err != nil {
				return fmt.Errorf("解析 reranker Provider 配置 JSON: %w", err)
			}
			r.mu.Lock()
			r.rrProvider = reranker.NewHTTPProvider(types.RerankerConfig{
				BaseURL:        rrCfg.BaseURL,
				APIKey:         string(apiKey),
				Model:          rrCfg.Model,
				TimeoutSeconds: rrCfg.TimeoutSeconds,
				MaxCandidates:  rrCfg.MaxCandidates,
			})
			r.mu.Unlock()
			slog.Info("Reranker Provider 已从数据库加载", "model", rrCfg.Model)
		}
	}

	return nil
}

// GetEmbeddingProvider 返回当前 Embedding Provider 实例。
// 引入动机：搜索 pipeline、索引作业和健康检查每次操作经此方法获取当前实例。
// 未配置时返回 nil，调用方据此降级。
func (r *Registry) GetEmbeddingProvider() embedding.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.embProvider
}

// GetRerankerProvider 返回当前 Reranker Provider 实例。
// 引入动机：搜索 pipeline 和健康检查每次操作经此方法获取当前实例。
// 未配置时返回 nil，调用方据此降级。
func (r *Registry) GetRerankerProvider() reranker.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rrProvider
}

// SaveAndReload 保存 Provider 配置到数据库，然后原子替换运行时实例。
//
// 引入动机：管理员 PUT API 调用此方法——先校验并构造新 Provider，
// 再持久化密文，最后原子替换运行时实例，无需重启。
//
// 参数：
//   - ctx：请求 context
//   - providerType：Provider 类型
//   - apiKey：明文 API Key（将在此方法内加密）
//   - configJSON：非敏感 JSON 配置
//   - updatedBy：操作者 user UUID
func (r *Registry) SaveAndReload(ctx context.Context, providerType ProviderType, apiKey string, configJSON json.RawMessage, updatedBy string) error {
	if r.rootKey == nil {
		return fmt.Errorf("根密钥未配置 (MASTER_ENCRYPTION_KEY)，无法加密 Provider API Key")
	}

	// 加密 API Key
	encryptedKey, err := crypto.Encrypt(r.rootKey, []byte(apiKey))
	if err != nil {
		return fmt.Errorf("加密 Provider API Key: %w", err)
	}

	// 持久化
	if err := r.repo.Upsert(ctx, providerType, encryptedKey, configJSON, updatedBy); err != nil {
		return fmt.Errorf("持久化 Provider 配置: %w", err)
	}

	// 构造新 Provider 并原子替换
	switch providerType {
	case ProviderTypeEmbedding:
		var embCfg EmbeddingConfigJSON
		if err := json.Unmarshal(configJSON, &embCfg); err != nil {
			return fmt.Errorf("解析 embedding 配置: %w", err)
		}
		newProvider := embedding.NewOpenAICompatibleProvider(types.EmbeddingConfig{
			BaseURL:             embCfg.BaseURL,
			APIKey:              apiKey,
			Model:               embCfg.Model,
			Dimensions:          embCfg.Dimensions,
			TimeoutSeconds:      embCfg.TimeoutSeconds,
			BatchSize:           embCfg.BatchSize,
			QueryInstruction:    embCfg.QueryInstruction,
			DocumentInstruction: embCfg.DocumentInstruction,
		})
		r.mu.Lock()
		r.embProvider = newProvider
		r.mu.Unlock()
		slog.Info("Embedding Provider 已热更新", "model", embCfg.Model)

	case ProviderTypeReranker:
		var rrCfg RerankerConfigJSON
		if err := json.Unmarshal(configJSON, &rrCfg); err != nil {
			return fmt.Errorf("解析 reranker 配置: %w", err)
		}
		newProvider := reranker.NewHTTPProvider(types.RerankerConfig{
			BaseURL:        rrCfg.BaseURL,
			APIKey:         apiKey,
			Model:          rrCfg.Model,
			TimeoutSeconds: rrCfg.TimeoutSeconds,
			MaxCandidates:  rrCfg.MaxCandidates,
		})
		r.mu.Lock()
		r.rrProvider = newProvider
		r.mu.Unlock()
		slog.Info("Reranker Provider 已热更新", "model", rrCfg.Model)
	}

	return nil
}

// CreateTestProvider 根据 API Key 和配置构造临时 Provider 实例，不持久化。
//
// 引入动机：POST test 端点需要用传入的凭据临时构造 Provider 进行连通性测试，
// 但不保存到数据库。
// 返回的 Provider 仅用于本次测试请求，用后丢弃。
func (r *Registry) CreateTestProvider(providerType ProviderType, apiKey string, configJSON json.RawMessage) (interface{}, error) {
	switch providerType {
	case ProviderTypeEmbedding:
		var embCfg EmbeddingConfigJSON
		if err := json.Unmarshal(configJSON, &embCfg); err != nil {
			return nil, fmt.Errorf("解析 embedding 配置: %w", err)
		}
		return embedding.NewOpenAICompatibleProvider(types.EmbeddingConfig{
			BaseURL:             embCfg.BaseURL,
			APIKey:              apiKey,
			Model:               embCfg.Model,
			Dimensions:          embCfg.Dimensions,
			TimeoutSeconds:      embCfg.TimeoutSeconds,
			BatchSize:           embCfg.BatchSize,
			QueryInstruction:    embCfg.QueryInstruction,
			DocumentInstruction: embCfg.DocumentInstruction,
		}), nil

	case ProviderTypeReranker:
		var rrCfg RerankerConfigJSON
		if err := json.Unmarshal(configJSON, &rrCfg); err != nil {
			return nil, fmt.Errorf("解析 reranker 配置: %w", err)
		}
		return reranker.NewHTTPProvider(types.RerankerConfig{
			BaseURL:        rrCfg.BaseURL,
			APIKey:         apiKey,
			Model:          rrCfg.Model,
			TimeoutSeconds: rrCfg.TimeoutSeconds,
			MaxCandidates:  rrCfg.MaxCandidates,
		}), nil
	}

	return nil, fmt.Errorf("未知 Provider 类型: %s", string(providerType))
}

// HasRootKey 返回根密钥是否已配置。
// 引入动机：启动检查需要判断根密钥状态。
func (r *Registry) HasRootKey() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.rootKey != nil
}

// CheckExistingSecrets 检查数据库中是否存在 Provider 密文。
// 引入动机：启动时若密文存在但根密钥缺失，需要 fail-fast。
// 此方法仅检查存在性，不解密。
func CheckExistingSecrets(ctx context.Context, repo Repository) (bool, error) {
	configs, err := repo.List(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	for _, cfg := range configs {
		if cfg.EncryptedKey != "" {
			return true, nil
		}
	}
	return false, nil
}
