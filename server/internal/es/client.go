// Package es 实现 Elasticsearch 客户端封装：连接管理、index mapping、版本化索引和 alias 管理。
//
// 引入动机：design/01-SEARCH.md §Index Version 要求索引版本化（knowledge_v12, knowledge_v13），
// 使用 alias（knowledge_current）原子切换，禁止原地破坏 active index。
// design/04-WEB-API.md §Security 要求 ES query 只能由服务器生成。
//
// 设计原则：
//   - ES 是可从 PostgreSQL 完整重建的搜索索引，不是业务真相源
//   - 对 embedding model/dimensions/instructions/chunk algorithm/analyzer 的重大变更，
//     创建新 index、全量重建、验证后用 ES aliases 原子切换
//   - 保留旧索引以支持 rollback，延迟清理由 job 处理
//   - 绝不原地破坏 active index
//   - 接口化，便于测试用 fake 替换
package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AliasName 是搜索索引的统一 alias 名称。
// 引入动机：design/01-SEARCH.md §Index Version 要求使用 alias knowledge_current。
const AliasName = "knowledge_current"

// IndexNamePrefix 是索引名称前缀。
// 引入动机：design/01-SEARCH.md §Index Version 要求索引命名为 knowledge_v{n}。
const IndexNamePrefix = "knowledge_v"

// Client 定义 Elasticsearch 客户端接口。
// 引入动机：搜索管线、index job、integrity check 都需要与 ES 交互，
// 接口化便于测试用 fake 替换真实 ES 连接。
type Client interface {
	// Ping 检查 ES 连接是否可用。
	Ping(ctx context.Context) error

	// CreateIndex 创建指定名称的索引，使用给定的 mapping 和 settings。
	// 引入动机：design/01-SEARCH.md §Index Version 要求为新 profile 创建新 index。
	CreateIndex(ctx context.Context, indexName string, mapping map[string]interface{}) error

	// DeleteIndex 删除指定名称的索引。
	// 引入动机：cleanup_old_indexes job 需要删除过期索引。
	DeleteIndex(ctx context.Context, indexName string) error

	// IndexExists 检查指定名称的索引是否存在。
	IndexExists(ctx context.Context, indexName string) (bool, error)

	// GetIndexDimensions 读取索引 mapping 中 embedding 字段的维度（dims）。
	// 引入动机：当 search profile 的 embedding 维度与已存在索引的 mapping 维度不一致时，
	// rebuild/index job 需要据此判断是否必须创建新索引，而非复用维度不可变的旧索引。
	// 索引不存在或没有 embedding 字段时返回 (0, nil)，调用方据此视为"无已知维度"。
	GetIndexDimensions(ctx context.Context, indexName string) (int, error)

	// UpdateAlias 原子性地更新 alias 指向的索引。
	// 引入动机：design/01-SEARCH.md §Index Version 要求 alias 原子切换。
	// actions 定义 add/remove 操作序列。
	UpdateAlias(ctx context.Context, actions []AliasAction) error

	// GetAliasIndex 返回 alias 当前指向的索引名称。
	// 如果 alias 不存在或指向多个索引，返回空字符串和 error。
	GetAliasIndex(ctx context.Context, alias string) (string, error)

	// BulkIndex 批量索引文档。
	// 引入动机：rebuild 和 index_document job 需要批量写入 chunk 到 ES。
	BulkIndex(ctx context.Context, indexName string, docs []IndexDoc) error

	// DeleteByQuery 删除匹配 query 的文档。
	// 引入动机：index_document job 需要先删除旧 chunk 再写入新 chunk。
	DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error

	// Search 执行搜索查询。
	// 引入动机：搜索管线需要执行 BM25 + vector 搜索。
	Search(ctx context.Context, indexName string, query map[string]interface{}) (*SearchResponse, error)

	// Count 返回匹配 query 的文档数量。
	// 引入动机：integrity check 需要统计 ES 中的 chunk 数量。
	Count(ctx context.Context, indexName string, query map[string]interface{}) (int64, error)

	// GetDocument 根据 ID 获取单个文档。
	GetDocument(ctx context.Context, indexName, docID string) (map[string]interface{}, error)

	// Refresh 刷新索引使最近写入的文档可搜索。
	// 引入动机：rebuild 完成后需要 refresh 才能验证。
	Refresh(ctx context.Context, indexName string) error

	// ListIndices 列出匹配 pattern 的所有索引名称。
	// 引入动机：cleanup_old_indexes job 需要列出所有 knowledge_v* 索引，
	// 以识别可清理的旧版本索引。
	ListIndices(ctx context.Context, pattern string) ([]string, error)
}

// AliasAction 定义 alias 更新操作。
// 引入动机：ES _aliases API 使用 actions 数组进行原子操作。
//
// ES8 要求的 JSON 格式为嵌套结构：
//
//	{"actions":[{"add":{"index":"knowledge_v1","alias":"knowledge_current"}}]}
//
// 而非扁平结构 {"actions":[{"Action":"add","Index":"...","Alias":"..."}]}。
// 通过自定义 MarshalJSON 实现 ES8 兼容的嵌套 JSON 格式。
type AliasAction struct {
	Action string // "add" 或 "remove"
	Index  string
	Alias  string
}

// aliasActionJSON 是 ES8 _aliases API 要求的嵌套 JSON 结构。
// 引入动机：ES8 的 _aliases 端点要求每个 action 是一个对象，
// 其 key 为操作类型（"add"/"remove"），value 为包含 index 和 alias 的对象。
// 此结构体用于 MarshalJSON 时生成正确的嵌套格式。
type aliasActionJSON struct {
	Add    *aliasActionParams `json:"add,omitempty"`
	Remove *aliasActionParams `json:"remove,omitempty"`
}

// aliasActionParams 是 alias action 的参数部分。
// 引入动机：ES8 _aliases API 中 add/remove 操作的参数包含 index 和 alias，
// 使用小写 JSON tag 匹配 ES8 API 规范。
type aliasActionParams struct {
	Index string `json:"index"`
	Alias string `json:"alias"`
}

// MarshalJSON 实现 ES8 兼容的嵌套 JSON 序列化。
// 引入动机：ES8 _aliases API 要求 actions 数组中每个元素是
// {"add":{"index":"...","alias":"..."}} 或 {"remove":{"index":"...","alias":"..."}} 格式，
// 而非 Go 结构体的默认扁平序列化。此方法确保生成的 JSON 符合 ES8 规范。
func (a AliasAction) MarshalJSON() ([]byte, error) {
	params := &aliasActionParams{
		Index: a.Index,
		Alias: a.Alias,
	}
	switch a.Action {
	case "add":
		return json.Marshal(aliasActionJSON{Add: params})
	case "remove":
		return json.Marshal(aliasActionJSON{Remove: params})
	default:
		return nil, fmt.Errorf("未知的 alias action 类型: %s", a.Action)
	}
}

// IndexDoc 是要索引的单条文档。
type IndexDoc struct {
	ID   string
	Body map[string]interface{}
}

// SearchResponse 是 ES 搜索响应的简化结构。
type SearchResponse struct {
	Hits struct {
		Total struct {
			Value int64 `json:"value"`
		} `json:"total"`
		Hits []struct {
			ID     string                 `json:"_id"`
			Score  float64                `json:"_score"`
			Source map[string]interface{} `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
	// Aggregations 是 ES 聚合响应的原始 map。
	// 引入动机：integrity check 需要通过 terms aggregation 获取所有已索引的 document_id
	// 及其 chunk count、revision、content_hash 等信息，用于与 PG 对比。
	Aggregations map[string]interface{} `json:"aggregations,omitempty"`
}

// HTTPClient 是基于 net/http 的 Elasticsearch 客户端实现。
// 引入动机：不引入重量级 ES SDK，使用标准库 HTTP 客户端与 ES REST API 交互，
// 保持依赖最小化和可控性。
type HTTPClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewHTTPClient 创建 ES HTTP 客户端。
// 引入动机：config 加载 ES_URL 后需要创建客户端实例。
func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Ping 检查 ES 连接是否可用。
func (c *HTTPClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return fmt.Errorf("创建 ping 请求: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ES 不可达: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ES ping 返回非 200 状态码: %d", resp.StatusCode)
	}
	return nil
}

// CreateIndex 创建指定名称的索引。
func (c *HTTPClient) CreateIndex(ctx context.Context, indexName string, mapping map[string]interface{}) error {
	body, err := json.Marshal(mapping)
	if err != nil {
		return fmt.Errorf("序列化 index mapping: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/"+indexName, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建 index 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("创建 index %s: %w", indexName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var errBody struct {
			Error map[string]interface{} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		slog.Error("ES 创建索引失败", "index", indexName, "status", resp.StatusCode, "error", errBody.Error)
		return fmt.Errorf("创建 index %s 返回状态码 %d", indexName, resp.StatusCode)
	}

	slog.Info("ES 索引创建成功", "index", indexName)
	return nil
}

// DeleteIndex 删除指定名称的索引。
func (c *HTTPClient) DeleteIndex(ctx context.Context, indexName string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/"+indexName, nil)
	if err != nil {
		return fmt.Errorf("创建删除 index 请求: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("删除 index %s: %w", indexName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("删除 index %s 返回状态码 %d", indexName, resp.StatusCode)
	}

	slog.Info("ES 索引删除成功", "index", indexName)
	return nil
}

// IndexExists 检查指定名称的索引是否存在。
func (c *HTTPClient) IndexExists(ctx context.Context, indexName string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL+"/"+indexName, nil)
	if err != nil {
		return false, fmt.Errorf("创建 HEAD index 请求: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("检查 index %s 是否存在: %w", indexName, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("检查 index %s 存在性返回状态码 %d", indexName, resp.StatusCode)
	}
}

// UpdateAlias 原子性地更新 alias 指向的索引。
func (c *HTTPClient) UpdateAlias(ctx context.Context, actions []AliasAction) error {
	body := map[string]interface{}{
		"actions": actions,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化 alias 更新: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/_aliases", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("创建 alias 更新请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("更新 alias: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 解析 ES 错误响应体以提供更准确的错误信息
		// 引入动机：原实现仅返回状态码，无法区分 400 的具体原因
		// （如同名索引冲突、alias 不存在等）。解析错误体有助于诊断和安全日志。
		var errBody struct {
			Error struct {
				Type    string `json:"type"`
				Reason  string `json:"reason"`
				Index   string `json:"index"`
				Details string `json:"index_uuid"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		slog.Error("ES alias 更新失败",
			"status", resp.StatusCode,
			"error_type", errBody.Error.Type,
			"error_reason", errBody.Error.Reason,
		)
		return fmt.Errorf("更新 alias 返回状态码 %d: %s: %s", resp.StatusCode, errBody.Error.Type, errBody.Error.Reason)
	}

	slog.Info("ES alias 更新成功", "actions", len(actions))
	return nil
}

// GetAliasIndex 返回 alias 当前指向的索引名称。
func (c *HTTPClient) GetAliasIndex(ctx context.Context, alias string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/_alias/"+alias, nil)
	if err != nil {
		return "", fmt.Errorf("创建 alias 查询请求: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("查询 alias %s: %w", alias, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("alias %s 不存在", alias)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("查询 alias %s 返回状态码 %d", alias, resp.StatusCode)
	}

	var result map[string]map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析 alias 响应: %w", err)
	}

	if len(result) == 0 {
		return "", fmt.Errorf("alias %s 未指向任何索引", alias)
	}
	if len(result) > 1 {
		return "", fmt.Errorf("alias %s 指向多个索引", alias)
	}

	for indexName := range result {
		return indexName, nil
	}

	return "", fmt.Errorf("alias %s 响应为空", alias)
}

// GetIndexDimensions 读取索引 mapping 中 embedding 字段的维度（dims）。
//
// 引入动机：ES 的 dense_vector.dims 建好后不可变，必须能读取既有索引的真实维度，
// 才能检测"search profile 的 embedding 维度与既有索引 mapping 维度不一致"，
// 进而创建新索引并重建，而不是把新维度向量写入维度不可变的旧索引（写入必然失败）。
//
// 语义约定：
//   - 索引不存在（HTTP 404）→ (0, nil)，调用方据此视为"无已知维度"
//   - 索引存在但 mapping 中没有 embedding 字段 → (0, nil)，同样视为"无已知维度"
//     （例如向 alias 名称直接写入时 ES 自动创建的空索引）
//   - 其他非 200 状态码、响应不是唯一索引条目、JSON 解析失败、dims 非法 → 返回 error
func (c *HTTPClient) GetIndexDimensions(ctx context.Context, indexName string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+indexName+"/_mapping", nil)
	if err != nil {
		return 0, fmt.Errorf("创建 get mapping 请求: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("查询 index %s mapping: %w", indexName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 索引不存在不是错误：调用方需要"无已知维度"这一结论来决定后续动作。
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("查询 index %s mapping 返回状态码 %d", indexName, resp.StatusCode)
	}

	var result map[string]struct {
		Mappings struct {
			Properties struct {
				Embedding struct {
					// 用指针区分"字段缺失"与"字段为 0"：dims 缺失时返回"无已知维度"。
					Dims *int `json:"dims"`
				} `json:"embedding"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("解析 index %s mapping 响应: %w", indexName, err)
	}

	if len(result) != 1 {
		// ES 对存在的索引必然返回唯一索引条目；0 个或多个都说明响应不符合预期。
		return 0, fmt.Errorf("查询 index %s mapping 返回 %d 个索引条目，期望唯一索引条目", indexName, len(result))
	}

	for indexKey, entry := range result {
		dims := entry.Mappings.Properties.Embedding.Dims
		if dims == nil {
			slog.Warn("index mapping 缺少 embedding.dims，视为无已知维度", "index", indexKey)
			return 0, nil
		}
		if *dims <= 0 {
			// dense_vector 的 dims 必须为正整数，出现非正值说明 mapping 异常或响应被篡改。
			return 0, fmt.Errorf("index %s mapping 的 embedding.dims 非法: %d", indexKey, *dims)
		}
		return *dims, nil
	}

	return 0, fmt.Errorf("查询 index %s mapping 未得到索引条目", indexName)
}

const (
	bulkResponseLimit   = 2 << 20
	diagnosticBodyLimit = 4096
	bulkErrorLogLimit   = 5
)

// bulkItemResponse 是 bulk 响应中单个操作的诊断字段。
// 只保留 ES 返回的元数据和错误分类，避免把文档 source 写入日志。
type bulkItemResponse struct {
	Index  string          `json:"_index"`
	ID     string          `json:"_id"`
	Status int             `json:"status"`
	Error  json.RawMessage `json:"error"`
}

// diagnosticContext 返回一次 HTTP 调用的可观测上下文，不包含请求体。
func (c *HTTPClient) diagnosticContext(ctx context.Context, requestURL string, docCount, requestBytes int, started time.Time) []any {
	attrs := []any{
		"url", diagnosticURL(requestURL),
		"documents", docCount,
		"request_bytes", requestBytes,
		"client_timeout", c.httpClient.Timeout,
		"duration", time.Since(started),
	}
	if deadline, ok := ctx.Deadline(); ok {
		attrs = append(attrs, "context_deadline", deadline, "context_remaining", time.Until(deadline))
	} else {
		attrs = append(attrs, "context_deadline", nil, "context_remaining", nil)
	}
	return attrs
}

// diagnosticURL 只记录 scheme/host/path，避免 URL 中的 userinfo 或 query 凭据进入日志。
func diagnosticURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: parsed.Path}).String()
}

// diagnosticBodySummary 限制响应摘要大小，并遮蔽常见凭据字段。
func diagnosticBodySummary(body []byte) string {
	if len(body) > diagnosticBodyLimit {
		body = body[:diagnosticBodyLimit]
	}
	text := string(body)
	for _, key := range []string{"password", "passwd", "token", "api_key", "apikey", "authorization", "credential", "secret"} {
		re := regexp.MustCompile(`(?i)([\"']?` + regexp.QuoteMeta(key) + `[\"']?\s*[:=]\s*[\"']?)[^,}\"']+`)
		text = re.ReplaceAllString(text, `${1}<redacted>`)
	}
	return text
}

// esErrorBody 是 ES 标准错误响应体的结构。
// 引入动机：ES 在 4xx/5xx 时返回 {"error":{"type":"...","reason":"..."},"status":400}，
// 只有解析出 type/reason 才能在不把整个响应体写进日志的前提下定位真实失败原因
// （例如 dense_vector 维度不匹配、index_not_found_exception）。
type esErrorBody struct {
	Error struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	} `json:"error"`
}

// parseESErrorBody 解析 ES 标准错误响应体，返回 (error.type, error.reason)。
//
// 引入动机：Search 等操作需要把 ES 的真实失败原因带进返回的 error 与 slog 日志，
// 只报告 HTTP 状态码无法诊断线上问题（如 400 的维度不匹配）。
//
// 语义：ok=false 表示响应体不是 ES 标准错误结构（非 JSON，或 type/reason 均为空），
// 调用方应退化为 diagnosticBodySummary 的截断摘要，保证错误信息在任何情况下都可定位。
func parseESErrorBody(body []byte) (errorType, reason string, ok bool) {
	var parsed esErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", false
	}
	errorType = strings.TrimSpace(parsed.Error.Type)
	reason = strings.TrimSpace(parsed.Error.Reason)
	if errorType == "" && reason == "" {
		return "", "", false
	}
	return errorType, reason, true
}

// esErrorDetail 把 ES 错误类型与原因拼成 "type: reason" 形式的单行摘要。
// 引入动机：type 与 reason 可能只存在其中一个（ES 部分错误只给 reason），
// 该函数保证不会拼出 "': '" 这类空字段结果，便于错误信息被稳定识别。
func esErrorDetail(errorType, reason string) string {
	switch {
	case errorType != "" && reason != "":
		return errorType + ": " + reason
	case errorType != "":
		return errorType
	default:
		return reason
	}
}

// bulkRetryMaxAttempts 是 bulk 写入遇到可重试状态码时的最大尝试次数（含首次尝试）。
// 引入动机：ES 的 429（too_many_requests，写入限流/队列拒绝）与 503（分片或节点暂时不可用）
// 属于瞬时过载，退避重试几次通常即可恢复；设上限是为了不把 job 无限拖住，
// 也避免重试流量把已经过载的 ES 压得更狠。
const bulkRetryMaxAttempts = 5

// bulkRetryBaseDelay 是首次重试前的退避时长，之后每次翻倍。
// 引入动机：给过载的 ES 留出回收写入队列的时间；200ms 足够短，不会明显拖慢正常写入。
const bulkRetryBaseDelay = 200 * time.Millisecond

// bulkRetryMaxDelay 是单次退避时长的上限。
// 引入动机：避免指数增长让 job 长时间无进展且难以观测；5s 与 ES 限流恢复的量级相当。
const bulkRetryMaxDelay = 5 * time.Second

// bulkRetryJitterRatio 是退避的随机抖动比例，实际等待落在 [d*(1-r), d*(1+r)] 区间。
// 引入动机：多个写入方对同一个过载 ES 同步重试会形成尖峰并再次触发限流，
// 抖动把重试时刻打散，提高重试的整体成功率。
const bulkRetryJitterRatio = 0.2

// isRetryableBulkStatus 判断 bulk 写入的 HTTP 状态码是否属于可重试的瞬时过载。
// 引入动机：只对 429/503 重试；其他非 200（如 400 mapping/维度错误、404 索引不存在）
// 是确定性失败，重试只会浪费 job 配额并掩盖真实原因。
func isRetryableBulkStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// bulkRetryDelay 返回第 attempt 次尝试失败后到下一次尝试前的退避时长。
// 引入动机：把"指数退避 + 抖动 + 上限"集中成一个可单测的纯函数，便于调参与验证边界。
// attempt 从 1 开始计数（1 表示首次尝试刚刚失败），基础时长为 base * 2^(attempt-1)，封顶 maxDelay。
func bulkRetryDelay(attempt int) time.Duration {
	delay := bulkRetryBaseDelay
	for i := 1; i < attempt && delay < bulkRetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > bulkRetryMaxDelay {
		delay = bulkRetryMaxDelay
	}
	// 抖动：在 [1-ratio, 1+ratio] 区间随机缩放，避免多方重试同步触发限流。
	return time.Duration(float64(delay) * (1 + bulkRetryJitterRatio*(2*rand.Float64()-1)))
}

// bulkAttempt 是一次 bulk HTTP 尝试的响应结果。
// 引入动机：重试需要把"发出一次请求并读完响应体"独立成一步，由调用方根据状态码决定
// 重试还是继续处理；同时在同一个地方关闭响应体，避免重试路径遗漏关闭或重复读取已消费的 body。
type bulkAttempt struct {
	Status      int    // HTTP 状态码
	ContentType string // 响应 Content-Type，用于失败诊断
	Body        []byte // 已读取的响应体（最多 bulkResponseLimit+1 字节，超出即视为响应过大）
	ReadErr     error  // 读取响应体失败的原因；非 nil 时 Body 只含已读到的部分
}

// doBulkAttempt 发起一次 bulk 请求并读取响应体。
//
// 引入动机：BulkIndex 需要对 429/503 重试，而 http.Request 及其 body reader 都不可复用，
// 每次尝试都必须用 bytes.NewReader(payload) 重建请求。本方法统一保证以下两点：
// 每次尝试都使用全新的请求与全新的 body reader，且 resp.Body 在返回前一定被关闭。
//
// 只返回传输层错误（构造请求、发送请求失败）；HTTP 状态码与响应体由 bulkAttempt 承载，
// 是否重试由调用方决定。
func (c *HTTPClient) doBulkAttempt(ctx context.Context, requestURL string, payload []byte) (bulkAttempt, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return bulkAttempt{}, fmt.Errorf("创建 bulk 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return bulkAttempt{}, fmt.Errorf("执行 bulk 请求: %w", err)
	}
	defer resp.Body.Close()

	attempt := bulkAttempt{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
	}
	attempt.Body, attempt.ReadErr = io.ReadAll(io.LimitReader(resp.Body, bulkResponseLimit+1))
	return attempt, nil
}

// BulkIndex 批量索引文档，并在失败时记录足够的请求、响应和 item 级诊断信息。
//
// 引入动机：rebuild 和 index_document job 需要高效批量写入，同时必须能定位 ES 的具体拒绝原因。
//
// 重试语义：ES 返回 429（写入限流）或 503（分片/节点暂时不可用）属于瞬时过载，
// 原实现直接失败会让 job 无谓失败甚至进入 dead。这里对这两种状态码做指数退避 + 抖动重试，
// 最多 bulkRetryMaxAttempts 次尝试；ctx 取消或超时立即中止并把 ctx 错误原样返回；
// 其余非 200（400 维度/mapping 错误、404 索引不存在等）仍保持立即失败，不做重试。
func (c *HTTPClient) BulkIndex(ctx context.Context, indexName string, docs []IndexDoc) error {
	if len(docs) == 0 {
		return nil
	}

	var buf bytes.Buffer
	for _, doc := range docs {
		action := map[string]interface{}{
			"index": map[string]interface{}{"_index": indexName, "_id": doc.ID},
		}
		actionLine, err := json.Marshal(action)
		if err != nil {
			return fmt.Errorf("序列化 bulk action (index=%s, doc_id=%s): %w", indexName, doc.ID, err)
		}
		buf.Write(actionLine)
		buf.WriteByte('\n')

		sourceLine, err := json.Marshal(doc.Body)
		if err != nil {
			return fmt.Errorf("序列化 bulk source (index=%s, doc_id=%s): %w", indexName, doc.ID, err)
		}
		buf.Write(sourceLine)
		buf.WriteByte('\n')
	}

	requestURL := c.baseURL + "/_bulk"
	requestBytes := buf.Len()
	payload := buf.Bytes()
	started := time.Now()

	var (
		attempt  bulkAttempt
		attempts int
	)
	for i := 1; i <= bulkRetryMaxAttempts; i++ {
		attempts = i

		result, err := c.doBulkAttempt(ctx, requestURL, payload)
		if err != nil {
			attrs := c.diagnosticContext(ctx, requestURL, len(docs), requestBytes, started)
			slog.Error("执行 ES bulk 请求失败", append(attrs, "attempt", i, "error", err)...)
			return fmt.Errorf("执行 bulk index (url=%s, documents=%d, request_bytes=%d, attempt=%d, client_timeout=%s): %w", diagnosticURL(requestURL), len(docs), requestBytes, i, c.httpClient.Timeout, err)
		}
		attempt = result

		if attempt.ReadErr != nil {
			// 读取响应体失败属于传输层问题（也可能是 ctx 已结束），重试同一请求没有意义。
			attrs := c.diagnosticContext(ctx, requestURL, len(docs), requestBytes, started)
			attrs = append(attrs, "attempt", i, "status", attempt.Status, "content_type", attempt.ContentType, "body_summary", diagnosticBodySummary(attempt.Body))
			slog.Error("读取 ES bulk 响应失败", append(attrs, "error", attempt.ReadErr)...)
			return fmt.Errorf("读取 bulk 响应 (url=%s, status=%d, attempt=%d, content_type=%q, body=%q): %w", diagnosticURL(requestURL), attempt.Status, i, attempt.ContentType, diagnosticBodySummary(attempt.Body), attempt.ReadErr)
		}

		if !isRetryableBulkStatus(attempt.Status) || i == bulkRetryMaxAttempts {
			// 不可重试的状态码，或可重试但尝试次数已用尽：交给下方统一的失败处理。
			break
		}

		delay := bulkRetryDelay(i)
		attrs := c.diagnosticContext(ctx, requestURL, len(docs), requestBytes, started)
		attrs = append(attrs, "attempt", i, "max_attempts", bulkRetryMaxAttempts, "status", attempt.Status, "retry_delay", delay, "body_summary", diagnosticBodySummary(attempt.Body))
		slog.Warn("ES bulk 写入遇到可重试状态，退避后重试", attrs...)

		select {
		case <-ctx.Done():
			// ctx 取消或超时必须立即返回，并把 ctx 的错误原样带给调用方，
			// 不能伪装成"重试耗尽"，也不能继续等待退避。
			slog.Error("ES bulk 重试等待被取消", append(attrs, "error", ctx.Err())...)
			return fmt.Errorf("bulk index 重试等待被取消 (url=%s, status=%d, attempt=%d, retry_delay=%s): %w", diagnosticURL(requestURL), attempt.Status, i, delay, ctx.Err())
		case <-time.After(delay):
		}
		// 退避结束后的第二次日志：确认重试确实按预期节奏发起，便于观测线上行为。
		slog.Info("ES bulk 开始重试",
			"url", diagnosticURL(requestURL),
			"attempt", i+1,
			"max_attempts", bulkRetryMaxAttempts,
			"previous_status", attempt.Status,
			"retry_delay", delay,
		)
	}

	responseBody := attempt.Body
	attrs := c.diagnosticContext(ctx, requestURL, len(docs), requestBytes, started)
	attrs = append(attrs, "attempts", attempts, "status", attempt.Status, "content_type", attempt.ContentType, "body_summary", diagnosticBodySummary(responseBody))
	if len(responseBody) > bulkResponseLimit {
		err := fmt.Errorf("响应超过 %d 字节限制", bulkResponseLimit)
		slog.Error("ES bulk 响应过大", append(attrs, "error", err)...)
		return fmt.Errorf("读取 bulk 响应 (url=%s, status=%d, attempts=%d): %w", diagnosticURL(requestURL), attempt.Status, attempts, err)
	}
	if attempt.Status != http.StatusOK {
		err := fmt.Errorf("bulk index 返回状态码 %d", attempt.Status)
		if isRetryableBulkStatus(attempt.Status) {
			// 走到这里说明状态码可重试但尝试次数已用尽。
			err = fmt.Errorf("bulk index 返回状态码 %d，重试 %d 次后仍失败", attempt.Status, attempts-1)
			attrs = append(attrs, "retry_exhausted", true)
		}
		slog.Error("ES bulk 请求返回错误状态", append(attrs, "error", err)...)
		return fmt.Errorf("%w (url=%s, attempts=%d, content_type=%q, body=%q)", err, diagnosticURL(requestURL), attempts, attempt.ContentType, diagnosticBodySummary(responseBody))
	}

	var bulkResp struct {
		Errors bool                          `json:"errors"`
		Items  []map[string]bulkItemResponse `json:"items"`
	}
	if err := json.Unmarshal(responseBody, &bulkResp); err != nil {
		slog.Error("解析 ES bulk 响应失败", append(attrs, "error", err)...)
		return fmt.Errorf("解析 bulk 响应 (url=%s, status=%d, content_type=%q, body=%q): %w", diagnosticURL(requestURL), attempt.Status, attempt.ContentType, diagnosticBodySummary(responseBody), err)
	}

	if bulkResp.Errors {
		failed := 0
		details := make([]string, 0, bulkErrorLogLimit)
		for _, item := range bulkResp.Items {
			for operation, result := range item {
				if len(result.Error) == 0 || string(result.Error) == "null" {
					continue
				}
				failed++
				var itemErr struct {
					Type   string `json:"type"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(result.Error, &itemErr); err != nil {
					itemErr.Reason = diagnosticBodySummary(result.Error)
				}
				itemErr.Reason = diagnosticBodySummary([]byte(itemErr.Reason))
				detail := fmt.Sprintf("operation=%s index=%s id=%s status=%d error.type=%s error.reason=%s", operation, result.Index, result.ID, result.Status, itemErr.Type, itemErr.Reason)
				if len(details) < bulkErrorLogLimit {
					details = append(details, detail)
				}
			}
		}
		slog.Error("ES bulk index 部分失败", append(attrs, "index", indexName, "errors", failed, "item_errors", details)...)
		if len(details) == 0 {
			return fmt.Errorf("bulk index 报告 errors=true，但未找到 item 错误详情 (index=%s, documents=%d)", indexName, len(docs))
		}
		return fmt.Errorf("bulk index 有 %d 个文档索引失败: %s", failed, strings.Join(details, "; "))
	}

	slog.Info("ES bulk index 成功", append(attrs, "index", indexName, "attempts", attempts)...)
	return nil
}

// DeleteByQuery 删除匹配 query 的文档。
//
// 幂等行为：当目标索引不存在时，ES 返回 404 且 error.type 为
// "index_not_found_exception"。此时可安全认为无旧 chunk 需要删除，
// 返回 nil（幂等成功）。其他 404 场景或非 200 状态码仍返回错误。
//
// 引入动机：index_document job 首次执行时目标索引可能尚未创建，
// delete_by_query 在不存在的索引上返回 404，原实现将其当作致命错误
// 导致作业 dead。按设计，索引不存在即无旧数据，应幂等成功。
func (c *HTTPClient) DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error {
	body, err := json.Marshal(map[string]interface{}{
		"query": query,
	})
	if err != nil {
		return fmt.Errorf("序列化 delete_by_query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+indexName+"/_delete_by_query", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("创建 delete_by_query 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("执行 delete_by_query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// 404 时解析 ES 错误体，仅对 index_not_found_exception 幂等成功
	if resp.StatusCode == http.StatusNotFound {
		var errBody struct {
			Error struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
				Index  string `json:"index"`
			} `json:"error"`
		}
		if decodeErr := json.NewDecoder(resp.Body).Decode(&errBody); decodeErr != nil {
			slog.Warn("delete_by_query 404 响应体解析失败，按通用 404 错误处理",
				"index", indexName, "decode_error", decodeErr)
		}

		if errBody.Error.Type == "index_not_found_exception" {
			slog.Info("delete_by_query 目标索引不存在，幂等成功",
				"index", indexName,
				"reason", errBody.Error.Reason)
			return nil
		}

		// 其他 404 类型仍为错误，记录安全日志（不含 secret）
		slog.Error("delete_by_query 返回 404 但非 index_not_found",
			"index", indexName,
			"error_type", errBody.Error.Type,
			"reason", errBody.Error.Reason)
		return fmt.Errorf("delete_by_query 返回 404，错误类型: %s", errBody.Error.Type)
	}

	// 其他非 200 状态码仍为错误
	return fmt.Errorf("delete_by_query 返回状态码 %d", resp.StatusCode)
}

// Search 执行搜索查询。
//
// 失败诊断：ES 在非 200 时会在响应体给出 error.type/error.reason（例如 dense_vector
// 维度不匹配返回 400 illegal_argument_exception）。只报告状态码无法定位线上问题，
// 因此这里读取（有上限的）错误响应体，把 ES 的真实原因带进 error 与 slog 日志。
func (c *HTTPClient) Search(ctx context.Context, indexName string, query map[string]interface{}) (*SearchResponse, error) {
	body, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("序列化 search query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+indexName+"/_search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建 search 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("执行 search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 有上限地读取错误体：ES 错误原因足以在 diagnosticBodyLimit 内表达，
		// 避免异常巨大的响应体进入内存或日志。
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, diagnosticBodyLimit))
		if readErr != nil {
			slog.Error("ES search 返回错误状态且读取错误响应体失败",
				"index", indexName,
				"status", resp.StatusCode,
				"error", readErr,
				"body_summary", diagnosticBodySummary(errBody),
			)
			return nil, fmt.Errorf("search 返回状态码 %d（读取错误响应体失败: %v）", resp.StatusCode, readErr)
		}

		errorType, reason, ok := parseESErrorBody(errBody)
		logAttrs := []any{
			"index", indexName,
			"status", resp.StatusCode,
			"error_type", errorType,
			"error_reason", reason,
		}
		if !ok {
			// 非 ES 标准错误体（如网关 HTML 错误页、error 为字符串）：退化为截断摘要。
			logAttrs = append(logAttrs, "body_summary", diagnosticBodySummary(errBody))
		}
		slog.Error("ES search 返回错误状态", logAttrs...)

		if !ok {
			return nil, fmt.Errorf("search 返回状态码 %d: %s", resp.StatusCode, diagnosticBodySummary(errBody))
		}
		return nil, fmt.Errorf("search 返回状态码 %d: %s", resp.StatusCode, esErrorDetail(errorType, reason))
	}

	var result SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 search 响应: %w", err)
	}

	return &result, nil
}

// Count 返回匹配 query 的文档数量。
func (c *HTTPClient) Count(ctx context.Context, indexName string, query map[string]interface{}) (int64, error) {
	body := map[string]interface{}{}
	if query != nil {
		body["query"] = query
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("序列化 count query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+indexName+"/_count", bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("创建 count 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("执行 count: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("count 返回状态码 %d", resp.StatusCode)
	}

	var result struct {
		Count int64 `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("解析 count 响应: %w", err)
	}

	return result.Count, nil
}

// GetDocument 根据 ID 获取单个文档。
func (c *HTTPClient) GetDocument(ctx context.Context, indexName, docID string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+indexName+"/_doc/"+docID, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 get document 请求: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取 document %s: %w", docID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get document 返回状态码 %d", resp.StatusCode)
	}

	var result struct {
		Found  bool                   `json:"found"`
		Source map[string]interface{} `json:"_source"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 get document 响应: %w", err)
	}

	if !result.Found {
		return nil, nil
	}
	return result.Source, nil
}

// Refresh 刷新索引使最近写入的文档可搜索。
func (c *HTTPClient) Refresh(ctx context.Context, indexName string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+indexName+"/_refresh", nil)
	if err != nil {
		return fmt.Errorf("创建 refresh 请求: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("执行 refresh: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("refresh 返回状态码 %d", resp.StatusCode)
	}

	return nil
}

// ListIndices 列出匹配 pattern 的所有索引名称。
// 引入动机：cleanup_old_indexes job 需要列出所有 knowledge_v* 索引，
// 以识别可清理的旧版本索引并排除当前 alias 指向的索引。
func (c *HTTPClient) ListIndices(ctx context.Context, pattern string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/_cat/indices/"+pattern+"?format=json&h=index", nil)
	if err != nil {
		return nil, fmt.Errorf("创建 list indices 请求: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("列出 indices: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list indices 返回状态码 %d", resp.StatusCode)
	}

	var result []struct {
		Index string `json:"index"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析 list indices 响应: %w", err)
	}

	indices := make([]string, 0, len(result))
	for _, r := range result {
		indices = append(indices, r.Index)
	}
	return indices, nil
}

// GenerateIndexName 根据 profile 名称和版本号生成 ES 索引名称。
// 引入动机：design/01-SEARCH.md §Index Version 要求索引命名为 knowledge_v{n}。
// 使用 profile 的版本号作为索引版本号。
func GenerateIndexName(profileVersion int) string {
	return fmt.Sprintf("%s%d", IndexNamePrefix, profileVersion)
}

// NextIndexName 返回下一个可用的版本化索引名（如 knowledge_v4）。
// 引入动机：当既有索引的 embedding 维度与当前 profile 不一致时，需要一个新索引名来承载
// 正确维度的 mapping，避免复用维度不可变的旧索引。基于 ES 中现有 knowledge_v* 索引的最大
// 版本号 +1，保证与现有索引不冲突；索引集为空时返回 knowledge_v1。
//
// 非 knowledge_v{n} 形式的名字（如 knowledge_current 或 knowledge_v1_backup）会被忽略，
// 不参与最大版本号计算。
func NextIndexName(ctx context.Context, client Client) (string, error) {
	indices, err := client.ListIndices(ctx, IndexNamePrefix+"*")
	if err != nil {
		return "", fmt.Errorf("列出 %s* 索引以计算下一个索引名: %w", IndexNamePrefix, err)
	}

	maxVersion := 0
	for _, name := range indices {
		version, ok := parseIndexVersion(name)
		if !ok {
			continue
		}
		if version > maxVersion {
			maxVersion = version
		}
	}

	return GenerateIndexName(maxVersion + 1), nil
}

// parseIndexVersion 从索引名解析版本号。
// 引入动机：NextIndexName 需要忽略非 knowledge_v{n} 形式的名字（如 knowledge_current）。
// 返回 (version, true) 表示解析成功。
//
// 只接受 knowledge_v 前缀后紧跟至少一位纯十进制数字的名字，
// 因此 knowledge_v、knowledge_v1_backup、knowledge_v-1 都视为解析失败。
func parseIndexVersion(name string) (int, bool) {
	suffix, hasPrefix := strings.CutPrefix(name, IndexNamePrefix)
	if !hasPrefix || suffix == "" {
		return 0, false
	}
	for _, char := range suffix {
		if char < '0' || char > '9' {
			return 0, false
		}
	}

	version, err := strconv.Atoi(suffix)
	if err != nil {
		// 全数字后缀仍转换失败只可能是整数溢出，不能静默当作有效版本号。
		slog.Error("索引名版本号超出整数范围，忽略该索引", "index", name, "error", err)
		return 0, false
	}
	return version, true
}

// BuildIndexMapping 根据 Search Profile 配置构建 ES index mapping。
// 引入动机：不同 profile 可能有不同的 dimensions 和 analyzer 配置，
// 需要动态生成 mapping。
//
// path 字段同时支持 BM25 text 检索和精确 keyword 过滤（H3）：
// text 子字段用于全文搜索，keyword 子字段用于精确匹配和排序。
// section_path 字段保持 text 类型用于 BM25 检索，同时保留原始层级结构。
func BuildIndexMapping(dimensions int, analyzer string) map[string]interface{} {
	return map[string]interface{}{
		"settings": map[string]interface{}{
			"index": map[string]interface{}{
				"number_of_shards":   1,
				"number_of_replicas": 0,
			},
			"analysis": map[string]interface{}{
				"analyzer": map[string]interface{}{
					"default": map[string]interface{}{
						"type": analyzer,
					},
				},
			},
		},
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"document_id":  map[string]interface{}{"type": "keyword"},
				"workspace_id": map[string]interface{}{"type": "keyword"},
				// path 同时支持 BM25 text 检索和精确 keyword 过滤（H3）
				"path": map[string]interface{}{
					"type":     "text",
					"analyzer": analyzer,
					"fields": map[string]interface{}{
						"keyword": map[string]interface{}{
							"type": "keyword",
						},
					},
				},
				"title":        map[string]interface{}{"type": "text", "analyzer": analyzer},
				"heading":      map[string]interface{}{"type": "text", "analyzer": analyzer},
				"section_path": map[string]interface{}{"type": "text", "analyzer": analyzer},
				"content":      map[string]interface{}{"type": "text", "analyzer": analyzer},
				"start_line":   map[string]interface{}{"type": "integer"},
				"end_line":     map[string]interface{}{"type": "integer"},
				"chunk_index":  map[string]interface{}{"type": "integer"},
				"content_hash": map[string]interface{}{"type": "keyword"},
				"revision":     map[string]interface{}{"type": "integer"},
				"status":       map[string]interface{}{"type": "keyword"},
				"is_special":   map[string]interface{}{"type": "boolean"},
				"embedding": map[string]interface{}{
					"type":       "dense_vector",
					"dims":       dimensions,
					"index":      true,
					"similarity": "cosine",
				},
			},
		},
	}
}

// SwitchAlias 原子性地将 alias 从旧索引切换到新索引。
//
// 引入动机：design/01-SEARCH.md §Index Version 要求 alias 原子切换，
// 保留旧索引以支持 rollback。
//
// 处理三种场景：
//  1. alias 已存在并指向旧索引：原子 remove old + add new。
//  2. alias 不存在且无同名具体索引：直接 add 创建 alias。
//  3. alias 不存在但存在同名具体索引（如 ES 自动创建了 knowledge_current 索引）：
//     先删除同名具体索引，再创建 alias。此场景发生在 index_document job
//     直接向 alias 名称写入数据时 ES 自动创建了具体索引。
//
// 安全约束：删除同名具体索引前确认它不是 newIndex 本身，
// 避免删除正在构建的新索引。删除后立即创建 alias，窗口极小。
func SwitchAlias(ctx context.Context, client Client, alias, newIndex string) error {
	// 尝试获取当前 alias 指向的索引
	oldIndex, err := client.GetAliasIndex(ctx, alias)
	if err == nil {
		// alias 已存在，原子切换：remove old + add new
		actions := []AliasAction{
			{Action: "remove", Index: oldIndex, Alias: alias},
			{Action: "add", Index: newIndex, Alias: alias},
		}
		if err := client.UpdateAlias(ctx, actions); err != nil {
			slog.Error("alias 原子切换失败", "old_index", oldIndex, "new_index", newIndex, "error", err)
			return fmt.Errorf("alias 切换失败: %w", err)
		}
		slog.Info("alias 原子切换成功", "old_index", oldIndex, "new_index", newIndex)
		return nil
	}

	// alias 不存在，检查是否存在同名具体索引
	exists, existsErr := client.IndexExists(ctx, alias)
	if existsErr != nil {
		return fmt.Errorf("检查同名索引 %s 是否存在: %w", alias, existsErr)
	}
	if exists {
		// 场景 3：存在同名具体索引（如 ES 自动创建的 knowledge_current），
		// 需要先删除才能创建同名 alias。
		if alias == newIndex {
			// 极端情况：newIndex 与 alias 同名，不能删除自己
			return fmt.Errorf("alias %s 不存在但同名具体索引即为目标索引，无法创建 alias", alias)
		}
		slog.Warn("发现同名具体索引，先删除再创建 alias", "index", alias, "new_index", newIndex)
		if err := client.DeleteIndex(ctx, alias); err != nil {
			return fmt.Errorf("删除同名具体索引 %s 以创建 alias: %w", alias, err)
		}
	}

	// 创建 alias 指向新索引
	actions := []AliasAction{
		{Action: "add", Index: newIndex, Alias: alias},
	}
	if err := client.UpdateAlias(ctx, actions); err != nil {
		return fmt.Errorf("创建 alias %s → %s: %w", alias, newIndex, err)
	}

	slog.Info("alias 创建成功", "new_index", newIndex, "alias", alias)
	return nil
}
