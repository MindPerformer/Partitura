// provider_test.go 使用可控 fake HTTP provider 测试 reranker provider。
//
// 引入动机：design/06-IMPLEMENTATION.md §Tests 要求使用可控 fake provider
// 验证 reranker 降级/日志，不调用真实外部付费 API。
package reranker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	types "partitura/server/internal/search/types"
)

func TestHTTPProvider_Rerank(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Errorf("请求路径应为 /rerank，得到 %s", r.URL.Path)
		}

		var req rerankerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}

		if req.Model != "test-reranker" {
			t.Errorf("model 应为 test-reranker，得到 %s", req.Model)
		}
		if req.Query != "test query" {
			t.Errorf("query 应为 'test query'，得到 %s", req.Query)
		}
		if len(req.Documents) != 3 {
			t.Errorf("应有 3 个候选文档，得到 %d", len(req.Documents))
		}

		// 返回重排序结果（反转顺序）
		resp := rerankerResponse{
			Results: []struct {
				Index    int     `json:"index"`
				Score    float64 `json:"relevance_score"`
			}{
				{Index: 2, Score: 0.9},
				{Index: 1, Score: 0.7},
				{Index: 0, Score: 0.5},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL:       server.URL,
		APIKey:        "test-key",
		Model:         "test-reranker",
		MaxCandidates: 20,
	})

	candidates := []types.RerankerCandidate{
		{Text: "doc 0", Index: 0},
		{Text: "doc 1", Index: 1},
		{Text: "doc 2", Index: 2},
	}

	results, err := provider.Rerank(context.Background(), "test query", candidates)
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("应返回 3 个结果，得到 %d", len(results))
	}

	if results[0].Index != 2 {
		t.Errorf("第一个结果 index 应为 2，得到 %d", results[0].Index)
	}
	if results[0].Score != 0.9 {
		t.Errorf("第一个结果 score 应为 0.9，得到 %f", results[0].Score)
	}
}

func TestHTTPProvider_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(rerankerResponse{
			Error: &struct {
				Message string `json:"message"`
			}{Message: "service unavailable"},
		})
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query", []types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Error("API 返回错误时 Rerank 应返回 error（供调用方降级）")
	}
}

func TestHTTPProvider_EmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rerankerResponse{Results: nil})
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query", []types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Error("空结果时 Rerank 应返回 error（供调用方降级为 RRF）")
	}
}

func TestHTTPProvider_MaxCandidatesLimit(t *testing.T) {
	var receivedDocs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rerankerRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedDocs = len(req.Documents)

		resp := rerankerResponse{}
		for i := range req.Documents {
			resp.Results = append(resp.Results, struct {
				Index    int     `json:"index"`
				Score    float64 `json:"relevance_score"`
			}{Index: i, Score: 0.5})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL:       server.URL,
		Model:         "test",
		MaxCandidates: 5,
	})

	candidates := make([]types.RerankerCandidate, 10)
	for i := range candidates {
		candidates[i] = types.RerankerCandidate{Text: "doc", Index: i}
	}

	_, err := provider.Rerank(context.Background(), "query", candidates)
	if err != nil {
		t.Fatalf("Rerank 失败: %v", err)
	}
	if receivedDocs != 5 {
		t.Errorf("应限制为 5 个候选，发送了 %d", receivedDocs)
	}
}

func TestHTTPProvider_Available(t *testing.T) {
	// 配置完整 + 上游可达（httptest 本地 fake）时应 available。
	// 修复说明：原用例 BaseURL 指向 http://localhost（无监听进程），属外部网络依赖；
	// 改用本地 httptest 返回合法 rerank 响应，行为确定、零外部依赖。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rerankerResponse{Results: []struct {
			Index    int     `json:"index"`
			Score    float64 `json:"relevance_score"`
		}{{Index: 0, Score: 1.0}}})
	}))
	defer upstream.Close()

	p1 := NewHTTPProvider(types.RerankerConfig{
		BaseURL: upstream.URL,
		Model:   "test",
	})
	if !p1.Available(context.Background()) {
		t.Error("配置完整且上游可达时应 available")
	}

	p2 := NewHTTPProvider(types.RerankerConfig{})
	if p2.Available(context.Background()) {
		t.Error("缺少配置时不应 available")
	}

	// 配置完整但上游不可达（已关闭的 httptest）时不应 available——由网络错误短路，无外部依赖
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close()
	p3 := NewHTTPProvider(types.RerankerConfig{
		BaseURL: closed.URL,
		Model:   "test",
	})
	if p3.Available(context.Background()) {
		t.Error("上游不可达时不应 available")
	}
}

func TestHTTPProvider_EmptyCandidates(t *testing.T) {
	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: "http://localhost",
		Model:   "test",
	})

	results, err := provider.Rerank(context.Background(), "query", nil)
	if err != nil {
		t.Fatalf("空候选不应返回错误: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("空候选应返回 0 个结果，得到 %d", len(results))
	}
}

// TestHTTPProvider_HTMLResponse 验证 200 OK 但 Content-Type 为 HTML 时
// Provider 返回错误，不尝试解析 HTML 为 JSON。
//
// 引入动机：与 embedding provider 相同的根因——代理/网关返回 HTML 错误页时
// 不应尝试 JSON 解析。reranker 降级为 RRF 但错误必须正确传播。
func TestHTTPProvider_HTMLResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body>Bad Gateway</body></html>"))
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Fatal("HTML 响应应返回错误")
	}
	if !strings.Contains(err.Error(), "Content-Type") {
		t.Errorf("错误信息应包含 Content-Type，实际: %s", err.Error())
	}
}

// TestHTTPProvider_Non2xxHTMLResponse 验证非 2xx 状态码 + HTML 响应体时
// Provider 在状态码检查阶段立即失败，错误信息不泄露响应体内容。
func TestHTTPProvider_Non2xxHTMLResponse(t *testing.T) {
	htmlBody := "<html><body><h1>503 Service Unavailable</h1><p>upstream timeout</p></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(htmlBody))
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Fatal("非 2xx HTML 响应应返回错误")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("错误信息应包含状态码 503，实际: %s", err.Error())
	}
	if strings.Contains(err.Error(), "upstream timeout") {
		t.Errorf("错误信息不应泄露响应体内容，实际: %s", err.Error())
	}
}

// TestHTTPProvider_TrailingSlashBaseURL 验证 BaseURL 带尾斜杠时
// 请求路径拼接正确，不会产生双斜杠。
func TestHTTPProvider_TrailingSlashBaseURL(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		resp := rerankerResponse{}
		resp.Results = append(resp.Results, struct {
			Index    int     `json:"index"`
			Score    float64 `json:"relevance_score"`
		}{Index: 0, Score: 0.9})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL + "/",
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err != nil {
		t.Fatalf("带尾斜杠 BaseURL 时 Rerank 应成功: %v", err)
	}
	if receivedPath != "/rerank" {
		t.Errorf("请求路径应为 /rerank，实际: %s", receivedPath)
	}
}

// TestHTTPProvider_ErrorNoSecretLeak 验证错误信息不泄露 API Key
// 和 Authorization 头内容。
func TestHTTPProvider_ErrorNoSecretLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("<html>invalid key: sk-secret-xyz</html>"))
	}))
	defer server.Close()

	secretKey := "sk-reranker-secret-456"
	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		APIKey:  secretKey,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Fatal("401 响应应返回错误")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("错误信息不应泄露 API Key，实际: %s", err.Error())
	}
	if strings.Contains(err.Error(), "sk-secret-xyz") {
		t.Errorf("错误信息不应泄露响应体中的密钥信息，实际: %s", err.Error())
	}
	if strings.Contains(err.Error(), "Bearer") {
		t.Errorf("错误信息不应泄露 Authorization 头，实际: %s", err.Error())
	}
}

// TestHTTPProvider_APIErrorFieldIn200OK 验证 200 OK 但响应体包含
// API 级别 error 字段时 Reranker 返回错误（供调用方降级为 RRF）。
//
// 引入动机：某些 API 在 200 OK 中返回 error 字段而非 HTTP 错误码，
// Provider 必须检查 result.Error 并返回错误，不能静默返回空结果。
func TestHTTPProvider_APIErrorFieldIn200OK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rerankerResponse{
			Error: &struct {
				Message string `json:"message"`
			}{Message: "model not found: qwen3-reranker"},
		})
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Fatal("200 OK 但含 error 字段时应返回错误")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("错误信息应包含 API 错误消息，实际: %s", err.Error())
	}
}

// TestHTTPProvider_BadJSONResponse 验证 200 OK + Content-Type: application/json
// 但响应体为无效 JSON 时 Reranker 返回错误（供调用方降级为 RRF）。
//
// 引入动机：API 可能因内部错误返回截断或损坏的 JSON，
// Provider 必须在 JSON 解码阶段捕获错误并返回带日志的错误。
func TestHTTPProvider_BadJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[{"index":0,"relevance_s`)) // 截断的 JSON
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Fatal("无效 JSON 响应应返回错误")
	}
	if !strings.Contains(err.Error(), "解析") {
		t.Errorf("错误信息应包含解析提示，实际: %s", err.Error())
	}
}

// TestHTTPProvider_AcceptHeader 验证请求包含 Accept: application/json 头。
//
// 引入动机：与 embedding provider 一致，添加 Accept 头让 API 网关/代理
// 知道客户端期望 JSON 响应。
func TestHTTPProvider_AcceptHeader(t *testing.T) {
	var receivedAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAccept = r.Header.Get("Accept")

		resp := rerankerResponse{}
		resp.Results = append(resp.Results, struct {
			Index    int     `json:"index"`
			Score    float64 `json:"relevance_score"`
		}{Index: 0, Score: 0.9})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, _ = provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if receivedAccept != "application/json" {
		t.Errorf("Accept 头应为 application/json，实际: %s", receivedAccept)
	}
}

// TestHTTPProvider_ResultIndexOutOfBounds 验证 API 返回越界索引时
// Reranker 返回错误（供调用方降级为 RRF）。
//
// 引入动机：API 可能因内部错误返回超出候选列表范围的索引，
// 调用方用 Index 映射回候选列表，越界索引会导致下游 panic 或错误排序。
func TestHTTPProvider_ResultIndexOutOfBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := rerankerResponse{
			Results: []struct {
				Index    int     `json:"index"`
				Score    float64 `json:"relevance_score"`
			}{
				{Index: 5, Score: 0.9}, // 只有 2 个候选，index=5 越界
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{
			{Text: "doc0", Index: 0},
			{Text: "doc1", Index: 1},
		})
	if err == nil {
		t.Fatal("越界索引应返回错误")
	}
	if !strings.Contains(err.Error(), "越界") {
		t.Errorf("错误信息应包含越界提示，实际: %s", err.Error())
	}
}

// TestHTTPProvider_NegativeIndexOutOfBounds 验证 API 返回负索引时
// Reranker 返回错误。
func TestHTTPProvider_NegativeIndexOutOfBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := rerankerResponse{
			Results: []struct {
				Index    int     `json:"index"`
				Score    float64 `json:"relevance_score"`
			}{
				{Index: -1, Score: 0.9},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	_, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Fatal("负索引应返回错误")
	}
	if !strings.Contains(err.Error(), "越界") {
		t.Errorf("错误信息应包含越界提示，实际: %s", err.Error())
	}
}

// TestHTTPProvider_PartialResults 验证 API 返回的结果数少于输入候选数时
// Reranker 仍返回结果（不视为错误，但记录警告日志）。
//
// 引入动机：某些 reranker API 可能过滤低相关性候选而返回较少结果，
// 这不一定是错误——调用方应能使用部分结果而非强制降级为 RRF。
func TestHTTPProvider_PartialResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 输入 3 个候选，只返回 2 个结果
		resp := rerankerResponse{
			Results: []struct {
				Index    int     `json:"index"`
				Score    float64 `json:"relevance_score"`
			}{
				{Index: 2, Score: 0.95},
				{Index: 0, Score: 0.6},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewHTTPProvider(types.RerankerConfig{
		BaseURL: server.URL,
		Model:   "test",
	})

	results, err := provider.Rerank(context.Background(), "query",
		[]types.RerankerCandidate{
			{Text: "doc0", Index: 0},
			{Text: "doc1", Index: 1},
			{Text: "doc2", Index: 2},
		})
	if err != nil {
		t.Fatalf("部分结果不应返回错误: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("应返回 2 个结果，得到 %d", len(results))
	}
	if results[0].Index != 2 {
		t.Errorf("第一个结果 index 应为 2，得到 %d", results[0].Index)
	}
}
