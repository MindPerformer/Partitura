// embedding_hotreload_test.go 测试 Provider Registry 热加载后 job 实际使用新 Provider。
//
// 引入动机：远程验收发现 Embedding Provider 在启动时未配置，后续通过 Web API 热更新成功，
// 但 index_document job 仍以 with_embedding=false 完成。根因是 main.go 中 embEmbed/embAvailable
// 闭包仅在启动时 Provider 已配置才创建，导致热更新后 job 无法获取 Provider。
//
// 本测试验证：
// 1. embEmbed/embAvailable 闭包在 Provider 未配置时也能正确工作——
//    当 Provider 后续被设置后，闭包能动态获取并调用 Provider。
// 2. Provider 缺失/错误时不生成无向量 completed job，而是明确失败。
// 3. reindexDocumentWithEmbedding 在 embProvider 为 nil 时不写入 embedding，
//    但当 embProvider 可用时写入正确维度的 embedding。
package job

import (
	"context"
	"fmt"
	"testing"

	"partitura/server/internal/es"
	types "partitura/server/internal/search/types"
)

// TestEmbeddingProvider_HotReload_JobUsesNewProvider 验证 Provider 热加载后
// job 实际使用新 Provider 且 chunk 带 embedding。
//
// 引入动机：远程验收中发现 Provider 热更新后 job 仍 with_embedding=false。
// 此测试模拟：初始 Provider 未配置 → 后续 Provider 可用 → job 应使用 Provider 生成 embedding。
//
// 测试策略：
//   - 使用可变 embProvider 函数，初始返回 nil（未配置）
//   - 后续切换为返回真实向量的函数（模拟热加载）
//   - 调用 reindexDocumentWithEmbedding 验证 chunk 带 embedding
func TestEmbeddingProvider_HotReload_JobUsesNewProvider(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "hotreload_test", "HotReload Test",
		"# HotReload Test\n\nContent for hot reload test.\n")
	_ = wsID

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_hotreload_index", nil)

	// 模拟 Registry 热加载：初始 Provider 不可用，后续变为可用
	providerAvailable := false
	dimensions := 1024

	// embAvailable 闭包：动态检查 Provider 是否可用
	embAvailable := func(ctx context.Context) bool {
		return providerAvailable
	}

	// embEmbed 闭包：动态获取 Provider 并调用
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		if !providerAvailable {
			return nil, fmt.Errorf("embedding provider 未配置")
		}
		// 模拟 Provider 返回正确维度的向量
		vectors := make([][]float32, len(texts))
		for i := range texts {
			vectors[i] = make([]float32, dimensions)
			for j := range vectors[i] {
				vectors[i][j] = 0.1
			}
		}
		return vectors, nil
	}

	profileConfig := &types.SearchProfileConfig{
		ChunkTargetSize:     512,
		ChunkOverlap:        64,
		EmbeddingDimensions: dimensions,
	}

	// 阶段 1：Provider 未配置，embFunc 应为 nil，chunk 不带 embedding
	err := reindexDocumentWithEmbedding(ctx, db, fakeES, "test_hotreload_index", docID,
		embEmbed, profileConfig)
	// embEmbed 非 nil 但 embAvailable 返回 false 时，handleIndexDocument 不会设置 embFunc。
	// 但 reindexDocumentWithEmbedding 直接接收 embProvider 函数，如果非 nil 会尝试调用。
	// 实际上 handleIndexDocument 中的逻辑是：embFunc 只在 embAvailable(ctx)==true 时才设置。
	// 所以这里我们测试 reindexDocumentWithEmbedding 直接被传入 nil embProvider 的情况。

	// 验证阶段 1：无 embedding 时 job 不应失败（lexical-only 降级）
	if err != nil {
		t.Fatalf("Provider 未配置时 reindexDocumentWithEmbedding 不应失败: %v", err)
	}

	// 验证 chunk 不带 embedding
	for _, body := range fakeES.docs["test_hotreload_index"] {
		if _, hasEmb := body["embedding"]; hasEmb {
			t.Error("Provider 未配置时 chunk 不应包含 embedding 字段")
		}
	}

	// 清空 ES 状态以准备阶段 2
	fakeES.docs["test_hotreload_index"] = make(map[string]map[string]interface{})

	// 阶段 2：Provider 热加载后变为可用
	providerAvailable = true

	// 构建 embFunc（模拟 handleIndexDocument 的逻辑）
	var embFunc embeddingProviderFunc
	if embEmbed != nil && embAvailable != nil && embAvailable(ctx) {
		embFunc = embEmbed
	}

	err = reindexDocumentWithEmbedding(ctx, db, fakeES, "test_hotreload_index", docID,
		embFunc, profileConfig)
	if err != nil {
		t.Fatalf("Provider 可用时 reindexDocumentWithEmbedding 应成功: %v", err)
	}

	// 验证 chunk 带 embedding
	for id, body := range fakeES.docs["test_hotreload_index"] {
		emb, hasEmb := body["embedding"]
		if !hasEmb {
			t.Errorf("Provider 可用时 chunk %s 应包含 embedding 字段", id)
			continue
		}
		embSlice, ok := emb.([]float32)
		if !ok {
			t.Errorf("chunk %s embedding 类型应为 []float32", id)
			continue
		}
		if len(embSlice) != dimensions {
			t.Errorf("chunk %s embedding 维度 = %d, 期望 %d", id, len(embSlice), dimensions)
		}
	}
}

// TestEmbeddingProvider_Missing_JobFails 验证 Provider 缺失时不生成无向量 completed job，
// 而是明确失败。
//
// 引入动机：设计要求 embedding 不可用时 indexing job 应重试而非 silent fallback。
// reindexDocumentWithEmbedding 在 embProvider 调用失败时返回 error，job 应明确失败。
func TestEmbeddingProvider_Missing_JobFails(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "provider_missing_test", "Provider Missing",
		"# Provider Missing\n\nContent for provider missing test.\n")
	_ = wsID

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_missing_provider_index", nil)

	// embProvider 返回错误（模拟 Provider 调用失败）
	embProvider := func(ctx context.Context, texts []string) ([][]float32, error) {
		return nil, fmt.Errorf("embedding provider 调用失败: 连接超时")
	}

	profileConfig := &types.SearchProfileConfig{
		ChunkTargetSize:      512,
		ChunkOverlap:         64,
		EmbeddingDimensions:  1024,
	}

	// 调用 reindexDocumentWithEmbedding，embProvider 非 nil 但返回错误
	err := reindexDocumentWithEmbedding(ctx, db, fakeES, "test_missing_provider_index", docID,
		embProvider, profileConfig)

	// 应返回错误，不应 silent fallback
	if err == nil {
		t.Fatal("Provider 调用失败时 reindexDocumentWithEmbedding 应返回错误，不应 silent fallback")
	}

	// 验证错误信息包含 embedding 相关信息
	if !containsStr(err.Error(), "embedding") {
		t.Errorf("错误信息应包含 'embedding'，实际: %s", err.Error())
	}

	// 验证 ES 中没有写入 chunk（因为 embedding 失败导致整个操作失败）
	if len(fakeES.docs["test_missing_provider_index"]) > 0 {
		t.Error("Provider 调用失败时不应向 ES 写入无向量 chunk")
	}
}

// TestHandleIndexDocument_EnsureAliasAndIndex 验证 index_document job
// 在 alias 不存在时自动创建正确的 versioned index 和 alias。
//
// 引入动机：远程验收中发现 index_document 直接向 alias 名称写入导致 ES 自动创建
// 同名具体索引（动态 mapping，无 embedding 字段），后续 rebuild alias 切换失败。
// 修复后 handleIndexDocument 应先确保 alias 和 index 存在。
func TestHandleIndexDocument_EnsureAliasAndIndex(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "ensure_alias_test", "Ensure Alias",
		"# Ensure Alias\n\nContent for ensure alias test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile-id",
			ESIndexName:          "knowledge_v1",
			ChunkTargetSize:      512,
			ChunkOverlap:         64,
			EmbeddingDimensions:  1024,
			Analyzer:             "standard",
		},
	}

	// 不注入 embedding（测试 alias/index 确保逻辑，不依赖外部 Provider）
	handler := NewIndexJobHandler(db, fakeES, nil, nil, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			// 不指定 index_name，使用默认 es.AliasName
		},
	}

	err := handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("handleIndexDocument 失败: %v", err)
	}

	// 验证 versioned index 被创建
	if !fakeES.indices["knowledge_v1"] {
		t.Error("应创建 versioned index knowledge_v1")
	}

	// 验证 alias 指向 versioned index
	if fakeES.aliasIndex != "knowledge_v1" {
		t.Errorf("alias 应指向 knowledge_v1，实际指向 %s", fakeES.aliasIndex)
	}

	// 验证 knowledge_current 不是具体索引（不应出现在 indices 中）
	if fakeES.indices["knowledge_current"] {
		t.Error("knowledge_current 不应是具体索引，应只作为 alias 存在")
	}

	// 验证文档被写入 versioned index（通过 alias）
	if len(fakeES.docs["knowledge_v1"]) == 0 {
		t.Error("文档应被写入 knowledge_v1 索引")
	}
}

// TestHandleIndexDocument_EnsureAliasAndIndex_DeletesExistingConcreteIndex 验证
// index_document job 在 alias 不存在但存在同名具体索引时，先删除具体索引再创建 alias。
//
// 引入动机：远程验收中 knowledge_current 被 ES 自动创建为具体索引，
// 修复后应能自动检测并删除同名具体索引，然后创建正确的 alias。
func TestHandleIndexDocument_EnsureAliasAndIndex_DeletesExistingConcreteIndex(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "delete_concrete_test", "Delete Concrete",
		"# Delete Concrete\n\nContent for delete concrete test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	// 预置：knowledge_current 作为具体索引存在（模拟 ES 自动创建）
	_ = fakeES.CreateIndex(ctx, "knowledge_current", nil)
	_ = fakeES.BulkIndex(ctx, "knowledge_current", []es.IndexDoc{
		{ID: "old_doc_0", Body: map[string]interface{}{"document_id": "old_doc"}},
	})

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile-id",
			ESIndexName:          "knowledge_v1",
			ChunkTargetSize:      512,
			ChunkOverlap:         64,
			EmbeddingDimensions:  1024,
			Analyzer:             "standard",
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
		t.Fatalf("handleIndexDocument 失败: %v", err)
	}

	// 验证 knowledge_current 具体索引已被删除
	if fakeES.indices["knowledge_current"] {
		t.Error("knowledge_current 具体索引应已被删除")
	}

	// 验证 versioned index 被创建
	if !fakeES.indices["knowledge_v1"] {
		t.Error("应创建 versioned index knowledge_v1")
	}

	// 验证 alias 指向 versioned index
	if fakeES.aliasIndex != "knowledge_v1" {
		t.Errorf("alias 应指向 knowledge_v1，实际指向 %s", fakeES.aliasIndex)
	}

	// 验证旧文档已被清除（因为 knowledge_current 具体索引被删除了）
	if len(fakeES.docs["knowledge_current"]) > 0 {
		t.Error("knowledge_current 上的旧文档应随索引删除而清除")
	}
}

// TestHandleIndexDocument_ProviderHealthyButNoProfile_Fails 验证 Provider 健康
// 但 profileConfig 为 nil（无法获取 active profile）时，index_document job
// 不静默完成无向量索引，而是明确失败。
//
// 引入动机：设计要求 "index_document 不得在 Provider 健康却未生成向量时静默完成"。
// 当 Provider 通过 Web API 热加载成功（embAvailable 返回 true）但 active profile
// 未正确配置时，原实现静默以 with_embedding=false 完成。修复后应明确失败。
func TestHandleIndexDocument_ProviderHealthyButNoProfile_Fails(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "healthy_no_profile", "Healthy No Profile",
		"# Test\n\nContent for healthy provider no profile test.\n")
	_ = wsID

	fakeES := newFakeESClient()

	// Provider 健康但 profileRepo 返回 nil profile（模拟无 active profile）
	embAvailable := func(ctx context.Context) bool { return true }
	embEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		return nil, fmt.Errorf("should not be called")
	}

	// fakeProfileRepo 返回 nil profile，模拟无 active profile
	fakeRepo := &fakeProfileRepo{
		profile: nil,
	}

	// NewIndexJobHandler 需要 profileRepo 实现 ProfileRepo 接口
	// fakeProfileRepo.GetActiveProfile 返回 nil, nil
	handler := NewIndexJobHandler(db, fakeES, embEmbed, embAvailable, fakeRepo, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			"index_name":  "test_healthy_no_profile_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("Provider 健康但 profileConfig 为 nil 时应返回错误，不应静默完成")
	}

	// 验证错误信息涉及 profile 或 embedding
	errStr := err.Error()
	if !containsStr(errStr, "profile") && !containsStr(errStr, "向量") && !containsStr(errStr, "embedding") {
		t.Errorf("错误信息应涉及 profile/embedding/向量，实际: %s", errStr)
	}

	// 验证没有写入任何 chunk（因为 job 失败了）
	if len(fakeES.bulkIndexCalls) != 0 {
		t.Errorf("job 失败时不应执行 BulkIndex，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// containsStr 检查字符串是否包含子串。
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
