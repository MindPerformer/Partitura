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
	"sort"
	"strings"
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
	// TruncatedByScore 是因归一化分数过低而被裁剪掉的结果数。
	// 引入动机：归一化裁剪会把尾部低相关噪声从结果集中移除，
	// 该计数让"裁剪行为"可观测，便于调用方与运维区分
	// "无命中"与"命中但被低分过滤"。
	TruncatedByScore int
}

// Search 执行完整的搜索管线。
//
// 引入动机：design/01-SEARCH.md §目标 定义搜索流程：
// query → BM25 multi-field + dense vector → RRF → top candidates → reranker → diversification → final results
//
// 降级行为：
//   - ES 不可用：返回 degraded 响应
//   - Embedding 不可用：仅使用 BM25 lexical 检索
//   - vector 检索失败：按 DegradationReason 细分为 vector_dimension_mismatch
//     （索引维度与查询向量维度不一致）、vector_field_missing（索引无 embedding 字段）
//     与 vector_search_failed（维度读取失败或原因不明）
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
				//
				// 细分降级原因（线上事故修复说明）：原先所有 vector 失败都被统一归因为
				// vector_search_failed，无法与"ES 抖动"区分；线上真实原因是索引 embedding 维度与
				// 查询向量维度不一致（索引 dims=1024、查询向量 4096）。此处结合索引 mapping 的
				// 真实维度做分类，分类只补充诊断信息，原始 vecErr 始终原样写入日志，
				// 不被分类结果替换、掩盖或吞掉。
				var vecErr error
				vectorResults, vecErr = p.searchVector(ctx, indexName, input, workspaceFilter, queryVec[0])
				if vecErr != nil {
					queryDims := len(queryVec[0])
					reason, indexDims, dimsErr := p.classifyVectorFailure(ctx, indexName, queryDims)
					logArgs := []any{
						"error", vecErr,
						"mode", mode,
						"index", indexName,
						"index_dims", indexDims,
						"query_dims", queryDims,
						"reason", reason,
					}
					if dimsErr != nil {
						// 维度读取自身失败也必须留痕：此时分类已退化为 vector_search_failed，
						// 若再静默丢弃该错误，排查现场将无任何可用线索。
						logArgs = append(logArgs, "index_dims_error", dimsErr)
					}
					slog.Warn("vector 检索失败", logArgs...)
					output.Degraded = true
					output.DegradationReason = reason
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

	// --- 阶段 6: 文档级聚合 ---
	// 引入动机：搜索改为"一个文档一条主结果"，避免同文档多 chunk 重复挤占
	// 结果位、重复输出 path/title；limit 自此作用于"文档数"而非"chunk 数"。
	// 聚合在 diversity 之后进行：diversity 先做 chunk 级多样性/相邻合并，
	// groupByDocument 再把每个文档折叠为单条主结果（MatchedChunks/OtherRanges）。
	finalResults = groupByDocument(finalResults)

	// --- 阶段 7: Score 归一化 + 低分裁剪 ---
	// 引入动机：RRF 融合分数是 ~0.0X 的不可读小数，归一化到 0~1 后模型可读；
	// 归一化后低于 minNormalizedScore 的尾部结果多为噪声，直接丢弃以控制输出质量。
	// 裁剪数量写入 output.TruncatedByScore 供响应透传与可观测。
	normalizedResults, truncatedByScore := normalizeAndTrimByScore(finalResults, output.RerankerUsed)
	finalResults = normalizedResults
	output.TruncatedByScore = truncatedByScore

	// --- 分页 ---
	// 注意：Total 采用裁剪后（文档级聚合后）的结果数，与"一个文档一条"语义一致。
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

// classifyVectorFailure 细分 vector 检索失败的降级原因。
//
// 引入动机：线上事故中 vector 检索失败被统一归因为 vector_search_failed，
// 无法与 ES 抖动区分，真正原因（查询向量维度与索引 embedding 维度不一致，
// 例如索引 dims=1024 而查询向量 4096）在管线层完全看不出来。此处结合索引 mapping 的
// 真实 embedding 维度与查询向量维度进行分类：
//
//   - dims > 0 且 dims != queryDims → "vector_dimension_mismatch"
//     （索引维度与查询向量维度不一致，例如索引 dims=1024 而查询向量 4096）
//   - dims == 0 → "vector_field_missing"
//     （索引不存在或没有 dense_vector 的 embedding 字段，与 es.Client.GetIndexDimensions 语义一致）
//   - 读取维度报错，或 dims == queryDims（维度一致但检索仍失败，原因不明）→ "vector_search_failed"
//
// 返回值说明：
//   - reason 是最终写入 SearchOutput.DegradationReason 的降级原因；
//   - indexDims 是读取到的索引维度（读取失败时为 0），仅供调用方写入日志；
//   - dimsErr 是 GetIndexDimensions 的原始错误，返回给调用方记录日志，绝不静默忽略。
//
// 本函数只产出诊断分类，不吞掉、不替换、不包装原始的 vector 检索错误；
// 原始错误由调用方原样记录。
func (p *Pipeline) classifyVectorFailure(ctx context.Context, indexName string, queryDims int) (reason string, indexDims int, dimsErr error) {
	dims, err := p.esClient.GetIndexDimensions(ctx, indexName)
	if err != nil {
		// 维度读取失败时无法判定具体原因，保持既有的笼统归因。
		return "vector_search_failed", 0, err
	}

	switch {
	case dims == 0:
		return "vector_field_missing", dims, nil
	case dims != queryDims:
		return "vector_dimension_mismatch", dims, nil
	default:
		return "vector_search_failed", dims, nil
	}
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
		// 请求 ES 高亮片段，用于 buildSnippet 生成"命中点可见"的 snippet。
		// 引入动机：原实现 snippet 永远截断 chunk 开头，与命中位置无关，
		// 模型只能看到开头套话。请求 content/heading/title 的 fragment 后，
		// snippet 可定位到真实命中处，显著提升结果可读性。
		// content 取 2 个 fragment 以覆盖可能的多个命中点，order=score 优先高分片段。
		"highlight": map[string]interface{}{
			"fields": map[string]interface{}{
				"title":   map[string]interface{}{"number_of_fragments": 1, "fragment_size": 80},
				"heading": map[string]interface{}{"number_of_fragments": 1, "fragment_size": 80},
				"content": map[string]interface{}{"number_of_fragments": 2, "fragment_size": 160, "order": "score"},
			},
			"pre_tags":             []string{"<em>"},
			"post_tags":            []string{"</em>"},
			"require_field_match": false,
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
			Chunk:      chunk,
			BM25Score:  hit.Score,
			Highlights: hit.Highlight,
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
			Snippet:     buildSnippet(c.Chunk, c.Highlights),
			Score:       c.RRFScore,
			Revision:    c.Chunk.Revision,
			Rank:        i + 1,
		}
	}
	return results
}

// rerankedToResults 将 reranker 结果转换为搜索结果。
//
// 注意：reranked 中 Index 越界的条目会被跳过（不产生结果项）。
// 原实现用预分配数组 + continue，越界时留下零值 SearchResult 占位，
// 导致结果里混入全空条目；改为按 append 紧凑收集，保证每条都是有效结果。
func rerankedToResults(candidates []types.CandidateResult, reranked []types.RerankerResult) []types.SearchResult {
	results := make([]types.SearchResult, 0, len(reranked))
	for _, rr := range reranked {
		if rr.Index < 0 || rr.Index >= len(candidates) {
			continue
		}
		c := candidates[rr.Index]
		results = append(results, types.SearchResult{
			DocumentID:  c.Chunk.DocumentID,
			Path:        c.Chunk.Path,
			Title:       c.Chunk.Title,
			SectionPath: c.Chunk.SectionPath,
			StartLine:   c.Chunk.StartLine,
			EndLine:     c.Chunk.EndLine,
			Snippet:     buildSnippet(c.Chunk, c.Highlights),
			Score:       rr.Score,
			Revision:    c.Chunk.Revision,
			Rank:        len(results) + 1,
		})
	}
	return results
}

// snippetMaxRunes 是 snippet 的总长度上限（按 rune 计）。
// 引入动机：snippet 需要在"命中点可见"与"响应体积可控"之间取一个上限，
// 240 rune 足以容纳 1~2 个 content fragment（各 ~160 字符）加连接符，
// 又不会把整篇 chunk 塞进结果里。
const snippetMaxRunes = 240

// contentFallbackRunes 是无高亮时 content 兜底截断的长度（按 rune 计）。
// 与原实现的 200 一致，保持降级路径输出规模不变。
const contentFallbackRunes = 200

// buildSnippet 基于 ES 高亮片段生成"命中点可见"的 snippet。
//
// 引入动机：原实现 snippet 永远取 chunk 开头（truncate(content,200)），
// 与命中位置无关，模型只能看到开头套话。本函数优先使用 ES 返回的高亮
// fragment 定位真实命中处，提升结果可读性。
//
// 取值优先级：
//  1. highlight["content"]：多 fragment 用 " … " 连接，保留 <em> 标记（模型可读）；
//  2. content 无高亮时降级到 highlight["heading"] / highlight["title"]；
//  3. 完全没有高亮时回退 truncate(chunk.Content, contentFallbackRunes)。
//
// 无论走哪条路径，最终结果都按 rune 截断到 snippetMaxRunes，
// 保证中文等多字节字符不会被按字节切断。
func buildSnippet(chunk types.Chunk, highlights map[string][]string) string {
	// 优先 content 高亮：content 是正文命中，最能反映相关性。
	if fragments := highlights["content"]; len(fragments) > 0 {
		return truncateRunes(strings.Join(fragments, " … "), snippetMaxRunes)
	}
	// 其次 heading 高亮：命中的是小节标题。
	if fragments := highlights["heading"]; len(fragments) > 0 {
		return truncateRunes(strings.Join(fragments, " … "), snippetMaxRunes)
	}
	// 再次 title 高亮：命中的是文档标题。
	if fragments := highlights["title"]; len(fragments) > 0 {
		return truncateRunes(strings.Join(fragments, " … "), snippetMaxRunes)
	}
	// 无任何高亮（如 vector-only 命中）时回退到 chunk 开头截断。
	return truncateRunes(chunk.Content, contentFallbackRunes)
}

// groupByDocument 把 chunk 级搜索结果折叠为"一个文档一条主结果"。
//
// 引入动机：原先一个文档的多个命中 chunk 各占一个结果位，重复输出
// path/title 浪费响应体积且稀释了文档多样性；聚合后 limit 作用于文档数，
// 每条结果代表一个文档的最相关命中段，同文档其余命中段折叠进 OtherRanges。
//
// 语义：
//   - 按 DocumentID 分组，每组取 Score 最高的 chunk 作为主结果；
//   - MatchedChunks = 组大小（该文档本次命中的 chunk 总数）；
//   - 组内其余 chunk 按 Score 降序填入 OtherRanges（最多 3 条）；
//   - 组间按主 chunk Score 降序排列，并重排 Rank。
//
// 输入应已按 Score 降序（diversity.Apply 输出保证），但函数内部仍显式
// 排序以保证独立调用时的正确性。
func groupByDocument(results []types.SearchResult) []types.SearchResult {
	if len(results) == 0 {
		return results
	}

	// 按文档分组，保持文档首次出现的顺序。
	docChunks := make(map[string][]types.SearchResult, len(results))
	docOrder := make([]string, 0, len(results))
	for _, r := range results {
		if _, exists := docChunks[r.DocumentID]; !exists {
			docOrder = append(docOrder, r.DocumentID)
		}
		docChunks[r.DocumentID] = append(docChunks[r.DocumentID], r)
	}

	grouped := make([]types.SearchResult, 0, len(docChunks))
	for _, docID := range docOrder {
		chunks := docChunks[docID]
		// 组内按 Score 降序，最高分 chunk 作为主结果。
		sort.SliceStable(chunks, func(i, j int) bool {
			return chunks[i].Score > chunks[j].Score
		})

		primary := chunks[0]
		primary.MatchedChunks = len(chunks)

		// 其余 chunk 折叠为行号区间（最多 3 条），保留可导航信息。
		// chunks[1:] 已按 Score 降序，取前 3 个即可。
		const maxOtherRanges = 3
		others := chunks[1:]
		if len(others) > maxOtherRanges {
			others = others[:maxOtherRanges]
		}
		if len(others) > 0 {
			primary.OtherRanges = make([]types.LineRange, 0, len(others))
			for _, oc := range others {
				primary.OtherRanges = append(primary.OtherRanges, types.LineRange{
					StartLine: oc.StartLine,
					EndLine:   oc.EndLine,
					Heading:   headingOf(oc),
				})
			}
		}
		grouped = append(grouped, primary)
	}

	// 组间按主 chunk Score 降序并重排 Rank。
	sort.SliceStable(grouped, func(i, j int) bool {
		return grouped[i].Score > grouped[j].Score
	})
	for i := range grouped {
		grouped[i].Rank = i + 1
	}
	return grouped
}

// headingOf 返回一个结果用于 OtherRanges 的 heading 文本。
// 引入动机：SearchResult 没有独立的 Heading 字段，heading 信息承载在
// SectionPath 的末级；取末级 heading 作为区间的可导航标签。
func headingOf(r types.SearchResult) string {
	if len(r.SectionPath) == 0 {
		return ""
	}
	return r.SectionPath[len(r.SectionPath)-1]
}

// minNormalizedScore 是归一化分数的下限阈值。
// 引入动机：归一化后低于该值的结果多为与主结果相关性差距过大的尾部噪声，
// 直接丢弃以控制输出质量；0.15 是一个保守的经验阈值，只裁掉明显脱节的长尾。
const minNormalizedScore = 0.15

// normalizeAndTrimByScore 对结果 Score 做归一化并按阈值裁剪尾部噪声。
//
// 引入动机：RRF 融合分数是 ~0.0X 的不可读小数，模型难以理解；归一化到
// 0~1（score /= top1Score）后分数具备可读性。归一化后低于 minNormalizedScore
// 的结果与最相关结果差距过大，属于噪声，予以裁剪并计数返回。
//
// 参数 usedReranker 表示本批结果的 Score 是否来自 reranker：
// reranker 分数本身通常已在 0~1，但若 reranker 返回全 0 或负分（异常/无效输出），
// 归一化会把所有结果置 0 进而被全部裁掉——这是误删。因此当 usedReranker 为 true
// 且 top1 <= 0 时跳过裁剪，原样返回，避免 reranker 异常输出导致结果全灭。
//
// 返回值：(归一化并裁剪后的结果, 被裁剪掉的结果数)。
func normalizeAndTrimByScore(results []types.SearchResult, usedReranker bool) ([]types.SearchResult, int) {
	if len(results) == 0 {
		return results, 0
	}

	// 找出归一化基准（最大 Score）。结果已按 Score 降序，首位即 top1，
	// 但显式取最大值以兼容独立调用时未排序的输入。
	top1 := results[0].Score
	for _, r := range results {
		if r.Score > top1 {
			top1 = r.Score
		}
	}

	if top1 <= 0 {
		if usedReranker {
			// reranker 全 0/负分属于异常输出：跳过归一化与裁剪，避免误删全部结果。
			return results, 0
		}
		// RRF 路径 top1==0：所有分数都是 0，归一化无意义，全部置 0，不裁剪。
		for i := range results {
			results[i].Score = 0
		}
		return results, 0
	}

	kept := make([]types.SearchResult, 0, len(results))
	truncated := 0
	for i := range results {
		normalized := results[i].Score / top1
		if normalized < minNormalizedScore {
			truncated++
			continue
		}
		results[i].Score = normalized
		kept = append(kept, results[i])
	}

	// 裁剪后重排 Rank，保持连续性。
	for i := range kept {
		kept[i].Rank = i + 1
	}
	return kept, truncated
}

// truncateRunes 按 rune 截断文本到指定长度，超出时追加省略号。
// 引入动机：原 truncate 按字节截断，遇到中文等多字节字符会在字符中间切断
// 产生乱码；snippet 需要按 rune 截断保证输出合法 UTF-8。
func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// truncate 截断文本到指定长度（按 rune）。
// 保留原函数签名以兼容既有调用，内部改为 rune 安全截断。
func truncate(s string, maxLen int) string {
	return truncateRunes(s, maxLen)
}

// generateSearchID 生成搜索唯一标识。
func generateSearchID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}
