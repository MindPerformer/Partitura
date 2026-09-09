// delete_by_query_test.go 测试 DeleteByQuery 对 404 index_not_found 的幂等行为。
//
// 引入动机：Phase6 远程验收发现首次 index_document job 在尝试 delete_by_query
// 旧 chunk 时 ES 返回 404（索引不存在），被当作致命错误导致作业 dead。
// 修复后 DeleteByQuery 仅对 index_not_found_exception 类型的 404 幂等成功，
// 其他 404 和 5xx 仍返回错误。
//
// 测试策略：使用 httptest.Server 模拟 ES 响应，验证：
//   - index_not_found 404 → 返回 nil（幂等成功）
//   - 其他类型 404 → 返回错误
//   - 5xx → 返回错误
//   - 200 → 返回 nil
package es

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDeleteByQuery_IndexNotFound404 验证索引不存在时 404 被幂等处理为成功。
// 引入动机：首次 index_document job 时目标索引可能尚未创建，delete_by_query
// 返回 404 index_not_found_exception，应幂等成功而非失败。
func TestDeleteByQuery_IndexNotFound404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":      "index_not_found_exception",
				"reason":    "no such index [knowledge_v1]",
				"index":     "knowledge_v1",
				"index_uuid": "_na_",
			},
			"status": 404,
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	ctx := context.Background()

	err := client.DeleteByQuery(ctx, "knowledge_v1", map[string]interface{}{
		"term": map[string]interface{}{"document_id": "doc1"},
	})
	if err != nil {
		t.Fatalf("索引不存在时 DeleteByQuery 应幂等成功，实际返回错误: %v", err)
	}
}

// TestDeleteByQuery_Other404 验证非 index_not_found 类型的 404 仍返回错误。
// 引入动机：不能宽泛吞所有 404，其他类型的 404 表示真正的异常。
func TestDeleteByQuery_Other404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":   "resource_not_found_exception",
				"reason": "some other 404",
			},
			"status": 404,
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	ctx := context.Background()

	err := client.DeleteByQuery(ctx, "knowledge_v1", map[string]interface{}{
		"term": map[string]interface{}{"document_id": "doc1"},
	})
	if err == nil {
		t.Fatal("非 index_not_found 的 404 应返回错误，实际返回 nil")
	}
}

// TestDeleteByQuery_5xxError 验证 5xx 状态码仍返回错误。
// 引入动机：ES 服务端错误不应被吞掉，仍需 fail-fast。
func TestDeleteByQuery_5xxError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"type":   "internal_server_error",
				"reason": "ES internal error",
			},
			"status": 500,
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	ctx := context.Background()

	err := client.DeleteByQuery(ctx, "knowledge_v1", map[string]interface{}{
		"term": map[string]interface{}{"document_id": "doc1"},
	})
	if err == nil {
		t.Fatal("5xx 错误应返回错误，实际返回 nil")
	}
}

// TestDeleteByQuery_200Success 验证正常 200 响应返回 nil。
func TestDeleteByQuery_200Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"took":    5,
			"deleted": 3,
			"batches": 1,
			"total":   3,
			"failures": 0,
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	ctx := context.Background()

	err := client.DeleteByQuery(ctx, "knowledge_v1", map[string]interface{}{
		"term": map[string]interface{}{"document_id": "doc1"},
	})
	if err != nil {
		t.Fatalf("200 响应应返回 nil，实际返回错误: %v", err)
	}
}

// TestDeleteByQuery_404MalformedBody 验证 404 响应体无法解析时仍返回错误。
// 引入动机：如果 404 响应体不是合法 JSON 或缺少 error.type 字段，
// 不应误判为 index_not_found，应安全失败。
func TestDeleteByQuery_404MalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`not valid json`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*time.Second)
	ctx := context.Background()

	err := client.DeleteByQuery(ctx, "knowledge_v1", map[string]interface{}{
		"term": map[string]interface{}{"document_id": "doc1"},
	})
	if err == nil {
		t.Fatal("404 且响应体无法解析时应返回错误，实际返回 nil")
	}
}
