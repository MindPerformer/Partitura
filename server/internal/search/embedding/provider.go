// Package embedding 实现可替换的 Embedding Provider 接口和 OpenAI-compatible HTTP 实现。
//
// 引入动机：design/01-SEARCH.md §Embedding Provider 要求：
//   - 定义 Provider abstraction
//   - 至少支持 OpenAI-compatible Embeddings API
//   - 配置 base_url, api_key, model, dimensions, timeout, batch_size, query_instruction, document_instruction
//   - 默认推荐 Qwen3 Embedding，但不写死模型名/维度/Provider
//   - 不得部署或引入任何本地 embedding 模型
//
// 设计原则：
//   - 接口化，可替换 Provider
//   - 失败需有结构化日志和可重试 job 错误
//   - 禁止阻塞 document 保存
//   - 不写死 provider/base URL/API key/model/dimensions
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"partitura/server/internal/search/httpdiag"
	types "partitura/server/internal/search/types"
)

// Provider 定义 Embedding Provider 接口。
// 引入动机：design/01-SEARCH.md §Embedding Provider 要求定义 Provider abstraction，
// 支持可替换的在线 Embedding API。
// 任何实现必须不写死 model/dimensions，通过配置传入。
type Provider interface {
	// Embed 将文本列表转换为向量列表。
	// 引入动机：index_document job 和搜索管线都需要调用 embedding。
	//
	// 参数：
	//   - ctx：请求 context
	//   - texts：待向量化的文本列表
	//
	// 返回向量列表，每个向量长度等于配置的 dimensions。
	// 如果 Provider 不可用，返回 error 供调用方降级处理。
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions 返回当前配置的向量维度。
	// 引入动机：ES index mapping 和搜索管线需要知道向量维度。
	Dimensions() int

	// Available 检查 Provider 是否可用。
	// 引入动机：搜索管线需要判断 embedding path 是否可用，不可用时降级为 lexical-only。
	Available(ctx context.Context) bool
}

// OpenAICompatibleProvider 是 OpenAI-compatible Embeddings API 的 HTTP 实现。
// 引入动机：design/01-SEARCH.md §Embedding Provider 要求至少支持 OpenAI-compatible API。
// Qwen3 Embedding 等 Provider 也兼容此 API 格式。
//
// 不写死任何 provider/base URL/API key/model/dimensions，全部通过 EmbeddingConfig 传入。
type OpenAICompatibleProvider struct {
	config     types.EmbeddingConfig
	httpClient *http.Client
}

// NewOpenAICompatibleProvider 创建 OpenAI-compatible Embedding Provider。
// 超时策略：embedding 批处理在慢模型/大 batch 下可能超过 30s 默认值，统一放宽为 5 分钟。
func NewOpenAICompatibleProvider(cfg types.EmbeddingConfig) *OpenAICompatibleProvider {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	return &OpenAICompatibleProvider{
		config:     cfg,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// embeddingRequest 是 OpenAI-compatible embeddings API 的请求体。
type embeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

// embeddingResponse 是 OpenAI-compatible embeddings API 的响应体。
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Embed 将文本列表转换为向量列表。
// 引入动机：index_document job 和搜索管线都需要调用 embedding。
// 按 batch_size 分批调用 API，避免单次请求过大。
func (p *OpenAICompatibleProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	batchSize := p.config.BatchSize
	if batchSize <= 0 {
		batchSize = 32
	}

	var allVectors [][]float32
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		vectors, err := p.embedBatch(ctx, batch)
		if err != nil {
			return nil, fmt.Errorf("embedding batch %d-%d: %w", i, end-1, err)
		}
		allVectors = append(allVectors, vectors...)
	}

	return allVectors, nil
}

// embedBatch 调用一次 embedding API 处理一批文本。
//
// 错误处理顺序（严格）：
//  1. 发送请求（含 Accept: application/json 和 Content-Type: application/json）
//  2. 检查 HTTP 状态码 — 非 2xx 立即失败，不尝试解析响应体
//  3. 检查响应 Content-Type — 非 application/json 立即失败
//  4. JSON 解码响应体
//  5. 检查 API 级别 error 字段
//  6. 验证向量数量和维度
//
// 此顺序确保 HTML 错误页、代理重定向页等非 JSON 响应在 JSON 解码之前
// 被状态码或 Content-Type 检查拦截，不会产生 "invalid character '<'" 解析错误，
// 且错误信息包含状态码和 Content-Type 供诊断，不泄露响应体。
func (p *OpenAICompatibleProvider) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	startedAt := time.Now()
	configuredTimeout := p.httpClient.Timeout
	reqBody := embeddingRequest{
		Model:      p.config.Model,
		Input:      texts,
		Dimensions: p.config.Dimensions,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化 embedding 请求: %w", err)
	}

	// 规范化 base URL：去除尾部斜杠，防止拼接时产生双斜杠
	baseURL := strings.TrimRight(p.config.BaseURL, "/")
	url := baseURL + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建 embedding 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		httpdiag.LogFailure(ctx, slog.LevelError, "embedding API 调用失败", httpdiag.Diagnostics{
			StartedAt: startedAt, URL: url, Model: p.config.Model, InputCount: len(texts), RequestBytes: len(body),
			ConfiguredTimeout: configuredTimeout, RequestBody: body, Err: err, Secret: p.config.APIKey,
		})
		return nil, fmt.Errorf("调用 embedding API: %w", err)
	}
	defer resp.Body.Close()

	responseBody, readErr := io.ReadAll(resp.Body)
	contentType := resp.Header.Get("Content-Type")
	baseDiag := httpdiag.Diagnostics{StartedAt: startedAt, URL: url, Model: p.config.Model, InputCount: len(texts), RequestBytes: len(body), ConfiguredTimeout: configuredTimeout, HTTPStatus: resp.StatusCode, ContentType: contentType, RequestBody: body, ResponseBody: responseBody, Secret: p.config.APIKey}
	if readErr != nil {
		baseDiag.Err = readErr
		httpdiag.LogFailure(ctx, slog.LevelError, "读取 embedding API 响应失败", baseDiag)
		return nil, fmt.Errorf("读取 embedding 响应: %w", readErr)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpdiag.LogFailure(ctx, slog.LevelError, "embedding API 返回非 2xx 状态码", baseDiag)
		return nil, fmt.Errorf("embedding API 返回非 2xx 状态码 %d (Content-Type: %s)", resp.StatusCode, contentType)
	}

	if !strings.Contains(contentType, "application/json") {
		httpdiag.LogFailure(ctx, slog.LevelError, "embedding API 响应 Content-Type 非 JSON", baseDiag)
		return nil, fmt.Errorf("embedding API 响应 Content-Type 非 JSON: %s (状态码 %d)", contentType, resp.StatusCode)
	}

	var result embeddingResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		baseDiag.Err = err
		httpdiag.LogFailure(ctx, slog.LevelError, "解析 embedding JSON 响应失败", baseDiag)
		return nil, fmt.Errorf("解析 embedding 响应: %w", err)
	}

	// 4. 检查 API 级别 error 字段
	if result.Error != nil {
		baseDiag.Err = fmt.Errorf("%s", result.Error.Message)
		httpdiag.LogFailure(ctx, slog.LevelError, "embedding API 返回错误", baseDiag)
		return nil, fmt.Errorf("embedding API 错误: %s", httpdiag.RedactSensitive(result.Error.Message, p.config.APIKey))
	}

	if len(result.Data) != len(texts) {
		httpdiag.LogFailure(ctx, slog.LevelError, "embedding 返回向量数量不匹配", baseDiag)
		slog.Error("embedding 返回向量数量不匹配详情", "expected", len(texts), "got", len(result.Data))
		return nil, fmt.Errorf("embedding 返回向量数量不匹配: expected %d, got %d", len(texts), len(result.Data))
	}

	vectors := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		if len(d.Embedding) != p.config.Dimensions {
			httpdiag.LogFailure(ctx, slog.LevelError, "embedding 向量维度不匹配", baseDiag)
			slog.Error("embedding 向量维度不匹配详情", "expected", p.config.Dimensions, "got", len(d.Embedding))
			return nil, fmt.Errorf("embedding 向量维度不匹配: expected %d, got %d", p.config.Dimensions, len(d.Embedding))
		}
		vectors[i] = d.Embedding
	}

	return vectors, nil
}

// Dimensions 返回当前配置的向量维度。
func (p *OpenAICompatibleProvider) Dimensions() int {
	return p.config.Dimensions
}

// Available 检查 Provider 是否可用。
// 引入动机：搜索管线需要判断 embedding path 是否可用。
// 通过发送一个最小 embedding 请求来验证 API 连通性和凭据有效性。
//
// 设计原则：
//   - 仅检查配置完整性是不够的——base_url 可能指向错误路径（如缺少 /v1 前缀），
//     配置完整但 API 返回 404 时 readyz 仍会误报 healthy。
//   - 发送单条文本的 embedding 请求，检查 HTTP 状态码和响应格式。
//   - 超时使用配置的 timeout，避免阻塞 readyz。
//   - 任何错误（网络、HTTP 非 2xx、JSON 解析失败）都返回 false。
//   - 不缓存结果——每次调用都实时检查，反映 Provider 热更新后的状态。
func (p *OpenAICompatibleProvider) Available(ctx context.Context) bool {
	if p.config.BaseURL == "" || p.config.Model == "" {
		return false
	}

	// 发送最小 embedding 请求验证 API 可用性
	vectors, err := p.Embed(ctx, []string{"health check"})
	if err != nil {
		slog.Debug("embedding Available 检查失败", "error", err, "model", p.config.Model)
		return false
	}
	return len(vectors) > 0
}
