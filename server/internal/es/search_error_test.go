// search_error_test.go 验证 Search 在 ES 返回非 200 时给出可诊断的错误信息。
//
// 引入动机：生产上 ES 偶发返回 400（如 dense_vector 维度不匹配）时，
// 原实现只返回 "search 返回状态码 400"，无法定位真实原因，因此必须用 httptest
// 驱动真实 HTTPClient 覆盖"ES 标准错误体"与"非标准错误体"两条分支。
package es

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHTTPClient_SearchReportsESErrorBody 验证 Search 把 ES 错误体的 error.type/reason
// 带进返回的 error，错误前缀保持可识别。
func TestHTTPClient_SearchReportsESErrorBody(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantType string
		wantWhy  string
		// wantCause 是 ES 层次化错误里真正定位问题的原因（root_cause/caused_by 摘要），
		// 必须出现在返回的 error 中；为空表示该用例的错误体没有层次化原因。
		wantCause string
	}{
		{
			name:      "400 维度不匹配",
			status:    http.StatusBadRequest,
			body:      `{"error":{"root_cause":[{"type":"illegal_argument_exception","reason":"Vector dimension error: expected dim: 1024, got 4096"}],"type":"search_phase_execution_exception","reason":"all shards failed"},"status":400}`,
			wantType:  "search_phase_execution_exception",
			wantWhy:   "all shards failed",
			wantCause: "root_cause: illegal_argument_exception: Vector dimension error: expected dim: 1024, got 4096",
		},
		{
			name:     "429 写入/查询限流",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"type":"too_many_requests","reason":"rejected execution of coordinating operation"},"status":429}`,
			wantType: "too_many_requests",
			wantWhy:  "rejected execution of coordinating operation",
		},
		{
			name:     "404 索引不存在",
			status:   http.StatusNotFound,
			body:     `{"error":{"type":"index_not_found_exception","reason":"no such index [knowledge_v1]"},"status":404}`,
			wantType: "index_not_found_exception",
			wantWhy:  "no such index [knowledge_v1]",
		},
		{
			name:     "错误体只有 reason",
			status:   http.StatusInternalServerError,
			body:     `{"error":{"reason":"simulated failure"}}`,
			wantType: "",
			wantWhy:  "simulated failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("请求方法 = %s, 期望 POST", r.Method)
				}
				if r.URL.Path != "/knowledge_v1/_search" {
					t.Errorf("请求路径 = %s, 期望 /knowledge_v1/_search", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := NewHTTPClient(server.URL, time.Second)
			resp, err := client.Search(context.Background(), "knowledge_v1", map[string]interface{}{"query": map[string]interface{}{"match_all": map[string]interface{}{}}})
			if err == nil {
				t.Fatalf("非 200 状态码应返回错误，实际得到响应 %+v", resp)
			}
			if resp != nil {
				t.Errorf("失败时应返回 nil 响应，得到 %+v", resp)
			}

			message := err.Error()
			if !strings.Contains(message, fmt.Sprintf("search 返回状态码 %d", tc.status)) {
				t.Errorf("错误 %q 应包含状态码前缀 %q", message, fmt.Sprintf("search 返回状态码 %d", tc.status))
			}
			if !strings.Contains(message, tc.wantWhy) {
				t.Errorf("错误 %q 应包含 error.reason %q", message, tc.wantWhy)
			}
			if tc.wantType != "" {
				// 同时校验 "type: reason" 的完整拼接形式，避免只把两个片段松散塞进错误。
				wantDetail := fmt.Sprintf("search 返回状态码 %d: %s: %s", tc.status, tc.wantType, tc.wantWhy)
				if !strings.Contains(message, wantDetail) {
					t.Errorf("错误 %q 应包含完整诊断 %q", message, wantDetail)
				}
			}
			if tc.wantCause != "" {
				// 外层 "all shards failed" 不含真实原因，root_cause 必须被带进错误信息，否则日志无法定位。
				if !strings.Contains(message, tc.wantCause) {
					t.Errorf("错误 %q 应包含 root_cause 摘要 %q", message, tc.wantCause)
				}
				// 原有 "<type>: <reason>" 连续子串必须保持完整，原因细节只能追加在其后。
				wantFull := fmt.Sprintf("search 返回状态码 %d: %s: %s（%s）", tc.status, tc.wantType, tc.wantWhy, tc.wantCause)
				if !strings.Contains(message, wantFull) {
					t.Errorf("错误 %q 应包含向后兼容前缀 + 追加原因 %q", message, wantFull)
				}
			}
		})
	}
}

// TestHTTPClient_SearchReportsNestedCausedBy 验证 error.caused_by 的嵌套链会被完整
// 带进返回的 error，且原有 "type: reason" 前缀保持连续。
//
// 引入动机：ES 常见形态是外层 search_phase_execution_exception，真正原因藏在 caused_by
// 内并可继续嵌套（如 illegal_argument_exception 由 number_format_exception 引起）；
// 只保留最外层会让线上日志无法定位，因此必须覆盖嵌套链。
func TestHTTPClient_SearchReportsNestedCausedBy(t *testing.T) {
	const body = `{"error":{"type":"search_phase_execution_exception","reason":"all shards failed","caused_by":{"type":"illegal_argument_exception","reason":"failed to parse query","caused_by":{"type":"number_format_exception","reason":"For input string: \"abc\""}}},"status":400}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, time.Second)
	_, err := client.Search(context.Background(), "knowledge_v1", nil)
	if err == nil {
		t.Fatal("非 200 状态码应返回错误")
	}

	message := err.Error()
	wantPrefix := "search 返回状态码 400: search_phase_execution_exception: all shards failed"
	if !strings.Contains(message, wantPrefix) {
		t.Errorf("错误 %q 应包含向后兼容前缀 %q", message, wantPrefix)
	}
	for _, want := range []string{
		"caused_by: illegal_argument_exception: failed to parse query",
		"caused_by: number_format_exception: For input string: \"abc\"",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("错误 %q 应包含嵌套原因 %q", message, want)
		}
	}
	if !strings.Contains(message, wantPrefix+"（caused_by:") {
		t.Errorf("错误 %q 应在原有前缀之后追加 caused_by 细节", message)
	}
}

// TestHTTPClient_SearchFallsBackToBodySummary 验证错误体不是 ES 标准结构时，
// Search 退化为截断后的响应体摘要，而不是只给出状态码。
func TestHTTPClient_SearchFallsBackToBodySummary(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "网关返回 HTML 错误页",
			status: http.StatusBadGateway,
			body:   "<html><body>502 Bad Gateway from proxy</body></html>",
		},
		{
			name:   "error 字段为字符串而非对象",
			status: http.StatusBadRequest,
			body:   `{"error":"query parse failure","status":400}`,
		},
		{
			name:   "空响应体",
			status: http.StatusInternalServerError,
			body:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := NewHTTPClient(server.URL, time.Second)
			_, err := client.Search(context.Background(), "knowledge_v1", nil)
			if err == nil {
				t.Fatal("非 200 状态码应返回错误")
			}

			message := err.Error()
			if !strings.Contains(message, fmt.Sprintf("search 返回状态码 %d", tc.status)) {
				t.Errorf("错误 %q 应包含状态码前缀 %d", message, tc.status)
			}
			if tc.body != "" && !strings.Contains(message, tc.body) {
				t.Errorf("错误 %q 应包含退化的响应体摘要 %q", message, tc.body)
			}
		})
	}
}

// TestHTTPClient_SearchSuccessStillParsesResponse 确认错误诊断的改动没有影响 200 正常路径。
func TestHTTPClient_SearchSuccessStillParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":{"total":{"value":1},"hits":[{"_id":"doc-1","_score":1.5,"_source":{"title":"hello"}}]}}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, time.Second)
	resp, err := client.Search(context.Background(), "knowledge_v1", nil)
	if err != nil {
		t.Fatalf("200 响应不应返回错误: %v", err)
	}
	if resp.Hits.Total.Value != 1 || len(resp.Hits.Hits) != 1 {
		t.Fatalf("hits = %+v, 期望 1 条命中", resp.Hits)
	}
	if resp.Hits.Hits[0].ID != "doc-1" {
		t.Errorf("命中 ID = %s, 期望 doc-1", resp.Hits.Hits[0].ID)
	}
}
