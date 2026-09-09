// es_integration_test.go 实现 ES 集成测试。
//
// 运行条件：设置 TEST_ELASTICSEARCH_URL 环境变量指向可运行的 Elasticsearch 实例。
// 例如：export TEST_ELASTICSEARCH_URL="http://localhost:9200"
//
// 引入动机：H2 要求 ES 集成测试，验证真实的 ES 索引创建、搜索、aggregation 等功能。
package es

import (
	"context"
	"os"
	"testing"
	"time"
)

// testESClient 返回测试用 ES 客户端，如果未设置 TEST_ELASTICSEARCH_URL 则跳过测试。
// 引入动机：与 TEST_DATABASE_URL 模式一致，未设置环境变量时跳过集成测试。
func testESClient(t *testing.T) *HTTPClient {
	t.Helper()
	url := os.Getenv("TEST_ELASTICSEARCH_URL")
	if url == "" {
		t.Skip("TEST_ELASTICSEARCH_URL 未设置，跳过 ES 集成测试。" +
			"设置 TEST_ELASTICSEARCH_URL 环境变量指向可运行的 Elasticsearch 实例后运行此测试。")
	}
	return NewHTTPClient(url, 30*time.Second)
}

// TestESIntegration_PingAndIndex 测试 ES ping、创建索引、索引文档、搜索、删除索引。
// 引入动机：H2 要求 ES 集成测试验证真实 ES 功能。
func TestESIntegration_PingAndIndex(t *testing.T) {
	client := testESClient(t)
	ctx := context.Background()
	indexName := "knowledge_test_integration"

	// 清理可能残留的测试索引
	_ = client.DeleteIndex(ctx, indexName)

	// 1. Ping
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}

	// 2. 创建索引
	mapping := BuildIndexMapping(1024, "standard")
	if err := client.CreateIndex(ctx, indexName, mapping); err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	defer client.DeleteIndex(ctx, indexName)

	// 3. 索引文档
	// 引入动机：使用 standard analyzer 下 match query "test" 能命中的内容。
	// standard analyzer 将 "testing" 分词为 "testing"（不是 "test"），
	// 因此两个文档的 content 都必须包含独立 token "test" 才能被 match query "test" 命中。
	docs := []IndexDoc{
		{
			ID: "doc1_0",
			Body: map[string]interface{}{
				"document_id":  "doc1",
				"workspace_id": "ws1",
				"path":         "test/doc1.md",
				"title":        "Test Document 1",
				"content":      "This is a test document about searching",
				"revision":     1,
				"status":       "active",
				"is_special":   false,
				"content_hash": "abc123",
			},
		},
		{
			ID: "doc2_0",
			Body: map[string]interface{}{
				"document_id":  "doc2",
				"workspace_id": "ws1",
				"path":         "test/doc2.md",
				"title":        "Test Document 2",
				"content":      "Another document for test search functionality",
				"revision":     1,
				"status":       "active",
				"is_special":   false,
				"content_hash": "def456",
			},
		},
	}
	if err := client.BulkIndex(ctx, indexName, docs); err != nil {
		t.Fatalf("BulkIndex 失败: %v", err)
	}

	// 4. Refresh
	if err := client.Refresh(ctx, indexName); err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}

	// 5. Count
	count, err := client.Count(ctx, indexName, nil)
	if err != nil {
		t.Fatalf("Count 失败: %v", err)
	}
	if count != 2 {
		t.Errorf("Count: 期望 2, 得到 %d", count)
	}

	// 6. Search
	query := map[string]interface{}{
		"query": map[string]interface{}{
			"match": map[string]interface{}{
				"content": "test",
			},
		},
	}
	resp, err := client.Search(ctx, indexName, query)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if resp.Hits.Total.Value < 2 {
		t.Errorf("Search 结果: 期望至少 2 条, 得到 %d", resp.Hits.Total.Value)
	}

	// 7. Aggregation (terms aggregation by document_id)
	aggQuery := map[string]interface{}{
		"size": 0,
		"aggs": map[string]interface{}{
			"doc_ids": map[string]interface{}{
				"terms": map[string]interface{}{
					"field": "document_id",
					"size":  100,
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
				},
			},
		},
	}
	aggResp, err := client.Search(ctx, indexName, aggQuery)
	if err != nil {
		t.Fatalf("Aggregation 查询失败: %v", err)
	}
	if aggResp.Aggregations == nil {
		t.Fatal("Aggregation 响应为空")
	}
	docIDsAgg, ok := aggResp.Aggregations["doc_ids"].(map[string]interface{})
	if !ok {
		t.Fatal("doc_ids 聚合不存在或类型错误")
	}
	buckets, ok := docIDsAgg["buckets"].([]interface{})
	if !ok {
		t.Fatal("buckets 不存在或类型错误")
	}
	if len(buckets) != 2 {
		t.Errorf("Aggregation buckets: 期望 2, 得到 %d", len(buckets))
	}

	// 8. DeleteByQuery
	delQuery := map[string]interface{}{
		"term": map[string]interface{}{
			"document_id": "doc1",
		},
	}
	if err := client.DeleteByQuery(ctx, indexName, delQuery); err != nil {
		t.Fatalf("DeleteByQuery 失败: %v", err)
	}
	if err := client.Refresh(ctx, indexName); err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}
	count, _ = client.Count(ctx, indexName, nil)
	if count != 1 {
		t.Errorf("DeleteByQuery 后 Count: 期望 1, 得到 %d", count)
	}

	// 9. DeleteIndex
	if err := client.DeleteIndex(ctx, indexName); err != nil {
		t.Fatalf("DeleteIndex 失败: %v", err)
	}
	exists, _ := client.IndexExists(ctx, indexName)
	if exists {
		t.Error("DeleteIndex 后索引不应存在")
	}
}
