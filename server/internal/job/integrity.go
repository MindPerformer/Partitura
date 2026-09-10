// integrity.go 实现索引完整性检查和修复逻辑。
//
// 引入动机：design/01-SEARCH.md §Index Integrity 要求检查：
//   - PostgreSQL current documents vs ES documents
//   - content_hash
//   - revision
//   - chunk count
//   - index profile
//   - missing chunks
//   - orphan chunks
//   - stale chunks
//
// 发现差异只修复相关文档，不盲目全库重建。
package job

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"partitura/server/internal/es"
	"partitura/server/internal/search/chunking"
	types "partitura/server/internal/search/types"
)

// IntegrityIssue 表示一个索引完整性问题。
type IntegrityIssue struct {
	// Type 是问题类型：missing, orphan, stale, hash_mismatch, revision_mismatch, chunk_count_mismatch。
	Type string
	// DocumentID 是相关文档 ID。
	DocumentID string
	// WorkspaceID 是文档所属 workspace ID。
	WorkspaceID string
	// Path 是文档路径。
	Path string
	// Detail 是问题描述。
	Detail string
}

// IntegrityResult 是完整性检查结果。
type IntegrityResult struct {
	Issues    []IntegrityIssue
	TotalPG   int
	TotalES   int
	CheckedOK bool
}

// DocumentForIndex 是从 PG 读取的用于索引的文档信息。
type DocumentForIndex struct {
	ID              string
	WorkspaceID     string
	Path            string
	Title           string
	ContentMarkdown string
	ContentHash     string
	RevisionNumber  int
	Status          string
	IsSpecial       bool
}

// esDocMeta 是从 ES aggregation 中提取的每个文档的元信息。
// 引入动机：integrity check 需要从 ES 获取每个文档的 chunk count、revision、content_hash
// 以与 PG 对比，检测 missing/orphan/stale/hash/revision/chunk count 差异。
type esDocMeta struct {
	ChunkCount  int
	Revision    int
	ContentHash string
}

// CheckIntegrity 检查 PG current documents 与 ES 索引的一致性。
//
// 引入动机：design/01-SEARCH.md §Index Integrity 要求检查 missing/orphan/stale/hash/revision/chunk 差异。
//
// 参数：
//   - ctx：请求 context
//   - db：PG 数据库连接
//   - esClient：ES 客户端
//   - indexName：ES 索引名称
//   - workspaceID：workspace ID（空表示检查所有 workspace）
//   - chunkTargetSize：chunk 目标大小（<=0 使用默认值 512）
//   - chunkOverlap：chunk 重叠大小（<0 使用默认值 64）
//
// 返回 IntegrityResult。
func CheckIntegrity(ctx context.Context, db *sql.DB, esClient es.Client, indexName, workspaceID string, chunkTargetSize, chunkOverlap int) (*IntegrityResult, error) {
	result := &IntegrityResult{}

	// 1. 从 PG 读取所有非特殊文件、非 archived 的 current documents
	pgDocs, err := readPGDocuments(ctx, db, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("读取 PG 文档: %w", err)
	}
	result.TotalPG = len(pgDocs)

	// 2. 从 ES 读取所有 chunk 的 document_id 及其元信息（通过 terms aggregation）
	esDocMetaMap, err := readESDocumentMeta(ctx, esClient, indexName, workspaceID)
	if err != nil {
		slog.Warn("读取 ES 文档元信息失败，可能 ES 不可用", "error", err)
		result.CheckedOK = false
		return result, nil
	}
	result.TotalES = len(esDocMetaMap)

	// 3. 构建 PG doc map 用于快速查找
	pgDocMap := make(map[string]DocumentForIndex, len(pgDocs))
	for _, doc := range pgDocs {
		pgDocMap[doc.ID] = doc
	}

	// 4. 检查 missing chunks（PG 有但 ES 无）
	for _, doc := range pgDocs {
		if _, exists := esDocMetaMap[doc.ID]; !exists {
			result.Issues = append(result.Issues, IntegrityIssue{
				Type:        "missing",
				DocumentID:  doc.ID,
				WorkspaceID: doc.WorkspaceID,
				Path:        doc.Path,
				Detail:      "文档在 PG 中存在但 ES 中缺少 chunk",
			})
		}
	}

	// 5. 检查 orphan chunks（ES 有但 PG 无）
	for docID := range esDocMetaMap {
		if _, exists := pgDocMap[docID]; !exists {
			result.Issues = append(result.Issues, IntegrityIssue{
				Type:       "orphan",
				DocumentID: docID,
				Detail:     "ES 中存在 chunk 但 PG 中文档已删除或已归档",
			})
		}
	}

	// 6. 检查 stale/revision_mismatch/hash_mismatch/chunk_count_mismatch
	//
	// stale vs revision_mismatch 区分：
	//   - stale: ES revision < PG revision，ES 中的 chunk 已过时，PG 有更新版本
	//   - revision_mismatch: ES revision > PG revision 或其他异常不匹配
	// 设计要求区分 stale chunks，因为 stale 是常见场景（文档更新后索引未跟上），
	// 而 revision_mismatch 表示更严重的异常。
	for _, doc := range pgDocs {
		esMeta, exists := esDocMetaMap[doc.ID]
		if !exists {
			continue // 已在 missing 中记录
		}

		// 检查 revision：区分 stale 和 revision_mismatch
		if esMeta.Revision != doc.RevisionNumber {
			if esMeta.Revision < doc.RevisionNumber {
				result.Issues = append(result.Issues, IntegrityIssue{
					Type:       "stale",
					DocumentID: doc.ID,
					Path:       doc.Path,
					Detail: fmt.Sprintf("ES revision %d 低于 PG revision %d，chunk 已过时",
						esMeta.Revision, doc.RevisionNumber),
				})
			} else {
				result.Issues = append(result.Issues, IntegrityIssue{
					Type:       "revision_mismatch",
					DocumentID: doc.ID,
					Path:       doc.Path,
					Detail: fmt.Sprintf("ES revision %d 高于 PG revision %d，索引异常",
						esMeta.Revision, doc.RevisionNumber),
				})
			}
			continue
		}

		// 检查 content_hash
		if esMeta.ContentHash != "" && esMeta.ContentHash != doc.ContentHash {
			result.Issues = append(result.Issues, IntegrityIssue{
				Type:       "hash_mismatch",
				DocumentID: doc.ID,
				Path:       doc.Path,
				Detail:     "ES content_hash 与 PG 不匹配",
			})
			continue
		}

		// 检查 chunk count
		// 从 PG 文档内容计算期望的 chunk 数量，使用与索引时相同的 chunk 参数
		expectedChunks := chunking.ChunkDocument(chunking.DocumentInput{
			DocumentID:  doc.ID,
			WorkspaceID: doc.WorkspaceID,
			Path:        doc.Path,
			Title:       doc.Title,
			Content:     doc.ContentMarkdown,
			Revision:    doc.RevisionNumber,
			Status:      doc.Status,
			IsSpecial:   doc.IsSpecial,
		}, chunkTargetSize, chunkOverlap)
		if len(expectedChunks) != esMeta.ChunkCount {
			result.Issues = append(result.Issues, IntegrityIssue{
				Type:       "chunk_count_mismatch",
				DocumentID: doc.ID,
				Path:       doc.Path,
				Detail:     fmt.Sprintf("ES chunk count %d 与 PG 期望 %d 不匹配", esMeta.ChunkCount, len(expectedChunks)),
			})
		}
	}

	// 7. 检查 ES 中是否存在特殊文档 chunk
	// 引入动机：design/00-MASTER.md §特殊文件 要求 PROJECT.md/AGENTS.md 不进入搜索索引，
	// integrity check 应检测 ES 中是否存在 is_special=true 的 chunk 并报告为 orphan。
	specialIssues := checkSpecialDocsInES(ctx, esClient, indexName, workspaceID)
	result.Issues = append(result.Issues, specialIssues...)

	result.CheckedOK = true
	return result, nil
}

// checkSpecialDocsInES 检查 ES 索引中是否存在 is_special=true 的 chunk。
// 引入动机：design/00-MASTER.md §特殊文件 要求 PROJECT.md/AGENTS.md 不进入搜索索引，
// integrity check 应检测并报告 ES 中的特殊文档 chunk 为 orphan（不应存在）。
//
// 返回的 IntegrityIssue 类型为 "orphan"（特殊文档 chunk 不应在 ES 中存在），
// 修复时通过 DeleteByQuery 删除。
func checkSpecialDocsInES(ctx context.Context, esClient es.Client, indexName, workspaceID string) []IntegrityIssue {
	query := map[string]interface{}{
		"size": 1000,
		"query": map[string]interface{}{
			"term": map[string]interface{}{
				"is_special": true,
			},
		},
	}

	// 添加 workspace filter
	if workspaceID != "" {
		query["query"] = map[string]interface{}{
			"bool": map[string]interface{}{
				"must": []interface{}{
					map[string]interface{}{
						"term": map[string]interface{}{"is_special": true},
					},
					map[string]interface{}{
						"term": map[string]interface{}{"workspace_id": workspaceID},
					},
				},
			},
		}
	}

	resp, err := esClient.Search(ctx, indexName, query)
	if err != nil {
		slog.Warn("查询 ES 中特殊文档 chunk 失败", "error", err)
		return nil
	}

	var issues []IntegrityIssue
	seen := make(map[string]bool)
	for _, hit := range resp.Hits.Hits {
		if did, ok := hit.Source["document_id"].(string); ok && did != "" && !seen[did] {
			seen[did] = true
			issues = append(issues, IntegrityIssue{
				Type:       "orphan",
				DocumentID: did,
				Detail:     "特殊文档 chunk 不应在 ES 索引中存在",
			})
		}
	}
	return issues
}

// RepairIssue 修复单个完整性问题。
// 引入动机：design/01-SEARCH.md §Index Integrity 要求发现差异只修复相关文档。
//
// 参数：
//   - ctx：请求 context
//   - db：PG 数据库连接
//   - esClient：ES 客户端
//   - indexName：ES 索引名称
//   - issue：完整性问题描述
//   - embProvider：可选的 embedding provider（nil 时跳过 embedding）
//   - profileConfig：可选的 profile 配置（提供 chunk 参数和 embedding 指令）
func RepairIssue(ctx context.Context, db *sql.DB, esClient es.Client, indexName string, issue IntegrityIssue, embProvider embeddingProviderFunc, profileConfig *types.SearchProfileConfig) error {
	switch issue.Type {
	case "missing", "stale", "revision_mismatch", "hash_mismatch", "chunk_count_mismatch":
		// 重新索引该文档（先删除旧 chunk，再写入新 chunk）
		// 引入动机：design/01-SEARCH.md §Index Integrity 要求发现差异只修复相关文档，
		// 不盲目全库重建。reindexDocumentWithEmbedding 先 DeleteByQuery 删除该文档的旧 chunk，
		// 再重新 chunking + BulkIndex 写入新 chunk。
		return reindexDocumentWithEmbedding(ctx, db, esClient, indexName, issue.DocumentID, embProvider, profileConfig)
	case "orphan":
		// 删除 ES 中的 orphan chunk（包括特殊文档 chunk）
		query := map[string]interface{}{
			"term": map[string]interface{}{
				"document_id": issue.DocumentID,
			},
		}
		return esClient.DeleteByQuery(ctx, indexName, query)
	default:
		return fmt.Errorf("未知问题类型: %s", issue.Type)
	}
}

// reindexDocumentWithEmbedding 重新索引单个文档，支持可选的 embedding 生成。
// 引入动机：C5 要求 index job 实际通过可配置在线 embedding provider 对 chunk 构建 embedding。
// 当 embProvider 和 profileConfig 都可用时，为每个 chunk 生成 embedding 向量。
// 当 embProvider 不可用时，仅写入 lexical 字段，不写入 embedding 字段。
//
// 参数：
//   - ctx：请求 context
//   - db：PG 数据库连接
//   - esClient：ES 客户端
//   - indexName：目标 ES 索引名称
//   - documentID：待索引的文档 ID
//   - embProvider：可选的 embedding provider（nil 时跳过 embedding）
//   - profileConfig：可选的 profile 配置（提供 chunk 参数和 embedding 指令）
func reindexDocumentWithEmbedding(ctx context.Context, db *sql.DB, esClient es.Client, indexName, documentID string, embProvider embeddingProviderFunc, profileConfig *types.SearchProfileConfig) error {
	// 从 PG 读取文档
	var doc DocumentForIndex
	err := db.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, content_markdown, content_hash, revision_number, status, is_special
		 FROM documents WHERE id = $1`,
		documentID,
	).Scan(&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.ContentMarkdown,
		&doc.ContentHash, &doc.RevisionNumber, &doc.Status, &doc.IsSpecial)
	if err != nil {
		return fmt.Errorf("读取文档 %s: %w", documentID, err)
	}

	// 特殊文件不入索引
	if doc.IsSpecial {
		return nil
	}

	// archived 文档不入索引（C6：archived 文档排除搜索）
	if doc.Status == "archived" {
		// 删除可能存在的旧 chunk
		delQuery := map[string]interface{}{
			"term": map[string]interface{}{
				"document_id": documentID,
			},
		}
		if err := esClient.DeleteByQuery(ctx, indexName, delQuery); err != nil {
			slog.Warn("删除 archived 文档旧 chunk 失败", "doc_id", documentID, "error", err)
		}
		return nil
	}

	// 删除旧 chunk
	delQuery := map[string]interface{}{
		"term": map[string]interface{}{
			"document_id": documentID,
		},
	}
	if err := esClient.DeleteByQuery(ctx, indexName, delQuery); err != nil {
		return fmt.Errorf("删除旧 chunk: %w", err)
	}

	// 重新 chunking
	targetSize := 0
	overlap := 0
	if profileConfig != nil {
		targetSize = profileConfig.ChunkTargetSize
		overlap = profileConfig.ChunkOverlap
	}
	chunks := chunking.ChunkDocument(chunking.DocumentInput{
		DocumentID:  doc.ID,
		WorkspaceID: doc.WorkspaceID,
		Path:        doc.Path,
		Title:       doc.Title,
		Content:     doc.ContentMarkdown,
		Revision:    doc.RevisionNumber,
		Status:      doc.Status,
		IsSpecial:   doc.IsSpecial,
	}, targetSize, overlap)

	if len(chunks) == 0 {
		slog.Info("文档无内容可索引", "doc_id", documentID)
		return nil
	}

	// 构建 embedding 输入并调用 provider（C5）
	//
	// 防御性检查：当 embProvider 非 nil 但 profileConfig 为 nil 时，
	// 不能静默跳过 embedding 并写入无向量 chunk。此情况应由调用方在
	// handleIndexDocument/handleRebuildIndex 中拦截，但 reindexDocumentWithEmbedding
	// 作为独立函数也必须自我保护，避免被其他调用路径绕过。
	if embProvider != nil && profileConfig == nil {
		return fmt.Errorf("embedding provider 可用但 profileConfig 为 nil，无法生成向量")
	}

	var embeddings [][]float32
	if embProvider != nil && profileConfig != nil {
		embInputs := make([]string, len(chunks))
		for i, c := range chunks {
			embInputs[i] = chunking.BuildEmbeddingInput(c, profileConfig.EmbeddingQueryInstruction, profileConfig.EmbeddingDocInstruction)
		}
		vectors, embErr := embProvider(ctx, embInputs)
		if embErr != nil {
			slog.Warn("embedding 生成失败，job 可重试；文档保存不受影响", "doc_id", documentID, "error", embErr)
			return fmt.Errorf("embedding 生成失败: %w", embErr)
		}
		if len(vectors) != len(chunks) {
			slog.Error("embedding 返回向量数量不匹配", "expected", len(chunks), "got", len(vectors), "doc_id", documentID)
			return fmt.Errorf("embedding 返回向量数量不匹配: expected %d, got %d", len(chunks), len(vectors))
		}
		// 验证向量维度
		if len(vectors) > 0 && profileConfig.EmbeddingDimensions > 0 {
			if len(vectors[0]) != profileConfig.EmbeddingDimensions {
				slog.Error("embedding 向量维度不匹配", "expected", profileConfig.EmbeddingDimensions, "got", len(vectors[0]), "doc_id", documentID)
				return fmt.Errorf("embedding 向量维度不匹配: expected %d, got %d", profileConfig.EmbeddingDimensions, len(vectors[0]))
			}
		}
		embeddings = vectors
	}

	// 写入 ES
	docs := make([]es.IndexDoc, len(chunks))
	for i, c := range chunks {
		body := map[string]interface{}{
			"document_id":  c.DocumentID,
			"workspace_id": c.WorkspaceID,
			"path":         c.Path,
			"title":        c.Title,
			"heading":      c.Heading,
			"section_path": c.SectionPath, // M2：保留真实层级 []string
			"content":      c.Content,
			"start_line":   c.StartLine,
			"end_line":     c.EndLine,
			"chunk_index":  c.ChunkIndex,
			"content_hash": c.ContentHash,
			"revision":     c.Revision,
			"status":       c.Status,
			"is_special":   c.IsSpecial,
		}
		// 仅当 embedding 可用时写入向量字段（C5）
		if embeddings != nil && i < len(embeddings) && len(embeddings[i]) > 0 {
			body["embedding"] = embeddings[i]
		}
		docs[i] = es.IndexDoc{
			ID:   fmt.Sprintf("%s_%d", c.DocumentID, c.ChunkIndex),
			Body: body,
		}
	}

	if err := esClient.BulkIndex(ctx, indexName, docs); err != nil {
		return fmt.Errorf("写入 ES: %w", err)
	}

	slog.Info("文档重新索引完成", "doc_id", documentID, "chunks", len(chunks), "with_embedding", embeddings != nil)
	return nil
}

// embeddingProviderFunc 是 embedding provider 的函数签名。
// 引入动机：避免 job 包对 embedding 包的直接依赖，使用函数类型实现依赖注入。
type embeddingProviderFunc func(ctx context.Context, texts []string) ([][]float32, error)

// readPGDocuments 从 PG 读取所有非特殊文件、非 archived 的 current documents。
// 引入动机：C6 要求 archived 文档不被搜索，integrity check 也应排除 archived。
func readPGDocuments(ctx context.Context, db *sql.DB, workspaceID string) ([]DocumentForIndex, error) {
	query := `SELECT id, workspace_id, path, title, content_markdown, content_hash, revision_number, status, is_special
		 FROM documents WHERE is_special = FALSE AND status != 'archived'`
	args := []interface{}{}

	if workspaceID != "" {
		query += ` AND workspace_id = $1`
		args = append(args, workspaceID)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 PG 文档: %w", err)
	}
	defer rows.Close()

	var docs []DocumentForIndex
	for rows.Next() {
		var d DocumentForIndex
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Path, &d.Title, &d.ContentMarkdown,
			&d.ContentHash, &d.RevisionNumber, &d.Status, &d.IsSpecial); err != nil {
			return nil, fmt.Errorf("扫描文档行: %w", err)
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历文档结果集: %w", err)
	}

	return docs, nil
}

// readESDocumentMeta 从 ES 读取所有 chunk 的 document_id 及其元信息（chunk count, revision, content_hash）。
// 引入动机：C3 要求 readESDocumentIDs 使用真实 ES aggregation 返回全部索引 document id
// 及必要 hash/revision/chunk count，CheckIntegrity 可以检测 missing/orphan/stale/hash/revision/chunk 差异。
// 使用 terms aggregation 按 document_id 分桶，子聚合获取每个文档的 chunk count、最新 revision 和 content_hash。
func readESDocumentMeta(ctx context.Context, esClient es.Client, indexName, workspaceID string) (map[string]esDocMeta, error) {
	// 构建 terms aggregation 查询
	query := map[string]interface{}{
		"size": 0,
		"aggs": map[string]interface{}{
			"doc_ids": map[string]interface{}{
				"terms": map[string]interface{}{
					"field": "document_id",
					"size":  100000, // 足够大的 bucket 数量
				},
				"aggs": map[string]interface{}{
					"chunk_count": map[string]interface{}{
						"value_count": map[string]interface{}{
							"field": "document_id",
						},
					},
					"max_revision": map[string]interface{}{
						"max": map[string]interface{}{
							"field": "revision",
						},
					},
					"sample_hash": map[string]interface{}{
						"terms": map[string]interface{}{
							"field": "content_hash",
							"size":  1,
						},
					},
				},
			},
		},
	}

	// 添加 workspace filter
	if workspaceID != "" {
		query["query"] = map[string]interface{}{
			"term": map[string]interface{}{
				"workspace_id": workspaceID,
			},
		}
	}

	resp, err := esClient.Search(ctx, indexName, query)
	if err != nil {
		return nil, fmt.Errorf("ES aggregation 查询: %w", err)
	}

	if resp.Aggregations == nil {
		return nil, fmt.Errorf("ES 响应中无 aggregations")
	}

	docIDsAgg, ok := resp.Aggregations["doc_ids"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("ES 响应中无 doc_ids 聚合")
	}

	buckets, ok := docIDsAgg["buckets"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("ES 响应中无 buckets")
	}

	result := make(map[string]esDocMeta, len(buckets))
	for _, bucket := range buckets {
		b, ok := bucket.(map[string]interface{})
		if !ok {
			continue
		}
		docID, _ := b["key"].(string)
		if docID == "" {
			continue
		}

		meta := esDocMeta{}

		// chunk count
		if cc, ok := b["chunk_count"].(map[string]interface{}); ok {
			if v, ok := cc["value"].(float64); ok {
				meta.ChunkCount = int(v)
			}
		}

		// max revision
		if mr, ok := b["max_revision"].(map[string]interface{}); ok {
			if v, ok := mr["value"].(float64); ok {
				meta.Revision = int(v)
			}
		}

		// sample hash (取第一个 bucket 的 key)
		if sh, ok := b["sample_hash"].(map[string]interface{}); ok {
			if shBuckets, ok := sh["buckets"].([]interface{}); ok && len(shBuckets) > 0 {
				if shBucket, ok := shBuckets[0].(map[string]interface{}); ok {
					if key, ok := shBucket["key"].(string); ok {
						meta.ContentHash = key
					}
				}
			}
		}

		result[docID] = meta
	}

	return result, nil
}

// IndexJobHandler 实现 JobHandler 接口，处理索引相关任务。
// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求支持 index_document, rebuild_index, repair_index 等任务。
// design/03-DOCUMENTS.md §Revision 要求定时清理历史 snapshot，即 cleanup_revisions 任务。
type IndexJobHandler struct {
	db       *sql.DB
	esClient es.Client
	// embEmbed 用于生成 embedding 向量（C5）
	embEmbed func(ctx context.Context, texts []string) ([][]float32, error)
	// embConfigured 判断 provider 是否已配置，不执行网络型健康检查。
	embConfigured func(ctx context.Context) bool
	// profileRepo 用于获取 active profile
	profileRepo ProfileRepo
	// jobRepo 用于在维度不一致时投递 rebuild_index 并查询队列去重。
	// 引入动机：index_document 的兜底路径（ensureAliasAndIndex）一旦发现 alias 指向的索引
	// embedding 维度与 profile 不一致，写入必然被 ES 拒绝，必须自动请求一次全量重建；
	// 同时需要通过查询队列避免重复入队多个 rebuild_index。
	jobRepo JobEnqueuer
	// revisionCleanupRepo 用于清理过期 revision
	// 引入动机：cleanup_revisions job 需要调用 document.CleanupRevisions，
	// 使用接口避免 job 包对 document 包的直接依赖。
	revisionCleanupRepo RevisionCleanupRepo
}

// ProfileRepo 定义获取 active profile 的接口。
// 引入动机：index job 需要获取 active profile 以确定 chunk 参数和 embedding 配置。
type ProfileRepo interface {
	GetActiveProfile(ctx context.Context) (*ProfileForJob, error)

	// UpdateProfileESIndex 更新 profile 记录的 ES 索引名。
	// 引入动机：rebuild 在维度不一致时会创建新索引并切换 alias，必须把新索引名回写到 profile，
	// 否则 profile 记录与 alias 实际指向不一致，后续 rebuild/rollback 会再次选错索引。
	UpdateProfileESIndex(ctx context.Context, id, indexName string) error
}

// JobEnqueuer 定义向 job 队列投递与查询任务的能力。
// 引入动机：维度不一致时 index_document 需要自动请求一次全量重建，并需查询队列以避免重复入队。
// job.NewPGRepository 已实现该接口，故生产环境直接注入即可。
type JobEnqueuer interface {
	// Enqueue 投递一个 job，返回 job ID。
	Enqueue(ctx context.Context, jobType string, payload map[string]interface{}) (string, error)
	// List 按状态过滤查询 job 列表，用于判断是否已有 rebuild_index 在队列中。
	List(ctx context.Context, statusFilter string, limit, offset int) (*ListResult, error)
}

// 编译期确认本包的 PG 仓储实现满足 JobEnqueuer 注入契约。
// 引入动机：IndexJobHandler 的 jobRepo 由组装层注入 PGRepository，若其 Enqueue/List 签名漂移，
// 这里会直接编译失败，而不是等到运行时才发现无法投递重建任务。
var _ JobEnqueuer = (*PGRepository)(nil)

// RevisionCleanupRepo 定义清理过期 revision 的接口。
// 引入动机：cleanup_revisions job 需要调用 document.CleanupRevisions，
// 使用接口避免 job 包对 document 包的直接依赖导致循环导入。
type RevisionCleanupRepo interface {
	// GetWorkspaceRetentionConfig 查询 workspace 的 revision 保留配置。
	GetWorkspaceRetentionConfig(ctx context.Context, workspaceID string) (retentionDays int, maxCount int, err error)
	// CleanupRevisions 清理指定文档的过期 revision，保留当前版本。
	CleanupRevisions(ctx context.Context, workspaceID, documentID string, retentionDays, maxCount int) (int, error)
}

// ProfileForJob 是 job handler 使用的 profile 信息。
// 引入动机：避免 job 包对 profile 包的循环依赖，使用独立结构体。
// Analyzer 字段引入动机：Phase 6 修复要求 rebuild_index 从 active profile
// 获取 analyzer 配置以创建正确的 ES index mapping，而非依赖 ES auto-create。
type ProfileForJob struct {
	ID                        string
	ESIndexName               string
	ChunkTargetSize           int
	ChunkOverlap              int
	EmbeddingDimensions       int
	EmbeddingQueryInstruction string
	EmbeddingDocInstruction   string
	// Analyzer 是 ES 索引的分析器名称（如 "standard"），用于创建 index mapping。
	Analyzer string
}

// NewIndexJobHandler 创建索引任务处理器。
// 引入动机：C5 要求 index job 实际生成 embedding，需要注入 embedding provider 和 profile repo。
// embEmbed 和 embConfigured 从 embedding.Provider 接口适配，避免 job 包导入 embedding 包。
// embConfigured 只表示 provider 已配置，不执行网络健康检查。
// jobRepo 用于维度不一致时投递 rebuild_index（可为 nil，但此时遇到维度不一致会显式报错而非静默跳过）。
// revisionCleanupRepo 用于 cleanup_revisions job，可为 nil（当 revision 清理未启用时）。
func NewIndexJobHandler(db *sql.DB, esClient es.Client, embEmbed func(ctx context.Context, texts []string) ([][]float32, error), embConfigured func(ctx context.Context) bool, profileRepo ProfileRepo, jobRepo JobEnqueuer, revisionCleanupRepo RevisionCleanupRepo) *IndexJobHandler {
	return &IndexJobHandler{
		db:                  db,
		esClient:            esClient,
		embEmbed:            embEmbed,
		embConfigured:       embConfigured,
		profileRepo:         profileRepo,
		jobRepo:             jobRepo,
		revisionCleanupRepo: revisionCleanupRepo,
	}
}

// HandleJob 处理一个索引任务。
func (h *IndexJobHandler) HandleJob(ctx context.Context, job *Job) error {
	switch job.Type {
	case types.JobIndexDocument:
		return h.handleIndexDocument(ctx, job)
	case types.JobRebuildIndex:
		return h.handleRebuildIndex(ctx, job)
	case types.JobRepairIndex:
		return h.handleRepairIndex(ctx, job)
	case types.JobCleanupOldIndexes:
		return h.handleCleanupOldIndexes(ctx, job)
	case types.JobCleanupRevisions:
		return h.handleCleanupRevisions(ctx, job)
	default:
		return fmt.Errorf("未知的 job 类型: %s", job.Type)
	}
}

// handleIndexDocument 处理单个文档索引任务。
// 引入动机：C5 要求 index job 实际通过 embedding provider 生成向量。
//
// 修复说明：当 indexName 是 alias（knowledge_current）且 alias 不存在时，
// 不能直接向 alias 名写入（ES 会自动创建同名具体索引，使用动态 mapping，
// 缺失 embedding 字段）。应先确保 alias 存在——如果不存在，从 active profile
// 获取 dimensions 和 analyzer 创建正确的 versioned index 并建立 alias。
func (h *IndexJobHandler) handleIndexDocument(ctx context.Context, job *Job) error {
	docID, _ := job.Payload["document_id"].(string)
	if docID == "" {
		return fmt.Errorf("缺少 document_id")
	}
	indexName, _ := job.Payload["index_name"].(string)
	if indexName == "" {
		indexName = es.AliasName
	}

	// 获取 active profile 以确定 chunk 参数和 embedding 配置
	var profileConfig *types.SearchProfileConfig
	var dimensions int
	var analyzer string
	var profileID string
	if h.profileRepo != nil {
		p, err := h.profileRepo.GetActiveProfile(ctx)
		if err == nil && p != nil {
			profileConfig = &types.SearchProfileConfig{
				ChunkTargetSize:           p.ChunkTargetSize,
				ChunkOverlap:              p.ChunkOverlap,
				EmbeddingDimensions:       p.EmbeddingDimensions,
				EmbeddingQueryInstruction: p.EmbeddingQueryInstruction,
				EmbeddingDocInstruction:   p.EmbeddingDocInstruction,
			}
			dimensions = p.EmbeddingDimensions
			analyzer = p.Analyzer
			profileID = p.ID
		}
	}

	// 确保 alias 和目标索引存在。
	// 引入动机：当 indexName 是 alias 且不存在时，直接写入会导致 ES 自动创建
	// 同名具体索引（动态 mapping，无 embedding 字段）。必须先创建正确的
	// versioned index 和 alias，再写入数据。
	// profileID 用于在 alias 指向的索引维度与 profile 不一致时投递 rebuild_index。
	if indexName == es.AliasName {
		if err := h.ensureAliasAndIndex(ctx, dimensions, analyzer, profileID); err != nil {
			return fmt.Errorf("确保索引和 alias 存在: %w", err)
		}
	}

	// 构建 embedding 函数
	//
	// 修复说明：当 Provider 已配置（embConfigured 返回 true）但 profileConfig 为 nil 时，
	// 原实现将 embFunc 设为 nil 并静默完成无向量索引。这违反设计要求：
	// "index_document 不得在 Provider 已配置却未生成向量时静默完成"。
	// embConfigured 仅用于判断是否配置，不执行网络健康检查；正式可用性由 Embed 结果决定。
	var embFunc embeddingProviderFunc
	if h.embEmbed != nil && h.embConfigured != nil && h.embConfigured(ctx) {
		if profileConfig == nil {
			return fmt.Errorf("embedding provider 已配置但无法获取 active profile 配置，无法生成向量")
		}
		embFunc = h.embEmbed
	}

	return reindexDocumentWithEmbedding(ctx, h.db, h.esClient, indexName, docID, embFunc, profileConfig)
}

// ensureAliasAndIndex 确保 alias（knowledge_current）存在并指向一个有效的 versioned index。
//
// 引入动机：index_document job 使用 alias 名称作为写入目标。
// 如果 alias 不存在，直接写入会导致 ES 自动创建同名具体索引（动态 mapping，
// 缺失 embedding 字段），后续 rebuild 的 alias 切换会因同名冲突返回 400。
// 此方法在 alias 不存在时创建正确的 versioned index 并建立 alias，
// 从 active profile 获取 dimensions 和 analyzer 配置。
//
// 约束：
//   - 如果 alias 已存在，校验其指向索引的 embedding 维度与 profile 一致，不一致时
//     投递一次 rebuild_index 并返回错误（fail fast）。
//   - 如果 alias 不存在但存在同名具体索引，先删除该索引再创建 alias。
//   - 创建新 index 时使用 active profile 的 dimensions 和 analyzer。
//
// profileID 的引入动机：alias 指向的索引维度与 profile 不一致时需要按 profile 投递
// rebuild_index 自动重建，profileID 用于标记待重建的 profile。
func (h *IndexJobHandler) ensureAliasAndIndex(ctx context.Context, dimensions int, analyzer string, profileID string) error {
	// 检查 alias 是否已存在
	aliasIndex, aliasErr := h.esClient.GetAliasIndex(ctx, es.AliasName)
	if aliasErr == nil {
		// alias 已存在，校验维度。
		//
		// 引入动机：ES 的 dense_vector.dims 建好后不可变。若 alias 指向的索引维度与 profile
		// 不一致（例如改了 profile 维度但没有重新激活/重建），写入新维度向量必然被 ES 以 400 拒绝。
		// 这里 fail fast 并请求一次全量重建，避免 index_document 在错误索引上无限重试。
		aliasDims, dimsErr := h.esClient.GetIndexDimensions(ctx, aliasIndex)
		if dimsErr != nil {
			return fmt.Errorf("读取 alias %s 指向索引 %s 的 embedding 维度: %w", es.AliasName, aliasIndex, dimsErr)
		}
		if aliasDims > 0 && dimensions > 0 && aliasDims != dimensions {
			slog.Error("索引维度与 profile 不一致，触发自动重建",
				"alias_index", aliasIndex, "index_dims", aliasDims, "profile_dims", dimensions)
			if err := h.enqueueRebuildIfAbsent(ctx, profileID); err != nil {
				return fmt.Errorf("索引 %s 维度 %d 与 profile 维度 %d 不一致，请求自动重建失败: %w",
					aliasIndex, aliasDims, dimensions, err)
			}
			return fmt.Errorf("索引 %s 维度 %d 与 profile 维度 %d 不一致，已请求自动重建",
				aliasIndex, aliasDims, dimensions)
		}
		// alias 已存在且维度一致，无需操作
		return nil
	}

	// alias 不存在，检查是否存在同名具体索引
	exists, err := h.esClient.IndexExists(ctx, es.AliasName)
	if err != nil {
		return fmt.Errorf("检查索引 %s 是否存在: %w", es.AliasName, err)
	}
	if exists {
		// 存在同名具体索引（ES 自动创建的），删除它
		slog.Warn("发现同名具体索引，先删除再创建 alias 和 versioned index", "index", es.AliasName)
		if err := h.esClient.DeleteIndex(ctx, es.AliasName); err != nil {
			return fmt.Errorf("删除同名具体索引 %s: %w", es.AliasName, err)
		}
	}

	// 创建 versioned index
	if analyzer == "" {
		analyzer = "standard"
	}
	if dimensions <= 0 {
		// 移除原先的 dimensions = 1024 兜底（引入动机）：静默兜底正是本次维度事故的隐患来源，
		// profile.embedding_dimensions 由 admin 接口校验 > 0，出现 <= 0 属数据异常，必须暴露。
		return fmt.Errorf("profile embedding 维度无效(%d)，无法创建索引 %s", dimensions, es.AliasName)
	}
	versionedIndex := es.GenerateIndexName(1)
	mapping := es.BuildIndexMapping(dimensions, analyzer)
	if err := h.esClient.CreateIndex(ctx, versionedIndex, mapping); err != nil {
		return fmt.Errorf("创建索引 %s（dimensions=%d, analyzer=%s）: %w", versionedIndex, dimensions, analyzer, err)
	}
	slog.Info("index_document：已创建新索引", "index", versionedIndex, "dimensions", dimensions, "analyzer", analyzer)

	// 创建 alias 指向新索引
	if err := es.SwitchAlias(ctx, h.esClient, es.AliasName, versionedIndex); err != nil {
		// alias 创建失败，清理刚创建的索引
		if delErr := h.esClient.DeleteIndex(ctx, versionedIndex); delErr != nil {
			slog.Error("清理失败的新索引失败", "index", versionedIndex, "error", delErr)
		}
		return fmt.Errorf("创建 alias %s → %s: %w", es.AliasName, versionedIndex, err)
	}

	return nil
}

// rebuildAbsentListLimit 是判断"是否已有 rebuild_index 在队列中"时每次查询的条数上限。
// 引入动机：去重只需检查最近一批 job，限定条数避免为一次判断拉取整张 jobs 表。
const rebuildAbsentListLimit = 50

// rebuildAbsentStatuses 是判断"是否已有 rebuild_index 需要处理"时查询的 job 状态集合。
//
// 引入动机（缺陷 C 修复）：原实现只查 pending/running，而 PGRepository.Fail 在重试次数耗尽时
// 会把 job 置为终态 'dead'（未耗尽时为 'pending'）——因此 dead 是 pending/running 之外唯一需要
// 额外覆盖的终态。一旦某次 rebuild 变成 dead，下一次 index_document 就会再入队一个全新的
// rebuild_index（经 es.NextIndexName 把索引名再推高一位），形成"无限自建、无限失败"。
//
// 取舍动机：自动重复入队并不能解决 rebuild 本身的失败原因（例如 bulk 429），只会让每次重建都
// 产出新的 knowledge_v{N+1}，把集群索引越堆越多。终态失败因此必须交给人工显式处置
// （管理端 job 重试，或人工触发一次显式 rebuild），而不是让系统自动无限重建。
var rebuildAbsentStatuses = []string{"pending", "running", "dead"}

// enqueueRebuildIfAbsent 在队列中不存在待处理、执行中或终态失败的 rebuild_index 时投递一次全量重建。
//
// 引入动机：index_document 兜底路径发现索引维度与 profile 不一致时，必须让系统自动重建到
// 正确维度，否则该文档的写入会永久失败。查询队列是为了避免每个 index_document 都重复入队。
//
// 去重为尽力而为：List 与 Enqueue 之间存在并发窗口，极端情况下可能入队 2 个 rebuild_index，
// 但 rebuild 幂等（第二个会复用刚建好的索引），不会造成数据损坏，仅多一次空转。
func (h *IndexJobHandler) enqueueRebuildIfAbsent(ctx context.Context, profileID string) error {
	if h.jobRepo == nil {
		// 不允许静默跳过：维度不一致已使当前索引不可写，若不投递重建，
		// index_document 会永久失败且没有任何自愈路径。返回错误保证调用方与日志都能看到。
		return fmt.Errorf("索引维度与 profile 不一致需要入队 %s，但 jobRepo 未注入", types.JobRebuildIndex)
	}

	for _, status := range rebuildAbsentStatuses {
		result, err := h.jobRepo.List(ctx, status, rebuildAbsentListLimit, 0)
		if err != nil {
			return fmt.Errorf("查询 %s 状态的 job 以判断是否已有 %s: %w", status, types.JobRebuildIndex, err)
		}
		if result == nil {
			return fmt.Errorf("查询 %s 状态的 job 返回空结果集", status)
		}
		for _, existing := range result.Jobs {
			if existing.Type == types.JobRebuildIndex {
				if status == "dead" {
					// 终态失败不自动重建：见 rebuildAbsentStatuses 的取舍动机。
					slog.Warn("已存在终态(dead) rebuild_index，跳过重复入队，需人工显式重试或手动重建",
						"status", status, "existing_job_id", existing.ID)
				} else {
					slog.Info("已存在待执行/执行中的 rebuild_index，跳过重复入队",
						"status", status, "existing_job_id", existing.ID)
				}
				return nil
			}
		}
	}

	nextIndexName, err := es.NextIndexName(ctx, h.esClient)
	if err != nil {
		return fmt.Errorf("计算重建目标索引名: %w", err)
	}
	if _, err := h.jobRepo.Enqueue(ctx, types.JobRebuildIndex, map[string]interface{}{
		"index_name": nextIndexName,
		"profile_id": profileID,
	}); err != nil {
		return fmt.Errorf("入队 %s（index_name=%s, profile_id=%s）: %w",
			types.JobRebuildIndex, nextIndexName, profileID, err)
	}
	slog.Info("已入队 rebuild_index 以修复索引维度不一致", "index_name", nextIndexName, "profile_id", profileID)
	return nil
}

// listVersionedIndexes 列出集群中所有版本化索引（knowledge_v*）。
// 引入动机：统计"已有版本化索引数量"（缺陷 D）与挑选可复用索引（缺陷 A）都需要这份列表，
// 统一走这里避免两处的 pattern 约定漂移。es.IndexNamePrefix 的前缀过滤天然排除了
// knowledge_current 这类非版本化名字（alias 名）。
func (h *IndexJobHandler) listVersionedIndexes(ctx context.Context) ([]string, error) {
	indices, err := h.esClient.ListIndices(ctx, es.IndexNamePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("列出 %s* 索引: %w", es.IndexNamePrefix, err)
	}
	return indices, nil
}

// findReusableIndex 在已存在的版本化索引中挑选一个 embedding 维度与 profile 完全一致的索引，返回其名字。
//
// 引入动机（缺陷 A 修复）：rebuild 发现目标索引维度与 profile 不一致时，原实现无条件新建
// knowledge_v{max+1}，导致每次重建都新增一个索引。只要集群中已经存在一个维度正确的索引
// （例如上一次重建已成功创建、但在 alias 切换之前失败的产物），就应该直接复用它。
//
// 确定性取舍：多个候选都匹配时取版本号最大的一个。动机：
//   - 版本号最大 = 最近一次构建的产物，其 analyzer 等 mapping 设置最接近当前 profile；
//   - 选择必须确定：同一现场无论重试多少次、由哪个 worker 执行，都必须复用同一个索引，
//     否则 alias 会在多个同维度索引之间来回漂移，旧索引清理也无从判断保留谁。
//
// 候选过滤规则：
//   - 候选来自 es.IndexNamePrefix+"*"，因此 knowledge_current 这类不含前缀的名字不会出现；
//   - GetIndexDimensions 对"索引不存在"或"没有 embedding 字段"返回 (0, nil)，这类候选必须跳过，
//     不能把 0 当作匹配（dimensions <= 0 已在调用方被拒绝，不存在 0 == dimensions 的合法情形）；
//   - 维度不一致的候选一律跳过，因此那个"维度不对的原目标索引"绝不会被选中；
//   - 名字无法解析出版本号时跳过并 Warn，不参与"最大版本"比较（不让不可比较的候选影响确定性）。
//
// 返回空字符串表示没有可复用的索引，调用方应退回新建路径。
func (h *IndexJobHandler) findReusableIndex(ctx context.Context, dimensions int) (string, error) {
	indices, err := h.listVersionedIndexes(ctx)
	if err != nil {
		return "", err
	}

	reusableIndex := ""
	reusableVersion := 0
	for _, candidate := range indices {
		dims, dimsErr := h.esClient.GetIndexDimensions(ctx, candidate)
		if dimsErr != nil {
			return "", fmt.Errorf("读取索引 %s 的 embedding 维度: %w", candidate, dimsErr)
		}
		if dims != dimensions {
			continue
		}
		version, ok := esIndexVersion(candidate)
		if !ok {
			slog.Warn("跳过无法解析版本号的索引候选", "index", candidate, "dimensions", dims)
			continue
		}
		if version > reusableVersion {
			reusableIndex = candidate
			reusableVersion = version
		}
	}
	return reusableIndex, nil
}

// esIndexVersion 从索引名解析版本号（knowledge_v{n} → n）。
// 引入动机：findReusableIndex 需要"取版本号最大者"这一确定性取舍，而版本号比较只在 job 包内使用；
// es 包的 parseIndexVersion 未导出，为不改动并行修改中的 es 包，这里按同一规则本地实现：
// 前缀之后必须是非空纯十进制数字，knowledge_v、knowledge_v1_backup、knowledge_v-1 都视为解析失败。
func esIndexVersion(name string) (int, bool) {
	suffix, hasPrefix := strings.CutPrefix(name, es.IndexNamePrefix)
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

// handleRebuildIndex 处理全量重建索引任务。
// 引入动机：C7 要求 rebuild 基于新 profile 创建新 versioned index → reindex → refresh →
// integrity validation → alias 原子切换 → PG 持久 profile indexname/status。
//
// Phase 6 修复：
//   - 目标索引不存在时，使用 active profile 的 dimensions 和 analyzer 显式创建正确 mapping，
//     绝不依赖 ES auto-create 或手动预建。
//   - 索引已存在时不重复创建，直接使用。
//   - alias 切换使用 ES8 兼容的嵌套 JSON 格式（通过 AliasAction.MarshalJSON 实现）。
//
// 维度自愈：ES 的 dense_vector.dims 建好后不可变。当目标索引的 embedding 维度与 active profile
// 不一致时，复用该索引会让后续写入必然被 ES 以 400 拒绝。此时先尝试复用集群中已存在的、维度正确的
// 索引（缺陷 A），找不到才新建一个索引名承载正确维度的 mapping，全量重建后切换 alias，
// 并把索引名回写 profile。
//
// 失败回收：本次 job 自己创建的索引，在 alias 切换成功之前的任何失败都会由本函数回收
// （缺陷 B），避免失败重试不断在集群里堆积无人引用的 knowledge_vN。
//
// 增长可见：新建索引时把已有版本化索引数量记入日志（缺陷 D），使失控增长在日志中可直接观察。
func (h *IndexJobHandler) handleRebuildIndex(ctx context.Context, job *Job) error {
	indexName, _ := job.Payload["index_name"].(string)
	workspaceID, _ := job.Payload["workspace_id"].(string)
	profileID, _ := job.Payload["profile_id"].(string)

	// 获取 profile 配置
	var profileConfig *types.SearchProfileConfig
	var activeProfile *ProfileForJob
	var analyzer string
	var dimensions int
	if h.profileRepo != nil {
		p, err := h.profileRepo.GetActiveProfile(ctx)
		if err == nil && p != nil {
			activeProfile = p
			profileConfig = &types.SearchProfileConfig{
				ChunkTargetSize:           p.ChunkTargetSize,
				ChunkOverlap:              p.ChunkOverlap,
				EmbeddingDimensions:       p.EmbeddingDimensions,
				EmbeddingQueryInstruction: p.EmbeddingQueryInstruction,
				EmbeddingDocInstruction:   p.EmbeddingDocInstruction,
			}
			analyzer = p.Analyzer
			dimensions = p.EmbeddingDimensions
		}
	}

	// 目标索引名：payload 未指定时回退到 active profile 记录的索引名。
	// 引入动机：自动入队的 rebuild_index 可能不带 index_name（例如"激活 profile"链路），
	// 此时必须能确定"当前正在使用的索引"，才能判断维度是否一致。
	if indexName == "" {
		if activeProfile == nil || activeProfile.ESIndexName == "" {
			return fmt.Errorf("缺少 index_name 且 active profile 未提供 ES 索引名")
		}
		indexName = activeProfile.ESIndexName
	}

	// Phase 6 修复：目标索引不存在时显式创建正确 mapping。
	// 引入动机：原实现依赖 ES auto-create 或外部预建索引，不符合设计要求。
	// 现在通过 IndexExists 检查，不存在时使用 active profile 的 dimensions 和 analyzer
	// 调用 BuildIndexMapping 创建正确 mapping，再 CreateIndex。
	// 索引已存在且维度一致时不重复创建，避免覆盖已有数据。
	exists, err := h.esClient.IndexExists(ctx, indexName)
	if err != nil {
		return fmt.Errorf("检查索引 %s 是否存在: %w", indexName, err)
	}

	// needCreate 表示是否必须新建索引。
	needCreate := !exists
	if exists && dimensions > 0 {
		existingDims, dimsErr := h.esClient.GetIndexDimensions(ctx, indexName)
		if dimsErr != nil {
			return fmt.Errorf("读取索引 %s 的 embedding 维度: %w", indexName, dimsErr)
		}
		if existingDims > 0 && existingDims != dimensions {
			// 维度不一致：旧索引的 dense_vector.dims 不可变，不能继续往它里面写新维度向量。
			//
			// 缺陷 A 修复说明：原实现在这里无条件调用 es.NextIndexName 取"最大版本号 + 1"的新名字，
			// 从不检查集群中是否已存在一个维度正确的索引可复用。线上事故中每次 rebuild 都产出
			// knowledge_v{N+1}，失败后又不回收，索引被堆到 knowledge_v7，而 alias 始终指向最老的
			// knowledge_v1（1024 维）。现在优先复用已存在的、维度正确的索引；只有确实找不到
			// 可复用的索引时才退回"新建下一个版本化索引"。
			reusableIndex, reuseErr := h.findReusableIndex(ctx, dimensions)
			if reuseErr != nil {
				return reuseErr
			}
			if reusableIndex != "" {
				slog.Info("rebuild：复用已存在且维度正确的索引，不再新建",
					"reused_index", reusableIndex, "reused_dims", dimensions,
					"mismatched_index", indexName, "mismatched_dims", existingDims)
				indexName = reusableIndex
				needCreate = false
			} else {
				newIndexName, nextErr := es.NextIndexName(ctx, h.esClient)
				if nextErr != nil {
					return fmt.Errorf("为 %d 维 embedding 计算新索引名: %w", dimensions, nextErr)
				}
				slog.Warn("索引维度与 profile 不一致且无可复用索引，自动创建新索引",
					"old_index", indexName, "old_dims", existingDims,
					"new_index", newIndexName, "profile_dims", dimensions)
				indexName = newIndexName
				needCreate = true
			}
		}
	}

	// createdIndexName 记录"本次 job 自己创建"的索引名；非空表示失败时必须由本函数回收。
	//
	// 缺陷 B 修复说明：原实现把 CreateIndex 放在 alias 切换之前，中间任何一步失败（逐文档索引、
	// integrity check、repair、SwitchAlias）都会把新索引遗留在集群里且 alias 未切换——下一次重试
	// 再建一个，这正是索引失控增长的放大器。复用的索引绝不会进入这里：它可能正是 alias 当前
	// 指向的索引，删除它会直接造成检索不可用。
	createdIndexName := ""

	// recycleCreatedIndexOnFailure 统一回收本次新建的索引，并把原始错误原样返回给 job 层。
	//
	// 引入动机：失败路径分散在逐文档索引、integrity check、repair、SwitchAlias 等多处，
	// 每个 return 点各写一遍回收代码极易漏掉其中一处；集中到此 helper，新增失败分支时按同一模式处理。
	//
	// 约束：
	//   - createdIndexName 为空（复用已有索引 / 本次未创建）时直接返回原始错误，绝不删除任何已有索引；
	//   - 回收失败必须 Error 记录（索引会残留并继续被后续 rebuild 看到），但绝不替换原始错误：
	//     job 层需要看到真实失败原因，才能正确决定重试与人工处置。
	recycleCreatedIndexOnFailure := func(cause error) error {
		if createdIndexName == "" {
			return cause
		}
		if delErr := h.esClient.DeleteIndex(ctx, createdIndexName); delErr != nil {
			slog.Error("rebuild 失败后回收本次新建的索引失败，索引残留在集群中",
				"index", createdIndexName, "cause", cause, "delete_error", delErr)
			return cause
		}
		slog.Warn("rebuild 未完成 alias 切换，已回收本次新建的索引",
			"index", createdIndexName, "cause", cause)
		return cause
	}

	if needCreate {
		// 确保有有效的 analyzer 和 dimensions
		if analyzer == "" {
			analyzer = "standard"
		}
		if dimensions <= 0 {
			// 移除原先的 dimensions = 1024 兜底（引入动机）：静默兜底正是本次维度事故的隐患来源，
			// profile.embedding_dimensions 由 admin 接口校验 > 0，出现 <= 0 属数据异常，必须暴露。
			return fmt.Errorf("profile embedding 维度无效(%d)，无法创建索引 %s", dimensions, indexName)
		}
		// 缺陷 D 修复说明：把"创建前已有的版本化索引数量"一并记入日志。
		// 引入动机：事故期间日志里只有零散的"已创建新索引"，无法直接看出索引数量在失控增长；
		// 有了这个字段，knowledge_v7 这类堆积可以在日志中一眼观察到。统计失败即报错（fail fast），
		// 不做静默降级。
		existingVersioned, listErr := h.listVersionedIndexes(ctx)
		if listErr != nil {
			return listErr
		}
		mapping := es.BuildIndexMapping(dimensions, analyzer)
		if err := h.esClient.CreateIndex(ctx, indexName, mapping); err != nil {
			return fmt.Errorf("创建索引 %s（dimensions=%d, analyzer=%s）: %w", indexName, dimensions, analyzer, err)
		}
		createdIndexName = indexName
		slog.Info("rebuild：已创建新索引", "index", indexName, "dimensions", dimensions,
			"analyzer", analyzer, "existing_versioned_indexes", len(existingVersioned))
	} else {
		slog.Info("rebuild：目标索引已存在，跳过创建", "index", indexName)
	}

	// 构建 embedding 函数
	//
	// 修复说明：当 Provider 已配置（embConfigured 返回 true）但 profileConfig 为 nil 时，
	// 原实现将 embFunc 设为 nil 并静默完成无向量索引。这违反设计要求：
	// "Provider 已配置且调用失败时 index_document 应失败/重试，不能产生无向量 completed job"。
	// 现在当 Provider 已配置但无法生成向量时（profileConfig 为 nil），明确返回错误。
	// embConfigured 仅用于判断是否配置，不执行网络健康检查；正式可用性由 Embed 结果决定。
	var embFunc embeddingProviderFunc
	if h.embEmbed != nil && h.embConfigured != nil && h.embConfigured(ctx) {
		if profileConfig == nil {
			return recycleCreatedIndexOnFailure(fmt.Errorf("embedding provider 已配置但无法获取 active profile 配置，无法生成向量"))
		}
		embFunc = h.embEmbed
	}

	// 从 PG 读取所有非特殊文件、非 archived 文档
	docs, err := readPGDocuments(ctx, h.db, workspaceID)
	if err != nil {
		return recycleCreatedIndexOnFailure(fmt.Errorf("读取 PG 文档: %w", err))
	}

	// 逐文档索引
	//
	// 设计约束：design/01-SEARCH.md §Index Version 要求"验证完整性 → alias 原子切换"。
	// 任何文档索引失败（无论是否有 embedding provider）都意味着新索引不完整，
	// 不应切换 alias 到不完整的新索引。
	//
	// 当 embFunc 不为 nil（Provider 已配置且健康）时，单文档失败立即中止。
	// 当 embFunc 为 nil（Provider 未配置）时，记录失败但继续索引其余文档，
	// 但最终如果有任何失败，不切换 alias。
	indexedCount := 0
	failedCount := 0
	for _, doc := range docs {
		if err := reindexDocumentWithEmbedding(ctx, h.db, h.esClient, indexName, doc.ID, embFunc, profileConfig); err != nil {
			slog.Error("重建索引时文档索引失败", "doc_id", doc.ID, "error", err)
			if embFunc != nil {
				// Provider 已配置但索引失败（含 embedding 失败），必须中止
				return recycleCreatedIndexOnFailure(fmt.Errorf("重建索引时文档 %s 索引失败: %w", doc.ID, err))
			}
			// Provider 未配置时，仅 lexical 索引失败可跳过，但记录失败
			failedCount++
			continue
		}
		indexedCount++
	}

	// 如果有任何文档索引失败（lexical-only 模式），不切换 alias
	// 引入动机：design/01-SEARCH.md §Index Version 要求"验证完整性 → alias 原子切换"，
	// 新索引不完整时切换 alias 会导致搜索结果缺失。保留旧 alias 保证可用性。
	if failedCount > 0 {
		slog.Error("rebuild 有文档索引失败，不切换 alias",
			"index", indexName, "indexed", indexedCount, "failed", failedCount)
		return recycleCreatedIndexOnFailure(fmt.Errorf("rebuild 有 %d 个文档索引失败，不切换 alias（索引不完整）", failedCount))
	}

	// 刷新索引
	if err := h.esClient.Refresh(ctx, indexName); err != nil {
		slog.Warn("刷新索引失败", "index", indexName, "error", err)
	}

	// integrity validation
	// 使用与索引时相同的 chunk 参数计算期望 chunk 数量，避免误报 chunk_count_mismatch
	chunkTargetSize := 0
	chunkOverlap := 0
	if profileConfig != nil {
		chunkTargetSize = profileConfig.ChunkTargetSize
		chunkOverlap = profileConfig.ChunkOverlap
	}
	integrityResult, err := CheckIntegrity(ctx, h.db, h.esClient, indexName, workspaceID, chunkTargetSize, chunkOverlap)
	if err != nil {
		slog.Error("rebuild 后 integrity check 失败", "index", indexName, "error", err)
		return recycleCreatedIndexOnFailure(fmt.Errorf("rebuild 后 integrity check: %w", err))
	}
	if !integrityResult.CheckedOK {
		return recycleCreatedIndexOnFailure(fmt.Errorf("rebuild 后 integrity check 未完成，ES 可能不可用"))
	}
	if len(integrityResult.Issues) > 0 {
		slog.Warn("rebuild 后发现 integrity 问题", "index", indexName, "issues", len(integrityResult.Issues))
		// 修复发现的问题，使用与 rebuild 相同的 embedding 配置
		// 引入动机：design/01-SEARCH.md §Index Version 要求"验证完整性 → alias 原子切换"，
		// 修复失败意味着完整性未通过验证，不应切换 alias。
		repairFailed := 0
		for _, issue := range integrityResult.Issues {
			if err := RepairIssue(ctx, h.db, h.esClient, indexName, issue, embFunc, profileConfig); err != nil {
				slog.Error("rebuild 后修复问题失败", "type", issue.Type, "doc_id", issue.DocumentID, "error", err)
				repairFailed++
			}
		}
		if repairFailed > 0 {
			slog.Error("rebuild 后 integrity 修复失败，不切换 alias",
				"index", indexName, "total_issues", len(integrityResult.Issues), "repair_failed", repairFailed)
			return recycleCreatedIndexOnFailure(fmt.Errorf("rebuild 后 %d 个 integrity 问题修复失败，不切换 alias", repairFailed))
		}
	}

	// alias 原子切换
	// 引入动机：design/01-SEARCH.md §Index Version 要求 alias 原子切换，
	// 保留旧索引以支持 rollback。新索引构建失败不切换旧 alias。
	if err := es.SwitchAlias(ctx, h.esClient, es.AliasName, indexName); err != nil {
		slog.Error("alias 切换失败，保留旧 alias 和旧索引", "index", indexName, "error", err)
		// alias 未切换成功，本次新建的索引没有被任何 alias 引用，必须回收（复用的索引不受影响）。
		return recycleCreatedIndexOnFailure(fmt.Errorf("alias 切换失败: %w", err))
	}

	// 回写 profile 的 es_index_name。
	//
	// 引入动机：维度不一致时 rebuild 会创建并切换到新索引（如 knowledge_v1 → knowledge_v2）。
	// 若不把新索引名写回 profile，profile 记录与 alias 实际指向不一致，后续 rebuild/rollback
	// 会再次基于旧索引名选错目标。因此回写失败必须让 job 失败，绝不吞掉。
	if h.profileRepo != nil {
		writebackProfileID := profileID
		if writebackProfileID == "" && activeProfile != nil {
			writebackProfileID = activeProfile.ID
		}
		if writebackProfileID == "" {
			// 无法确定 profile 就无从回写，用 Warn 让该情况在日志中可见，而不是完全静默。
			slog.Warn("rebuild 完成但无法确定 profile_id，跳过 es_index_name 回写",
				"index", indexName, "payload_profile_id", profileID)
		} else if err := h.profileRepo.UpdateProfileESIndex(ctx, writebackProfileID, indexName); err != nil {
			return fmt.Errorf("回写 profile %s 的 es_index_name=%s: %w", writebackProfileID, indexName, err)
		}
	}

	slog.Info("全量重建索引完成", "index", indexName, "documents", len(docs), "indexed", indexedCount, "failed", failedCount, "profile_id", profileID)
	return nil
}

// handleRepairIndex 处理索引修复任务。
func (h *IndexJobHandler) handleRepairIndex(ctx context.Context, job *Job) error {
	indexName, _ := job.Payload["index_name"].(string)
	workspaceID, _ := job.Payload["workspace_id"].(string)
	if indexName == "" {
		indexName = es.AliasName
	}

	// 获取 profile 配置以确定 chunk 参数和 embedding 配置
	var profileConfig *types.SearchProfileConfig
	var analyzer string
	var dimensions int
	if h.profileRepo != nil {
		p, err := h.profileRepo.GetActiveProfile(ctx)
		if err == nil && p != nil {
			profileConfig = &types.SearchProfileConfig{
				ChunkTargetSize:           p.ChunkTargetSize,
				ChunkOverlap:              p.ChunkOverlap,
				EmbeddingDimensions:       p.EmbeddingDimensions,
				EmbeddingQueryInstruction: p.EmbeddingQueryInstruction,
				EmbeddingDocInstruction:   p.EmbeddingDocInstruction,
			}
			analyzer = p.Analyzer
			dimensions = p.EmbeddingDimensions
		}
	}
	_ = analyzer
	_ = dimensions

	// 构建 embedding 函数
	var embFunc embeddingProviderFunc
	if h.embEmbed != nil && h.embConfigured != nil && h.embConfigured(ctx) {
		if profileConfig == nil {
			return fmt.Errorf("embedding provider 已配置但无法获取 active profile 配置，无法生成向量")
		}
		embFunc = h.embEmbed
	}

	// 执行完整性检查，使用 profile 的 chunk 参数
	chunkTargetSize := 0
	chunkOverlap := 0
	if profileConfig != nil {
		chunkTargetSize = profileConfig.ChunkTargetSize
		chunkOverlap = profileConfig.ChunkOverlap
	}
	result, err := CheckIntegrity(ctx, h.db, h.esClient, indexName, workspaceID, chunkTargetSize, chunkOverlap)
	if err != nil {
		return fmt.Errorf("完整性检查: %w", err)
	}

	if !result.CheckedOK {
		return fmt.Errorf("完整性检查未完成，ES 可能不可用")
	}

	// 修复每个问题，使用与索引相同的 embedding 配置
	for _, issue := range result.Issues {
		if err := RepairIssue(ctx, h.db, h.esClient, indexName, issue, embFunc, profileConfig); err != nil {
			slog.Error("修复问题失败", "type", issue.Type, "doc_id", issue.DocumentID, "error", err)
		}
	}

	slog.Info("索引修复完成", "index", indexName, "issues_found", len(result.Issues))
	return nil
}

// handleCleanupOldIndexes 处理清理旧索引任务。
// 引入动机：C8 要求实现有限、确定的旧版本索引清理：
// 列出 versioned 索引，排除 current alias 与需保留 rollback 的 index，
// 遵循配置/明确定义的保留数，真正调用 DeleteIndex。
var defaultKeepOldIndexes = 2

func (h *IndexJobHandler) handleCleanupOldIndexes(ctx context.Context, job *Job) error {
	keepCount, _ := job.Payload["keep_count"].(float64)
	if keepCount <= 0 {
		keepCount = float64(defaultKeepOldIndexes)
	}

	// 获取当前 alias 指向的索引
	currentIndex, err := h.esClient.GetAliasIndex(ctx, es.AliasName)
	if err != nil {
		return fmt.Errorf("获取当前 alias 索引: %w", err)
	}

	// 列出所有 knowledge_v* 索引
	indices, err := h.esClient.ListIndices(ctx, es.IndexNamePrefix+"*")
	if err != nil {
		return fmt.Errorf("列出索引: %w", err)
	}

	// 排除当前 alias 指向的索引
	var oldIndices []string
	for _, idx := range indices {
		if idx == currentIndex {
			continue
		}
		oldIndices = append(oldIndices, idx)
	}

	// 按名称排序（版本号大的在前）
	// 引入动机：保留较新的旧索引以支持 rollback
	sortIndicesDesc(oldIndices)

	// 保留 keepCount 个旧索引，删除其余
	deletedCount := 0
	for i := int(keepCount); i < len(oldIndices); i++ {
		idx := oldIndices[i]
		if err := h.esClient.DeleteIndex(ctx, idx); err != nil {
			slog.Error("删除旧索引失败", "index", idx, "error", err)
			// 继续删除其他索引，不因单个失败而中止
			continue
		}
		deletedCount++
		slog.Info("旧索引已删除", "index", idx)
	}

	slog.Info("清理旧索引完成", "current_index", currentIndex, "old_count", len(oldIndices), "kept", int(keepCount), "deleted", deletedCount)
	return nil
}

// sortIndicesDesc 按索引名称降序排序。
// 引入动机：cleanup 需要保留较新的旧索引，按名称降序排列后取前 N 个保留。
func sortIndicesDesc(indices []string) {
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] > indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}
}

// handleCleanupRevisions 处理清理过期 revision 任务。
// 引入动机：design/03-DOCUMENTS.md §Revision 要求定时清理历史 snapshot，
// design/05-OPERATIONS.md §Background Jobs 列出 cleanup_revisions 任务类型。
// 读取 workspace 的 retention 配置，对指定文档（或全部文档）执行 revision 清理。
func (h *IndexJobHandler) handleCleanupRevisions(ctx context.Context, job *Job) error {
	if h.revisionCleanupRepo == nil {
		return fmt.Errorf("cleanup_revisions job 需要 revisionCleanupRepo，但未注入")
	}

	workspaceID, _ := job.Payload["workspace_id"].(string)
	if workspaceID == "" {
		return fmt.Errorf("缺少 workspace_id")
	}

	// 获取 workspace retention 配置
	retentionDays, maxCount, err := h.revisionCleanupRepo.GetWorkspaceRetentionConfig(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("查询 workspace retention 配置: %w", err)
	}

	// 如果 payload 指定了 document_id，只清理该文档
	if docID, ok := job.Payload["document_id"].(string); ok && docID != "" {
		cleaned, err := h.revisionCleanupRepo.CleanupRevisions(ctx, workspaceID, docID, retentionDays, maxCount)
		if err != nil {
			return fmt.Errorf("清理文档 %s 的 revision: %w", docID, err)
		}
		slog.Info("revision 清理完成", "workspace_id", workspaceID, "document_id", docID, "cleaned", cleaned)
		return nil
	}

	// 未指定 document_id：遍历 workspace 下所有文档执行清理
	rows, err := h.db.QueryContext(ctx,
		`SELECT id FROM documents WHERE workspace_id = $1`,
		workspaceID,
	)
	if err != nil {
		return fmt.Errorf("查询 workspace 文档列表: %w", err)
	}
	defer rows.Close()

	totalCleaned := 0
	for rows.Next() {
		var docID string
		if err := rows.Scan(&docID); err != nil {
			return fmt.Errorf("扫描文档 ID: %w", err)
		}
		cleaned, err := h.revisionCleanupRepo.CleanupRevisions(ctx, workspaceID, docID, retentionDays, maxCount)
		if err != nil {
			slog.Error("清理文档 revision 失败", "workspace_id", workspaceID, "document_id", docID, "error", err)
			continue
		}
		totalCleaned += cleaned
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历文档列表: %w", err)
	}

	slog.Info("workspace revision 清理完成", "workspace_id", workspaceID, "total_cleaned", totalCleaned)
	return nil
}
