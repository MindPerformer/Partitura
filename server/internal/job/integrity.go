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
	Issues     []IntegrityIssue
	TotalPG    int
	TotalES    int
	CheckedOK  bool
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
					"field":  "document_id",
					"size":   100000, // 足够大的 bucket 数量
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
	db        *sql.DB
	esClient  es.Client
	// embEmbed 用于生成 embedding 向量（C5）
	embEmbed  func(ctx context.Context, texts []string) ([][]float32, error)
	// embAvailable 检查 embedding provider 是否可用
	embAvailable func(ctx context.Context) bool
	// profileRepo 用于获取 active profile
	profileRepo ProfileRepo
	// revisionCleanupRepo 用于清理过期 revision
	// 引入动机：cleanup_revisions job 需要调用 document.CleanupRevisions，
	// 使用接口避免 job 包对 document 包的直接依赖。
	revisionCleanupRepo RevisionCleanupRepo
}

// ProfileRepo 定义获取 active profile 的接口。
// 引入动机：index job 需要获取 active profile 以确定 chunk 参数和 embedding 配置。
type ProfileRepo interface {
	GetActiveProfile(ctx context.Context) (*ProfileForJob, error)
}

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
	ID                   string
	ESIndexName          string
	ChunkTargetSize      int
	ChunkOverlap         int
	EmbeddingDimensions  int
	EmbeddingQueryInstruction string
	EmbeddingDocInstruction   string
	// Analyzer 是 ES 索引的分析器名称（如 "standard"），用于创建 index mapping。
	Analyzer                  string
}

// NewIndexJobHandler 创建索引任务处理器。
// 引入动机：C5 要求 index job 实际生成 embedding，需要注入 embedding provider 和 profile repo。
// embEmbed 和 embAvailable 从 embedding.Provider 接口适配，避免 job 包导入 embedding 包。
// revisionCleanupRepo 用于 cleanup_revisions job，可为 nil（当 revision 清理未启用时）。
func NewIndexJobHandler(db *sql.DB, esClient es.Client, embEmbed func(ctx context.Context, texts []string) ([][]float32, error), embAvailable func(ctx context.Context) bool, profileRepo ProfileRepo, revisionCleanupRepo RevisionCleanupRepo) *IndexJobHandler {
	return &IndexJobHandler{
		db:                  db,
		esClient:            esClient,
		embEmbed:            embEmbed,
		embAvailable:        embAvailable,
		profileRepo:         profileRepo,
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
		}
	}

	// 确保 alias 和目标索引存在。
	// 引入动机：当 indexName 是 alias 且不存在时，直接写入会导致 ES 自动创建
	// 同名具体索引（动态 mapping，无 embedding 字段）。必须先创建正确的
	// versioned index 和 alias，再写入数据。
	if indexName == es.AliasName {
		if err := h.ensureAliasAndIndex(ctx, dimensions, analyzer); err != nil {
			return fmt.Errorf("确保索引和 alias 存在: %w", err)
		}
	}

	// 构建 embedding 函数
	//
	// 修复说明：当 Provider 健康（embAvailable 返回 true）但 profileConfig 为 nil 时，
	// 原实现将 embFunc 设为 nil 并静默完成无向量索引。这违反设计要求：
	// "index_document 不得在 Provider 健康却未生成向量时静默完成"。
	// 现在当 Provider 健康但无法生成向量时（profileConfig 为 nil），明确返回错误。
	var embFunc embeddingProviderFunc
	if h.embEmbed != nil && h.embAvailable != nil && h.embAvailable(ctx) {
		if profileConfig == nil {
			return fmt.Errorf("embedding provider 健康但无法获取 active profile 配置，无法生成向量")
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
//   - 如果 alias 已存在，不做任何操作（alias 可能指向已有 index）。
//   - 如果 alias 不存在但存在同名具体索引，先删除该索引再创建 alias。
//   - 创建新 index 时使用 active profile 的 dimensions 和 analyzer。
func (h *IndexJobHandler) ensureAliasAndIndex(ctx context.Context, dimensions int, analyzer string) error {
	// 检查 alias 是否已存在
	_, aliasErr := h.esClient.GetAliasIndex(ctx, es.AliasName)
	if aliasErr == nil {
		// alias 已存在，无需操作
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
		dimensions = 1024
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

// handleRebuildIndex 处理全量重建索引任务。
// 引入动机：C7 要求 rebuild 基于新 profile 创建新 versioned index → reindex → refresh →
// integrity validation → alias 原子切换 → PG 持久 profile indexname/status。
//
// Phase 6 修复：
// - 目标索引不存在时，使用 active profile 的 dimensions 和 analyzer 显式创建正确 mapping，
//   绝不依赖 ES auto-create 或手动预建。
// - 索引已存在时不重复创建，直接使用。
// - alias 切换使用 ES8 兼容的嵌套 JSON 格式（通过 AliasAction.MarshalJSON 实现）。
func (h *IndexJobHandler) handleRebuildIndex(ctx context.Context, job *Job) error {
	indexName, _ := job.Payload["index_name"].(string)
	workspaceID, _ := job.Payload["workspace_id"].(string)
	profileID, _ := job.Payload["profile_id"].(string)

	if indexName == "" {
		return fmt.Errorf("缺少 index_name")
	}

	// 获取 profile 配置
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

	// Phase 6 修复：目标索引不存在时显式创建正确 mapping。
	// 引入动机：原实现依赖 ES auto-create 或外部预建索引，不符合设计要求。
	// 现在通过 IndexExists 检查，不存在时使用 active profile 的 dimensions 和 analyzer
	// 调用 BuildIndexMapping 创建正确 mapping，再 CreateIndex。
	// 索引已存在时不重复创建，避免覆盖已有数据。
	exists, err := h.esClient.IndexExists(ctx, indexName)
	if err != nil {
		return fmt.Errorf("检查索引 %s 是否存在: %w", indexName, err)
	}
	if !exists {
		// 确保有有效的 analyzer 和 dimensions
		if analyzer == "" {
			analyzer = "standard"
		}
		if dimensions <= 0 {
			dimensions = 1024
		}
		mapping := es.BuildIndexMapping(dimensions, analyzer)
		if err := h.esClient.CreateIndex(ctx, indexName, mapping); err != nil {
			return fmt.Errorf("创建索引 %s（dimensions=%d, analyzer=%s）: %w", indexName, dimensions, analyzer, err)
		}
		slog.Info("rebuild：已创建新索引", "index", indexName, "dimensions", dimensions, "analyzer", analyzer)
	} else {
		slog.Info("rebuild：目标索引已存在，跳过创建", "index", indexName)
	}

	// 构建 embedding 函数
	//
	// 修复说明：当 Provider 健康（embAvailable 返回 true）但 profileConfig 为 nil 时，
	// 原实现将 embFunc 设为 nil 并静默完成无向量索引。这违反设计要求：
	// "Provider 已配置且调用失败时 index_document 应失败/重试，不能产生无向量 completed job"。
	// 现在当 Provider 健康但无法生成向量时（profileConfig 为 nil），明确返回错误。
	var embFunc embeddingProviderFunc
	if h.embEmbed != nil && h.embAvailable != nil && h.embAvailable(ctx) {
		if profileConfig == nil {
			return fmt.Errorf("embedding provider 健康但无法获取 active profile 配置，无法生成向量")
		}
		embFunc = h.embEmbed
	}

	// 从 PG 读取所有非特殊文件、非 archived 文档
	docs, err := readPGDocuments(ctx, h.db, workspaceID)
	if err != nil {
		return fmt.Errorf("读取 PG 文档: %w", err)
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
				return fmt.Errorf("重建索引时文档 %s 索引失败: %w", doc.ID, err)
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
		return fmt.Errorf("rebuild 有 %d 个文档索引失败，不切换 alias（索引不完整）", failedCount)
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
		return fmt.Errorf("rebuild 后 integrity check: %w", err)
	}
	if !integrityResult.CheckedOK {
		return fmt.Errorf("rebuild 后 integrity check 未完成，ES 可能不可用")
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
			return fmt.Errorf("rebuild 后 %d 个 integrity 问题修复失败，不切换 alias", repairFailed)
		}
	}

	// alias 原子切换
	// 引入动机：design/01-SEARCH.md §Index Version 要求 alias 原子切换，
	// 保留旧索引以支持 rollback。新索引构建失败不切换旧 alias。
	if err := es.SwitchAlias(ctx, h.esClient, es.AliasName, indexName); err != nil {
		slog.Error("alias 切换失败，保留旧 alias 和旧索引", "index", indexName, "error", err)
		return fmt.Errorf("alias 切换失败: %w", err)
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
	if h.embEmbed != nil && h.embAvailable != nil && h.embAvailable(ctx) {
		if profileConfig == nil {
			return fmt.Errorf("embedding provider 健康但无法获取 active profile 配置，无法生成向量")
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
