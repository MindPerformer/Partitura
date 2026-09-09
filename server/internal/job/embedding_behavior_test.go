// embedding_behavior_test.go 测试 embedding provider 失败时 job 的行为。
//
// 引入动机：远程验收发现 rebuild/index 流程会跳过失败文档仍标记成功，
// 造成 with_embedding=false 且 chunk 无向量。修复后必须：
//   - Provider 已配置且调用失败时 index/rebuild job 不能静默完成或写无向量 chunk
//   - 必须返回可重试错误
//   - 向量维度必须匹配 active profile
//
// 本测试使用 fake ES client、fake profile repo 和可控的 embEmbed/embAvailable 闭包，
// 不依赖真实 PG（部分测试）或真实外部 API。
package job

import (
	"context"
	"fmt"
	"testing"

	types "partitura/server/internal/search/types"
)

// TestHandleIndexDocument_EmbeddingFailure_ReturnsError 验证 index_document job
// 在 Provider 健康（embAvailable 返回 true）但 embEmbed 调用失败时，
// 返回可重试错误，不静默完成，不写无向量 chunk。
//
// 引入动机：设计要求 "embedding 不可用时 indexing job 重试"，不能 silent fallback。
func TestHandleIndexDocument_EmbeddingFailure_ReturnsError(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "emb_fail_index", "Emb Fail Index",
		"# Emb Fail\n\nContent for embedding failure test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	// Provider 健康（embAvailable 返回 true）但 embEmbed 调用失败
	embAvailable := func(ctx context.Context) bool { return true }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		return nil, fmt.Errorf("embedding API 调用失败: 连接超时")
	}

	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			"index_name":  "test_emb_fail_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("Provider 健康但 embEmbed 调用失败时 index_document 应返回错误")
	}

	// 错误信息应包含 embedding 相关信息
	if !containsStr(err.Error(), "embedding") {
		t.Errorf("错误信息应包含 'embedding'，实际: %s", err.Error())
	}

	// 不应写入任何 chunk（embedding 失败导致整个操作失败）
	if len(fakeES.bulkIndexCalls) != 0 {
		t.Errorf("embedding 失败时不应执行 BulkIndex，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// TestHandleRebuildIndex_EmbeddingFailure_Aborts 验证 rebuild_index job
// 在 Provider 健康（embAvailable 返回 true）但 embEmbed 调用失败时，
// 中止整个 rebuild 并返回错误，不继续处理其他文档，不静默完成。
//
// 引入动机：原实现在 rebuild 时单个文档索引失败仅 continue 跳过，
// 最终返回 nil（completed），造成无向量 completed job。修复后必须中止。
func TestHandleRebuildIndex_EmbeddingFailure_Aborts(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 创建两个测试文档
	wsID1, _ := setupTestDocument(t, db, "rebuild_emb_fail_1", "Rebuild Emb Fail 1",
		"# Doc 1\n\nContent for rebuild embedding failure test 1.\n")
	wsID2, _ := setupTestDocument(t, db, "rebuild_emb_fail_2", "Rebuild Emb Fail 2",
		"# Doc 2\n\nContent for rebuild embedding failure test 2.\n")
	_ = wsID1
	_ = wsID2

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	// Provider 健康但 embEmbed 调用失败
	embAvailable := func(ctx context.Context) bool { return true }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		return nil, fmt.Errorf("embedding API 503: service unavailable")
	}

	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobRebuildIndex,
		Payload: map[string]interface{}{
			"index_name": "test_rebuild_emb_fail",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("Provider 健康但 embEmbed 调用失败时 rebuild 应返回错误")
	}

	// 错误信息应包含 embedding 或索引失败相关信息
	if !containsStr(err.Error(), "embedding") && !containsStr(err.Error(), "索引失败") {
		t.Errorf("错误信息应包含 embedding 或索引失败信息，实际: %s", err.Error())
	}
}

// TestHandleIndexDocument_DimensionMismatch_Fails 验证 index_document job
// 在 embedding 返回的向量维度与 active profile 配置不匹配时返回错误。
//
// 引入动机：向量维度必须匹配 active profile，错误维度必须失败。
func TestHandleIndexDocument_DimensionMismatch_Fails(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "dim_mismatch_test", "Dim Mismatch",
		"# Dim Mismatch\n\nContent for dimension mismatch test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	// Profile 配置维度为 1024
	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	// Provider 返回 512 维向量（与 profile 的 1024 不匹配）
	embAvailable := func(ctx context.Context) bool { return true }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i := range texts {
			vectors[i] = make([]float32, 512) // 错误维度
		}
		return vectors, nil
	}

	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			"index_name":  "test_dim_mismatch_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("向量维度不匹配时应返回错误")
	}

	// 错误信息应包含维度信息
	if !containsStr(err.Error(), "维度") && !containsStr(err.Error(), "dimension") {
		t.Errorf("错误信息应包含维度/dimension，实际: %s", err.Error())
	}

	// 不应写入任何 chunk
	if len(fakeES.bulkIndexCalls) != 0 {
		t.Errorf("维度不匹配时不应执行 BulkIndex，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// TestHandleIndexDocument_CorrectDimensions_WritesChunks 验证 index_document job
// 在 embedding 成功返回正确维度向量时，chunk 包含 embedding 字段。
//
// 引入动机：验证正常路径——JSON 成功响应的向量进入 chunk。
func TestHandleIndexDocument_CorrectDimensions_WritesChunks(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "correct_dim_test", "Correct Dim",
		"# Correct Dim\n\nContent for correct dimensions test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	dimensions := 4096
	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: dimensions,
			Analyzer:            "standard",
		},
	}

	// Provider 返回正确维度的向量
	embAvailable := func(ctx context.Context) bool { return true }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		vectors := make([][]float32, len(texts))
		for i := range texts {
			vectors[i] = make([]float32, dimensions)
			for j := range vectors[i] {
				vectors[i][j] = 0.01
			}
		}
		return vectors, nil
	}

	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			"index_name":  "test_correct_dim_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("embedding 成功时 index_document 应成功: %v", err)
	}

	// 验证 chunk 包含 embedding 字段
	if len(fakeES.bulkIndexCalls) == 0 {
		t.Fatal("应执行 BulkIndex")
	}
	for _, bulkCall := range fakeES.bulkIndexCalls {
		for _, doc := range bulkCall.Docs {
			emb, hasEmb := doc.Body["embedding"]
			if !hasEmb {
				t.Error("chunk 应包含 embedding 字段")
				continue
			}
			embSlice, ok := emb.([]float32)
			if !ok {
				t.Errorf("embedding 类型应为 []float32，实际: %T", emb)
				continue
			}
			if len(embSlice) != dimensions {
				t.Errorf("embedding 维度应为 %d，实际: %d", dimensions, len(embSlice))
			}
		}
	}
}

// TestReindexDocumentWithEmbedding_ProviderButNoProfile_Fails 验证
// reindexDocumentWithEmbedding 在 embProvider 非 nil 但 profileConfig 为 nil 时
// 返回错误，不静默写入无向量 chunk。
//
// 引入动机：防御性检查——即使调用方未正确拦截此情况，
// reindexDocumentWithEmbedding 自身也必须安全失败。
func TestReindexDocumentWithEmbedding_ProviderButNoProfile_Fails(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "provider_no_profile", "Provider No Profile",
		"# Test\n\nContent for provider no profile test.\n")
	_ = wsID

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_guard_index", nil)

	// embProvider 非 nil 但 profileConfig 为 nil
	embProvider := func(ctx context.Context, texts []string) ([][]float32, error) {
		return nil, fmt.Errorf("should not be called")
	}

	err := reindexDocumentWithEmbedding(ctx, db, fakeES, "test_guard_index", docID,
		embProvider, nil)
	if err == nil {
		t.Fatal("embProvider 非 nil 但 profileConfig 为 nil 时应返回错误")
	}

	if !containsStr(err.Error(), "profileConfig") && !containsStr(err.Error(), "向量") {
		t.Errorf("错误信息应涉及 profileConfig 或向量，实际: %s", err.Error())
	}

	// 不应写入任何 chunk
	if len(fakeES.bulkIndexCalls) != 0 {
		t.Errorf("防御性检查失败时不应执行 BulkIndex，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// TestHandleRebuildIndex_NoProvider_LexicalOnly 验证 rebuild_index job
// 在 Provider 未配置（embAvailable 返回 false）时，仅写入 lexical 字段，
// 允许继续并最终成功。
//
// 引入动机：Provider 未配置时 lexical-only 降级是设计允许的，
// 不应因此失败。只有 Provider 已配置但调用失败时才必须报错。
func TestHandleRebuildIndex_NoProvider_LexicalOnly(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, _ := setupTestDocument(t, db, "no_provider_rebuild", "No Provider Rebuild",
		"# No Provider\n\nContent for no provider rebuild test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	// Provider 未配置
	embAvailable := func(ctx context.Context) bool { return false }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		t.Error("Provider 未配置时 embEmbed 不应被调用")
		return nil, fmt.Errorf("should not be called")
	}

	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobRebuildIndex,
		Payload: map[string]interface{}{
			"index_name": "test_no_provider_rebuild",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("Provider 未配置时 rebuild 应成功（lexical-only 降级）: %v", err)
	}

	// 验证 chunk 不包含 embedding 字段
	for _, bulkCall := range fakeES.bulkIndexCalls {
		for _, doc := range bulkCall.Docs {
			if _, hasEmb := doc.Body["embedding"]; hasEmb {
				t.Error("Provider 未配置时 chunk 不应包含 embedding 字段")
			}
		}
	}
}

// TestHandleIndexDocument_NoProvider_LexicalOnly 验证 index_document job
// 在 Provider 未配置时仅写入 lexical 字段，不报错。
func TestHandleIndexDocument_NoProvider_LexicalOnly(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "no_provider_index", "No Provider Index",
		"# No Provider\n\nContent for no provider index test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	// Provider 未配置
	embAvailable := func(ctx context.Context) bool { return false }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		t.Error("Provider 未配置时 embEmbed 不应被调用")
		return nil, fmt.Errorf("should not be called")
	}

	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			"index_name":  "test_no_provider_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("Provider 未配置时 index_document 应成功（lexical-only 降级）: %v", err)
	}

	// 验证 chunk 不包含 embedding 字段
	for _, bulkCall := range fakeES.bulkIndexCalls {
		for _, doc := range bulkCall.Docs {
			if _, hasEmb := doc.Body["embedding"]; hasEmb {
				t.Error("Provider 未配置时 chunk 不应包含 embedding 字段")
			}
		}
	}
}

// TestHandleRebuildIndex_ESDeleteByQueryPreserved 验证 rebuild 流程中
// reindexDocumentWithEmbedding 的 DeleteByQuery 仍被正确调用（保留已有精确错误处理）。
//
// 引入动机：任务要求保留已有 ES DeleteByQuery 和 SwitchAlias 精确错误处理。
func TestHandleRebuildIndex_ESDeleteByQueryPreserved(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, _ := setupTestDocument(t, db, "delbyquery_test", "DelByQuery Test",
		"# DelByQuery\n\nContent for DeleteByQuery test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	handler := NewIndexJobHandler(db, fakeES, nil, nil, fakeRepo, nil)

	job := &Job{
		Type: types.JobRebuildIndex,
		Payload: map[string]interface{}{
			"index_name": "test_delbyquery_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("rebuild 应成功: %v", err)
	}

	// 验证 DeleteByQuery 被调用（reindexDocumentWithEmbedding 先删旧 chunk）
	if len(fakeES.deleteByQueryCalls) == 0 {
		t.Error("rebuild 应调用 DeleteByQuery 删除旧 chunk")
	}
}

// TestHandleIndexDocument_AliasSwitchPreserved 验证 index_document job
// 在 alias 不存在时创建 alias 的 SwitchAlias 调用仍正确工作。
//
// 引入动机：任务要求保留已有 SwitchAlias 精确错误处理。
func TestHandleIndexDocument_AliasSwitchPreserved(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "alias_switch_test", "Alias Switch",
		"# Alias Switch\n\nContent for alias switch test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	handler := NewIndexJobHandler(db, fakeES, nil, nil, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
		},
	}

	err := handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("index_document 应成功: %v", err)
	}

	// 验证 alias 被创建并指向 versioned index
	if fakeES.aliasIndex == "" {
		t.Error("alias 应被创建")
	}
	if !fakeES.indices[fakeES.aliasIndex] {
		t.Errorf("alias 指向的索引 %s 应存在", fakeES.aliasIndex)
	}
}

// TestHandleIndexDocument_EmbeddingCountMismatch_Fails 验证 index_document job
// 在 embedding 返回的向量数量与 chunk 数量不匹配时返回错误，不写无向量 chunk。
//
// 引入动机：Provider 可能因限流或内部错误返回部分向量，job 层必须检测数量不匹配
// 并返回可重试错误，不能静默写入不完整的 chunk。
func TestHandleIndexDocument_EmbeddingCountMismatch_Fails(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "count_mismatch_test", "Count Mismatch",
		"# Count Mismatch\n\nContent for count mismatch test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	// Provider 返回的向量数量少于 chunk 数量
	embAvailable := func(ctx context.Context) bool { return true }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		// 只返回 1 个向量，无论输入有多少
		vectors := make([][]float32, 1)
		vectors[0] = make([]float32, 1024)
		return vectors, nil
	}

	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			"index_name":  "test_count_mismatch_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("向量数量不匹配时应返回错误")
	}

	if !containsStr(err.Error(), "不匹配") {
		t.Errorf("错误信息应包含数量不匹配提示，实际: %s", err.Error())
	}

	// 不应写入任何 chunk
	if len(fakeES.bulkIndexCalls) != 0 {
		t.Errorf("向量数量不匹配时不应执行 BulkIndex，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// TestHandleRebuildIndex_EmptyWorkspace_Succeeds 验证 rebuild_index job
// 在 workspace 无文档时成功完成，不因空文档列表失败。
func TestHandleRebuildIndex_EmptyWorkspace_Succeeds(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 创建 workspace 但不创建文档
	var userID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO users (username, email, password_hash, system_role) VALUES ($1, $2, $3, 'system_admin') RETURNING id`,
		"test_empty_ws_user", "test_empty_ws_user@example.com", "test_hash",
	).Scan(&userID)
	if err != nil {
		t.Fatalf("创建测试用户: %v", err)
	}
	var wsID string
	err = db.QueryRowContext(ctx,
		`INSERT INTO workspaces (name, display_name, max_document_size_bytes, revision_retention_days, revision_max_count, created_by)
		 VALUES ($1, $2, 2097152, 365, 30, $3) RETURNING id`,
		"test_empty_ws", "Empty WS", userID,
	).Scan(&wsID)
	if err != nil {
		t.Fatalf("创建测试 workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, wsID)
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	handler := NewIndexJobHandler(db, fakeES, nil, nil, fakeRepo, nil)

	job := &Job{
		Type: types.JobRebuildIndex,
		Payload: map[string]interface{}{
			"index_name":   "test_empty_ws_index",
			"workspace_id": wsID,
		},
	}

	err = handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("空 workspace rebuild 应成功: %v", err)
	}
}
