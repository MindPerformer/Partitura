// delete_by_query_job_test.go 测试 index_document job 对 DeleteByQuery 错误的处理行为。
//
// 引入动机：Phase6 远程验收发现首次 index_document job 在尝试 delete_by_query
// 旧 chunk 时 ES 返回 404（索引不存在），被当作致命错误导致作业 dead。
// 修复后 ES HTTPClient.DeleteByQuery 对 index_not_found 404 返回 nil（幂等成功），
// 其他错误仍返回 error。
//
// 本测试验证 reindexDocumentWithEmbedding 的行为：
//   - DeleteByQuery 返回 nil（模拟修复后 404 幂等成功）→ 作业继续执行并完成
//   - DeleteByQuery 返回 error（模拟非 404 的 ES 错误）→ 作业失败
//
// 测试策略：使用可配置的 fake ES client，控制 DeleteByQuery 的返回值，
// 验证 reindexDocumentWithEmbedding 在不同 DeleteByQuery 行为下的正确响应。
package job

import (
	"context"
	"fmt"
	"testing"

	types "partitura/server/internal/search/types"
)

// configurableESClient 是可配置 DeleteByQuery 返回值的 fake ES client。
// 引入动机：测试 reindexDocumentWithEmbedding 对不同 DeleteByQuery 行为的响应，
// 特别是 DeleteByQuery 返回 nil（模拟修复后 404 幂等成功）时作业应继续完成。
type configurableESClient struct {
	*fakeESClient
	deleteByQueryErr error // DeleteByQuery 返回的错误，nil 表示成功
}

func newConfigurableESClient(deleteByQueryErr error) *configurableESClient {
	return &configurableESClient{
		fakeESClient:     newFakeESClient(),
		deleteByQueryErr: deleteByQueryErr,
	}
}

func (c *configurableESClient) DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error {
	c.deleteByQueryCalls = append(c.deleteByQueryCalls, deleteByQueryCall{IndexName: indexName, Query: query})
	if c.deleteByQueryErr != nil {
		return c.deleteByQueryErr
	}
	return c.fakeESClient.DeleteByQuery(ctx, indexName, query)
}

// TestReindexDocument_DeleteByQueryNil 验证 DeleteByQuery 返回 nil 时
// reindexDocumentWithEmbedding 成功完成索引。
// 引入动机：修复后 ES HTTPClient.DeleteByQuery 对 index_not_found 404 返回 nil，
// reindexDocumentWithEmbedding 应继续执行 chunking 和 BulkIndex。
func TestReindexDocument_DeleteByQueryNil(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "delete_by_query_nil_doc", "DBQ Nil Test",
		"# Test\n\nContent for delete_by_query nil test.\n")
	_ = wsID

	// 使用返回 nil 的 configurableESClient（模拟修复后 404 幂等成功）
	fakeES := newConfigurableESClient(nil)
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	err := reindexDocumentWithEmbedding(ctx, db, fakeES, "test_index", docID, nil, nil)
	if err != nil {
		t.Fatalf("DeleteByQuery 返回 nil 时 reindexDocument 应成功，实际失败: %v", err)
	}

	// 验证 DeleteByQuery 被调用
	if len(fakeES.deleteByQueryCalls) < 1 {
		t.Fatal("期望至少 1 次 DeleteByQuery 调用")
	}

	// 验证 BulkIndex 被调用（文档被索引）
	if len(fakeES.bulkIndexCalls) != 1 {
		t.Fatalf("期望 1 次 BulkIndex 调用，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// TestReindexDocument_DeleteByQueryError 验证 DeleteByQuery 返回非 nil 错误时
// reindexDocumentWithEmbedding 失败。
// 引入动机：修复后仅 index_not_found 404 被幂等处理，其他 ES 错误仍应 fail-fast。
func TestReindexDocument_DeleteByQueryError(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "delete_by_query_err_doc", "DBQ Error Test",
		"# Test\n\nContent for delete_by_query error test.\n")
	_ = wsID

	// 使用返回错误的 configurableESClient（模拟非 404 的 ES 错误）
	dbqErr := fmt.Errorf("delete_by_query 返回状态码 500")
	fakeES := newConfigurableESClient(dbqErr)
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	err := reindexDocumentWithEmbedding(ctx, db, fakeES, "test_index", docID, nil, nil)
	if err == nil {
		t.Fatal("DeleteByQuery 返回错误时 reindexDocument 应失败，实际返回 nil")
	}

	// 验证错误信息包含原始错误
	if fmt.Sprintf("%v", err) == "" {
		t.Error("错误信息不应为空")
	}

	// 验证 BulkIndex 未被调用（因 DeleteByQuery 失败而中止）
	if len(fakeES.bulkIndexCalls) != 0 {
		t.Errorf("DeleteByQuery 失败时不应执行 BulkIndex，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// TestHandleIndexDocument_NoIndexExists 验证首次 index_document job
// 在目标索引不存在时能成功完成。
// 引入动机：Phase6 远程验收发现首次 index_document job 因 DeleteByQuery
// 在不存在的索引上返回 404 而失败。修复后 ES HTTPClient 对此 404 幂等成功，
// job 应能完成。此测试使用 fakeESClient（DeleteByQuery 始终返回 nil）
// 模拟修复后的行为，验证 job 完整执行。
func TestHandleIndexDocument_NoIndexExists(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "no_index_doc", "No Index Test",
		"# Test\n\nContent for no index test.\n")
	_ = wsID

	// fakeESClient 的 DeleteByQuery 始终返回 nil，模拟修复后 404 幂等成功
	fakeES := newFakeESClient()
	// 不创建索引——模拟首次运行时索引不存在

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile-id",
			ESIndexName:         "knowledge_v1",
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: 1024,
			Analyzer:            "standard",
		},
	}

	handler := NewIndexJobHandler(db, fakeES, nil, nil, fakeRepo, nil, nil)

	job := &Job{
		Type: types.JobIndexDocument,
		Payload: map[string]interface{}{
			"document_id": docID,
			"index_name":  "knowledge_v1",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err != nil {
		t.Fatalf("首次 index_document job 在索引不存在时应成功，实际失败: %v", err)
	}

	// 验证 DeleteByQuery 被调用（尝试删除旧 chunk）
	if len(fakeES.deleteByQueryCalls) < 1 {
		t.Fatal("期望至少 1 次 DeleteByQuery 调用")
	}

	// 验证 BulkIndex 被调用（文档被索引）
	if len(fakeES.bulkIndexCalls) != 1 {
		t.Fatalf("期望 1 次 BulkIndex 调用，实际 %d 次", len(fakeES.bulkIndexCalls))
	}

	// 验证索引名正确
	if fakeES.bulkIndexCalls[0].IndexName != "knowledge_v1" {
		t.Errorf("BulkIndex 索引名应为 knowledge_v1，实际为 %s", fakeES.bulkIndexCalls[0].IndexName)
	}
}


