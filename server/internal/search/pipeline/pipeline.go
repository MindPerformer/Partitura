// Package pipeline 实现完整的搜索管线：
// query → BM25 multi-field + dense vector → RRF → top candidates → reranker → diversification → final results。
//
// 引入动机：design/01-SEARCH.md §目标 定义了搜索管线的完整流程。
// design/04-WEB-API.md §Search 要求 ES query 只能由服务器生成，客户端不能自定义 workspace filter。
//
// 设计原则：
//   - 搜索仅当前 Workspace，服务端生成固定 workspace filter
//   - ES 不可用时返回 degraded 响应
//   - Embedding 不可用时保持 lexical 检索路径
//   - Reranker 不可用时直接返回 RRF 结果
//   - 不出现 XxxService/XxxManager/XxxController
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"partitura/server/internal/es"
	"partitura/server/internal/search/chunking"
	"partitura/server/internal/search/diversity"
	"partitura/server/internal/search/embedding"
	"partitura/server/internal/search/reranker"
	"partitura/server/internal/search/rrf"
	types "partitura/server/internal/search/types"
)

// Pipeline 是搜索管线的执行器。
// 引入动机：将搜索管线的各阶段（BM25、vector、RRF、reranker、diversity）编排在一起，
// 由 Search Profile 配置驱动。
type Pipeline struct {
	esClient          es.Client
	embeddingProvider embedding.Provider
	rerankerProvider  reranker.Provider
	// embeddingProviderFn 是动态获取 Embedding Provider 的函数。
	// 引入动机：计划要求搜索管线从 Provider Registry 获取当前实例，
	// 支持热更新后无需重启。若此函数非 nil，优先使用它获取 Provider。
	embeddingProviderFn func() embedding.Provider
	// rerankerProviderFn 是动态获取 Reranker Provider 的函数。
	// 引入动机：同上，支持从 Registry 热加载。
	rerankerProviderFn func() reranker.Provider
}

// NewPipeline 创建搜索管线执行器。
func NewPipeline(esClient es.Client, embProvider embedding.Provider, rrProvider reranker.Provider) *Pipeline {
	return &Pipeline{
		esClient:          esClient,
		embeddingProvider: embProvider,
		rerankerProvider:  rrProvider,
	}
}

// NewPipelineWithRegistry 创建支持 Provider 热更新的搜索管线执行器。
// 引入动机：计划要求搜索管线从 Provider Registry 获取当前实例，
// 管理员保存 Provider 配置后无需重启服务。
// 传入的函数每次调用返回当前 Provider 实例（可能为 nil）。
func NewPipelineWithRegistry(esClient es.Client, embFn func() embedding.Provider, rrFn func() reranker.Provider) *Pipeline {
	return &Pipeline{
		esClient:            esClient,
		embeddingProviderFn: embFn,
		rerankerProviderFn:  rrFn,
	}
}

// currentEmbeddingProvider 返回当前有效的 Embedding Provider。
// 引入动机：优先从 Registry 函数获取（支持热更新），回退到静态实例。
func (p *Pipeline) currentEmbeddingProvider() embedding.Provider {
	if p.embeddingProviderFn != nil {
		return p.embeddingProviderFn()
	}
	return p.embeddingProvider
}

// currentRerankerProvider 返回当前有效的 Reranker Provider。
// 引入动机：同上，优先从 Registry 函数获取。
func (p *Pipeline) currentRerankerProvider() reranker.Provider {
	if p.rerankerProviderFn != nil {
		return p.rerankerProviderFn()
	}
	return p.rerankerProvider
}

// SearchInput 是搜索管线的输入参数。
type SearchInput struct {
	// WorkspaceID 是当前 workspace 的 UUID（服务端生成，客户端不能定义或绕过）。
	WorkspaceID string
	// Query 是搜索查询文本。
	Query string
	// Profile 是当前 active 的 Search Profile 配置。
	Profile types.SearchProfileConfig
	// Limit 是返回结果的最大数量。
	Limit int
	// Offset 是分页偏移量。
	Offset int
	// Mode 控制搜索管线行为："hybrid"（默认）、"lexical"、"semantic"。
	// 引入动机：M3 要求 search request 支持 mode 参数，允许客户端指定检索模式。
	Mode string
	// IndexName 是本次检索的目标索引名，留空表示由 Profile.ESIndexName 决定。
	//
	// 引入动机：design/01-SEARCH.md §Index Version 要求索引必须 versioned，
	// 并通过 alias knowledge_current 做原子切换，禁止原地破坏 active index。
	// 两类调用方对目标索引的需求不同：
	//   - 普通搜索（handler）必须打 alias，否则 Profile.ESIndexName 与实际索引一旦不一致
	//     （手动重建换索引名、换 alias、改 profile），ES 会返回 400/404，搜索直接降级；
	//   - 评测/调优（evaluate_profile、optimize_profile）针对的是候选 profile 自己的
	//     具体索引，该索引通常不是 alias 当前指向的索引，因此必须打具体索引名。
	// 由调用方显式指定目标索引，避免在管线内部无条件使用 alias 而破坏评测。
	IndexName string
}

// SearchOutput 是搜索管线的输出结果。
type SearchOutput struct {
	Results           []types.SearchResult
	Total             int
	Degraded          bool
	DegradationReason string
	RerankerUsed      bool
	RerankerCost      float64
	SearchID          string
	LatencyMs         int
}

// Search 执行完整的搜索管线。
//
// 引入动机：design/01-SEARCH.md §目标 定义搜索流程：
// query → BM25 multi-field + dense vector → RRF → top candidates → reranker → diversification → final results
//
// 降级行为：
//   - ES 不可用：返回 degraded 响应
//   - Embedding 不可用：仅使用 BM25 lexical 检索
//   - Reranker 不可用：直接返回 RRF 结果
func (p *Pipeline) Search(ctx context.Context, input SearchInput) (*SearchOutput, error) {
	startTime := time.Now()
	searchID := generateSearchID()

	output := &SearchOutput{
		SearchID: searchID,
	}

	if input.Limit <= 0 {
		input.Limit = 10
	}
	if input.Limit > 100 {
		input.Limit = 100
	}

	// 检查 ES 是否可用
	if p.esClient == nil {
		output.Degraded = true
		output.DegradationReason = "elasticsearch_unavailable"
		return output, nil
	}

	if err := p.esClient.Ping(ctx); err != nil {
		slog.Warn("ES 不可用，搜索降级", "error", err)
		output.Degraded = true
		output.DegradationReason = "elasticsearch_unavailable"
		return output, nil
	}

	// 目标索引解析顺序：显式 IndexName → Profile.ESIndexName → es.AliasName。
	// 引入动机：design/01-SEARCH.md §Index Version 要求索引 versioned 且通过 alias
	// knowledge_current 原子切换。普通搜索由 handler 显式传入 alias；评测路径不传
	// IndexName，保持打候选 profile 的具体索引名，管线本身不强制使用 alias。
	indexName := input.IndexName
	if indexName == "" {
		indexName = input.Profile.ESIndexName
	}
	if indexName == "" {
		indexName = es.AliasName
	}

	// 服务端生成 workspace filter，客户端不能定义或绕过
	// C6：排除 archived 文档和特殊文件，确保搜索结果只包含 active 非特殊文档
	workspaceFilter := map[string]interface{}{
		"bool": map[string]interface{}{
			"filter": []interface{}{
				map[string]interface{}{
					"term": map[string]interface{}{
						"workspace_id": input.WorkspaceID,
					},
				},
				map[string]interface{}{
					"term": map[string]interface{}{
						"is_special": false,
					},
				},
				map[string]interface{}{
					"bool": map[string]interface{}{
						"must_not": map[string]interface{}{
							"term": map[string]interface{}{
								"status": "archived",
							},
						},
					},
				},
			},
		},
	}

	// 确定搜索模式（M3）
	// 默认 hybrid：BM25 + vector → RRF
	// lexical：仅 BM25
	// semantic：仅 vector
	mode := input.Mode
	if mode == "" {
		mode = "hybrid"
	}

	// --- 阶段 1: BM25 multi-field 检索 ---
	var bm25Results []types.CandidateResult
	if mode == "hybrid" || mode == "lexical" {
		var bm25Err error
		bm25Results, bm25Err = p.searchBM25(ctx, indexName, input, workspaceFilter)
		if bm25Err != nil {
			slog.Warn("BM25 检索失败", "error", bm25Err)
			bm25Results = nil
		}
	}

	// --- 阶段 2: Dense vector 检索 ---
	embProvider := p.currentEmbeddingProvider()
	var vectorResults []types.CandidateResult
	if mode == "hybrid" || mode == "semantic" {
		if embProvider == nil {
			// 未配置 embedding provider 时，保持 lexical-only 降级语义。
			output.Degraded = true
			output.DegradationReason = "embedding_unavailable"
		} else {
			// 直接调用 Embed。Available(ctx) 通常会发起一次健康检查请求，
			// 再调用 Embed 会让同一次搜索重复访问模型 API；Embed 失败时统一降级。
			queryInput := chunking.BuildQueryEmbeddingInput(input.Query, input.Profile.EmbeddingQueryInstruction)
			queryVec, embErr := embProvider.Embed(ctx, []string{queryInput})
			if embErr != nil || len(queryVec) == 0 {
				slog.Warn("embedding 不可用，降级为 lexical-only", "error", embErr)
				output.Degraded = true
				output.DegradationReason = "embedding_unavailable"
			} else {
				// 修复说明：原实现忽略 searchVector 返回错误，ES knn 查询失败（如 400 参数错误）
				// 时静默产生 0 命中且 degraded=false。现在记录错误：semantic 模式直接降级失败；
				// hybrid 模式降级为 lexical-only 检索，避免无声空结果。
				var vecErr error
				vectorResults, vecErr = p.searchVector(ctx, indexName, input, workspaceFilter, queryVec[0])
				if vecErr != nil {
					slog.Warn("vector 检索失败", "error", vecErr, "mode", mode)
					output.Degraded = true
					output.DegradationReason = "vector_search_failed"
					if mode == "semantic" {
						// semantic 仅依赖向量检索，失败即无可返回结果
						output.LatencyMs = int(time.Since(startTime).Milliseconds())
						return output, nil
					}
					vectorResults = nil
				}
			}
		}
	}

	// --- 阶段 3: RRF 融合 ---
	candidates := rrf.Fuse(bm25Results, vectorResults, input.Profile.RRFK, input.Profile.RerankerCandidateCount)

	if len(candidates) == 0 {
		output.LatencyMs = int(time.Since(startTime).Milliseconds())
		return output, nil
	}

	// --- 阶段 4: Reranker ---
	// 仅在 reranker 可用且候选数 > 1 时执行；reranker 只改变排序、不改变结果集。
	// 真实第三方 reranker 可能瞬时超时/不可用（Nginx proxy_read_timeout 60s 内未返回即 504），
	// 此处统一视为降级提示并以 RRF 顺序直接返回，避免整个搜索请求被拖至 504。
	rrProvider := p.currentRerankerProvider()
	var finalResults []types.SearchResult
	if rrProvider != nil && len(candidates) > 1 {
		// 直接调用 Rerank。Available(ctx) 会额外触发一次健康检查请求，
		// 调用失败由现有降级逻辑处理，避免同一次搜索重复访问模型 API。
		rerankerCandidates := make([]types.RerankerCandidate, len(candidates))
		for i, c := range candidates {
			rerankerCandidates[i] = types.RerankerCandidate{
				Text:  c.Chunk.Content,
				Index: i,
			}
		}

		reranked, err := rrProvider.Rerank(ctx, input.Query, rerankerCandidates)
		if err != nil {
			slog.Warn("reranker 不可用，降级为 RRF 结果", "error", err)
			output.Degraded = true
			if output.DegradationReason == "" {
				output.DegradationReason = "reranker_unavailable"
			}
			// 直接使用 RRF 结果
			finalResults = candidatesToResults(candidates)
		} else {
			output.RerankerUsed = true
			// 按 reranker 分数排序
			finalResults = rerankedToResults(candidates, reranked)
		}
	} else {
		// 无 reranker 或候选不足，直接使用 RRF 结果。
		// 候选不足时不调用 provider，也不将其视为 provider 不可用。
		finalResults = candidatesToResults(candidates)
	}

	// --- 阶段 5: Document Diversification ---
	finalResults = diversity.Apply(finalResults, input.Profile.MaxChunksPerDocument, input.Profile.MergeAdjacentChunks)

	// --- 分页 ---
	output.Total = len(finalResults)
	if input.Offset >= len(finalResults) {
		finalResults = nil
	} else {
		end := input.Offset + input.Limit
		if end > len(finalResults) {
			end = len(finalResults)
		}
		finalResults = finalResults[input.Offset:end]
	}

	output.Results = finalResults
	output.LatencyMs = int(time.Since(startTime).Milliseconds())

	return output, nil
}

// searchBM25 执行 BM25 multi-field 检索。
// 引入动机：design/01-SEARCH.md 要求 BM25 multi-field 搜索，
// field boost 受 Search Profile 控制。
func (p *Pipeline) searchBM25(ctx context.Context, indexName string, input SearchInput, workspaceFilter map[string]interface{}) ([]types.CandidateResult, error) {
	profile := input.Profile

	query := map[string]interface{}{
		"size": profile.LexicalTopK,
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"filter": []interface{}{workspaceFilter},
				"must": []interface{}{
					map[string]interface{}{
						"multi_match": map[string]interface{}{
							"query": input.Query,
							"fields": []interface{}{
								fmt.Sprintf("title^%g", profile.TitleBoost),
								fmt.Sprintf("heading^%g", profile.HeadingBoost),
								fmt.Sprintf("path^%g", profile.PathBoost),
								fmt.Sprintf("section_path^%g", profile.HeadingBoost),
								fmt.Sprintf("content^%g", profile.BodyBoost),
							},
						},
					},
				},
			},
		},
	}

	resp, err := p.esClient.Search(ctx, indexName, query)
	if err != nil {
		return nil, fmt.Errorf("BM25 搜索: %w", err)
	}

	results := make([]types.CandidateResult, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		chunk := hitToChunk(hit.Source)
		chunk.WorkspaceID = input.WorkspaceID
		results = append(results, types.CandidateResult{
			Chunk:     chunk,
			BM25Score: hit.Score,
		})
	}

	return results, nil
}

// searchVector 执行 dense vector 检索。
// 引入动机：design/01-SEARCH.md 要求 dense vector search。
func (p *Pipeline) searchVector(ctx context.Context, indexName string, input SearchInput, workspaceFilter map[string]interface{}, queryVec []float32) ([]types.CandidateResult, error) {
	profile := input.Profile

	// 构建 KNN 查询（ES 8.17 knn query 语法：field/query_vector/k/num_candidates，可嵌套在 bool.must）
	// 修复说明：原实现误用 "embedding"/"vector" 作为参数名，ES 返回 400，错误被调用方忽略后
	// 静默产生 0 命中且 degraded=false。修正为 knn query 规范字段名。
	query := map[string]interface{}{
		"size": profile.VectorTopK,
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"filter": []interface{}{workspaceFilter},
				"must": []interface{}{
					map[string]interface{}{
						"knn": map[string]interface{}{
							"field":          "embedding",
							"query_vector":   queryVec,
							"k":              profile.VectorTopK,
							"num_candidates": profile.VectorTopK * 2,
						},
					},
				},
			},
		},
	}

	resp, err := p.esClient.Search(ctx, indexName, query)
	if err != nil {
		return nil, fmt.Errorf("vector 搜索: %w", err)
	}

	results := make([]types.CandidateResult, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		chunk := hitToChunk(hit.Source)
		chunk.WorkspaceID = input.WorkspaceID
		results = append(results, types.CandidateResult{
			Chunk:       chunk,
			VectorScore: hit.Score,
		})
	}

	return results, nil
}

// hitToChunk 从 ES hit 的 _source 构建 Chunk。
func hitToChunk(source map[string]interface{}) types.Chunk {
	chunk := types.Chunk{
		DocumentID:  getString(source, "document_id"),
		Path:        getString(source, "path"),
		Title:       getString(source, "title"),
		Heading:     getString(source, "heading"),
		Content:     getString(source, "content"),
		ContentHash: getString(source, "content_hash"),
		Status:      getString(source, "status"),
	}
	chunk.StartLine = getInt(source, "start_line")
	chunk.EndLine = getInt(source, "end_line")
	chunk.ChunkIndex = getInt(source, "chunk_index")
	chunk.Revision = getInt(source, "revision")

	if sp, ok := source["section_path"].([]interface{}); ok && len(sp) > 0 {
		chunk.SectionPath = make([]string, 0, len(sp))
		for _, v := range sp {
			if s, ok := v.(string); ok && s != "" {
				chunk.SectionPath = append(chunk.SectionPath, s)
			}
		}
	} else if spStr, ok := source["section_path"].(string); ok && spStr != "" {
		// 兼容 ES 可能返回字符串的情况
		chunk.SectionPath = []string{spStr}
	}
	if isSpecial, ok := source["is_special"].(bool); ok {
		chunk.IsSpecial = isSpecial
	}

	return chunk
}

// getString 从 map 安全获取字符串值。
func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// getInt 从 map 安全获取整数值。
func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	if v, ok := m[key].(int); ok {
		return v
	}
	return 0
}

// candidatesToResults 将候选结果转换为搜索结果。
func candidatesToResults(candidates []types.CandidateResult) []types.SearchResult {
	results := make([]types.SearchResult, len(candidates))
	for i, c := range candidates {
		results[i] = types.SearchResult{
			DocumentID:  c.Chunk.DocumentID,
			Path:        c.Chunk.Path,
			Title:       c.Chunk.Title,
			SectionPath: c.Chunk.SectionPath,
			StartLine:   c.Chunk.StartLine,
			EndLine:     c.Chunk.EndLine,
			Snippet:     truncate(c.Chunk.Content, 200),
			Score:       c.RRFScore,
			Revision:    c.Chunk.Revision,
			Rank:        i + 1,
		}
	}
	return results
}

// rerankedToResults 将 reranker 结果转换为搜索结果。
func rerankedToResults(candidates []types.CandidateResult, reranked []types.RerankerResult) []types.SearchResult {
	results := make([]types.SearchResult, len(reranked))
	for i, rr := range reranked {
		if rr.Index < 0 || rr.Index >= len(candidates) {
			continue
		}
		c := candidates[rr.Index]
		results[i] = types.SearchResult{
			DocumentID:  c.Chunk.DocumentID,
			Path:        c.Chunk.Path,
			Title:       c.Chunk.Title,
			SectionPath: c.Chunk.SectionPath,
			StartLine:   c.Chunk.StartLine,
			EndLine:     c.Chunk.EndLine,
			Snippet:     truncate(c.Chunk.Content, 200),
			Score:       rr.Score,
			Revision:    c.Chunk.Revision,
			Rank:        i + 1,
		}
	}
	return results
}

// truncate 截断文本到指定长度。
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// generateSearchID 生成搜索唯一标识。
func generateSearchID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}
