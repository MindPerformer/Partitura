// Package reranker 实现可替换的 Reranker Provider 接口和在线 HTTP 实现。
//
// 引入动机：design/01-SEARCH.md §Reranker Provider 要求：
//   - 定义独立 Provider
//   - 配置 base_url, api_key, model, timeout, max_candidates
//   - 默认推荐 Qwen3 Reranker，Provider 化
//   - Reranker 不可用时直接返回 RRF 结果，系统降级而不是搜索失败
//   - 不得成为搜索单点故障
//
// 设计原则：
//   - 接口化，可替换 Provider
//   - 异常、超时、无效结果必须记录降级原因且直接返回 RRF 结果
//   - 不使 search HTTP 失败
//   - 不写死 provider/base URL/API key/model
package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	types "partitura/server/internal/search/types"
)

// Provider 定义 Reranker Provider 接口。
// 引入动机：design/01-SEARCH.md §Reranker Provider 要求定义独立 Provider。
type Provider interface {
	// Rerank 对候选文本列表按与 query 的相关性进行重排序。
	// 引入动机：design/01-SEARCH.md 搜索管线要求 reranker 对 RRF top candidates 重排序。
	//
	// 参数：
	//   - ctx：请求 context
	//   - query：查询文本
	//   - candidates：候选文本列表
	//
	// 返回按相关性降序排列的结果列表。
	// 如果 Provider 不可用，返回 error 供调用方降级为 RRF 结果。
	Rerank(ctx context.Context, query string, candidates []types.RerankerCandidate) ([]types.RerankerResult, error)

	// Available 检查 Provider 是否可用。
	Available(ctx context.Context) bool
}

// HTTPProvider 是在线 Reranker API 的 HTTP 实现。
// 引入动机：design/01-SEARCH.md §Reranker Provider 要求在线 API，
// 支持 OpenAI-compatible reranker API 格式。
type HTTPProvider struct {
	config      types.RerankerConfig
	httpClient  *http.Client
}

// NewHTTPProvider 创建 Reranker HTTP Provider。
// 超时策略：E2E/真实第三方 rerank 常对长文档较慢，超时下限统一 5 分钟（默认 300s），
// 上限 10 分钟。调用方若未配置超时则按 5 分钟默认生效。
func NewHTTPProvider(cfg types.RerankerConfig) *HTTPProvider {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	return &HTTPProvider{
		config:     cfg,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// rerankerRequest 是 reranker API 的请求体。
// 引入动机：兼容常见的 reranker API 格式。
type rerankerRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

// rerankerResponse 是 reranker API 的响应体。
type rerankerResponse struct {
	Results []struct {
		Index    int     `json:"index"`
		Score    float64 `json:"relevance_score"`
	} `json:"results"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Rerank 对候选文本列表按与 query 的相关性进行重排序。
func (p *HTTPProvider) Rerank(ctx context.Context, query string, candidates []types.RerankerCandidate) ([]types.RerankerResult, error) {
	if len(candidates) == 0 {
		return nil, nil
	}

	// 限制候选数量
	maxCand := p.config.MaxCandidates
	if maxCand <= 0 {
		maxCand = len(candidates)
	}
	if len(candidates) > maxCand {
		candidates = candidates[:maxCand]
	}

	docs := make([]string, len(candidates))
	for i, c := range candidates {
		docs[i] = c.Text
	}

	reqBody := rerankerRequest{
		Model:     p.config.Model,
		Query:     query,
		Documents: docs,
		TopN:      len(candidates),
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("序列化 reranker 请求: %w", err)
	}

	// 规范化 base URL：去除尾部斜杠，防止拼接时产生双斜杠
	baseURL := strings.TrimRight(p.config.BaseURL, "/")
	url := baseURL + "/rerank"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建 reranker 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if p.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.config.APIKey)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		slog.Warn("reranker API 调用失败，将降级为 RRF 结果", "error", err, "model", p.config.Model)
		return nil, fmt.Errorf("调用 reranker API: %w", err)
	}
	defer resp.Body.Close()

	// 1. 非 2xx 立即失败，不尝试解析响应体
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("reranker API 返回非 2xx 状态码，将降级为 RRF 结果",
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"path", url,
			"model", p.config.Model)
		return nil, fmt.Errorf("reranker API 返回非 2xx 状态码 %d (Content-Type: %s)", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// 2. 检查响应 Content-Type 必须是 JSON
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/json") {
		slog.Warn("reranker API 响应 Content-Type 非 JSON，将降级为 RRF 结果",
			"content_type", contentType,
			"status", resp.StatusCode,
			"path", url,
			"model", p.config.Model)
		return nil, fmt.Errorf("reranker API 响应 Content-Type 非 JSON: %s (状态码 %d)", contentType, resp.StatusCode)
	}

	// 3. JSON 解码响应体
	var result rerankerResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		slog.Warn("解析 reranker JSON 响应失败，将降级为 RRF 结果",
			"error", err,
			"status", resp.StatusCode,
			"content_type", contentType,
			"path", url)
		return nil, fmt.Errorf("解析 reranker 响应: %w", err)
	}

	// 4. 检查 API 级别 error 字段
	if result.Error != nil {
		slog.Warn("reranker API 返回错误，将降级为 RRF 结果",
			"message", result.Error.Message,
			"path", url,
			"model", p.config.Model)
		return nil, fmt.Errorf("reranker API 错误: %s", result.Error.Message)
	}

	if len(result.Results) == 0 {
		slog.Warn("reranker 返回空结果，将降级为 RRF 结果",
			"path", url,
			"model", p.config.Model)
		return nil, fmt.Errorf("reranker 返回空结果")
	}

	// 5. 验证结果索引在合法范围内 [0, len(candidates))
	// 引入动机：API 可能因内部错误返回越界索引，调用方用 Index 映射回候选列表，
	// 越界索引会导致下游 panic 或错误排序。
	for i, r := range result.Results {
		if r.Index < 0 || r.Index >= len(candidates) {
			slog.Error("reranker 返回越界索引",
				"result_index", i,
				"returned_index", r.Index,
				"candidate_count", len(candidates),
				"path", url,
				"model", p.config.Model)
			return nil, fmt.Errorf("reranker 返回越界索引: result[%d].index=%d, candidates=%d", i, r.Index, len(candidates))
		}
	}

	// 6. 部分结果警告：返回结果数少于输入候选数
	// 引入动机：某些 API 可能过滤低相关性候选而返回较少结果，
	// 这不一定是错误，但调用方应知晓部分结果可能缺失候选。
	if len(result.Results) < len(candidates) {
		slog.Warn("reranker 返回部分结果",
			"expected", len(candidates),
			"got", len(result.Results),
			"path", url,
			"model", p.config.Model)
	}

	// 转换为 RerankerResult
	results := make([]types.RerankerResult, len(result.Results))
	for i, r := range result.Results {
		results[i] = types.RerankerResult{
			Index: r.Index,
			Score: r.Score,
		}
	}

	return results, nil
}

// Available 检查 Provider 是否可用。
// 引入动机：搜索管线需要判断 reranker path 是否可用。
// 通过发送一个最小 reranker 请求来验证 API 连通性和凭据有效性。
//
// 设计原则：
//   - 仅检查配置完整性是不够的——base_url 可能指向错误路径，
//     配置完整但 API 返回 404 时 readyz 仍会误报 healthy。
//   - 发送单条文本的 reranker 请求，检查 HTTP 状态码和响应格式。
//   - 超时使用配置的 timeout，避免阻塞 readyz。
//   - 任何错误（网络、HTTP 非 2xx、JSON 解析失败）都返回 false。
//   - 不缓存结果——每次调用都实时检查，反映 Provider 热更新后的状态。
func (p *HTTPProvider) Available(ctx context.Context) bool {
	if p.config.BaseURL == "" || p.config.Model == "" {
		return false
	}

	// 发送最小 reranker 请求验证 API 可用性
	_, err := p.Rerank(ctx, "health check", []types.RerankerCandidate{{Text: "test"}})
	if err != nil {
		slog.Debug("reranker Available 检查失败", "error", err, "model", p.config.Model)
		return false
	}
	return true
}
