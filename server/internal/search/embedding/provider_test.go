// provider_test.go 使用可控 fake HTTP provider 测试 embedding provider。
//
// 引入动机：design/06-IMPLEMENTATION.md §Tests 要求使用可控 fake HTTP provider
// 验证参数可配置、embedding dimensions 不硬编码、provider 错误降级/日志，
// 不调用真实外部付费 API。
package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	types "partitura/server/internal/search/types"
)

func TestOpenAICompatibleProvider_Embed(t *testing.T) {
	// 创建 fake HTTP server 模拟 embedding API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("请求路径应为 /embeddings，得到 %s", r.URL.Path)
		}

		var req embeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}

		if req.Model != "test-model" {
			t.Errorf("model 应为 test-model，得到 %s", req.Model)
		}
		if req.Dimensions != 256 {
			t.Errorf("dimensions 应为 256，得到 %d", req.Dimensions)
		}

		// 返回与输入文本数量匹配的向量
		resp := embeddingResponse{}
		for range req.Input {
			vec := make([]float32, req.Dimensions)
			for i := range vec {
				vec[i] = 0.1
			}
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: vec})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		Model:      "test-model",
		Dimensions: 256,
		BatchSize:  10,
	})

	ctx := context.Background()
	vectors, err := provider.Embed(ctx, []string{"hello", "world"})
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}

	if len(vectors) != 2 {
		t.Fatalf("应返回 2 个向量，得到 %d", len(vectors))
	}

	for i, v := range vectors {
		if len(v) != 256 {
			t.Errorf("向量 %d 的维度应为 256，得到 %d", i, len(v))
		}
	}
}

func TestOpenAICompatibleProvider_DimensionsNotHardcoded(t *testing.T) {
	// 验证 dimensions 可配置，不硬编码
	dims := []int{128, 256, 512, 1024, 1536}

	for _, d := range dims {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req embeddingRequest
			json.NewDecoder(r.Body).Decode(&req)

			resp := embeddingResponse{}
			vec := make([]float32, req.Dimensions)
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: vec})

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}))

		provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
			BaseURL:    server.URL,
			Model:      "test",
			Dimensions: d,
		})

		if provider.Dimensions() != d {
			t.Errorf("dimensions 应为 %d，得到 %d", d, provider.Dimensions())
		}

		ctx := context.Background()
		vectors, err := provider.Embed(ctx, []string{"test"})
		if err != nil {
			t.Errorf("dimensions=%d 时 Embed 失败: %v", d, err)
		}
		if len(vectors[0]) != d {
			t.Errorf("dimensions=%d 时返回向量维度不匹配", d)
		}

		server.Close()
	}
}

func TestOpenAICompatibleProvider_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(embeddingResponse{
			Error: &struct {
				Message string `json:"message"`
			}{Message: "internal error"},
		})
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	ctx := context.Background()
	_, err := provider.Embed(ctx, []string{"test"})
	if err == nil {
		t.Error("API 返回错误时 Embed 应返回 error")
	}
}

func TestOpenAICompatibleProvider_DimensionMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := embeddingResponse{}
		// 返回错误维度的向量
		vec := make([]float32, 64) // 应为 128
		resp.Data = append(resp.Data, struct {
			Embedding []float32 `json:"embedding"`
		}{Embedding: vec})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	ctx := context.Background()
	_, err := provider.Embed(ctx, []string{"test"})
	if err == nil {
		t.Error("维度不匹配时应返回 error")
	}
}

func TestOpenAICompatibleProvider_BatchSize(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req embeddingRequest
		json.NewDecoder(r.Body).Decode(&req)

		resp := embeddingResponse{}
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: make([]float32, 128)})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
		BatchSize:  3,
	})

	ctx := context.Background()
	// 7 个文本，batch_size=3，应分 3 批（3+3+1）
	_, err := provider.Embed(ctx, []string{"a", "b", "c", "d", "e", "f", "g"})
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if callCount != 3 {
		t.Errorf("应调用 3 次 API（batch_size=3, 7 texts），得到 %d", callCount)
	}
}

func TestOpenAICompatibleProvider_Available(t *testing.T) {
	// 配置完整 + 上游可达（httptest 本地 fake）时应 available。
	// 修复说明：原用例 BaseURL 指向无监听进程的 localhost:8080，属外部网络依赖，CI/离线环境不稳定；
	// 改用本地 httptest 返回合法 embedding 响应，行为确定、零外部依赖。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(embeddingResponse{Data: []struct {
			Embedding []float32 `json:"embedding"`
		}{{Embedding: make([]float32, 128)}}})
	}))
	defer upstream.Close()

	// 配置完整且上游可达时应 available
	p1 := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    upstream.URL,
		Model:      "test",
		Dimensions: 128,
	})
	if !p1.Available(context.Background()) {
		t.Error("配置完整且上游可达时应 available")
	}

	// 缺少 BaseURL 时不应 available（纯配置校验，无需网络）
	p2 := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		Model: "test",
	})
	if p2.Available(context.Background()) {
		t.Error("缺少 BaseURL 时不应 available")
	}

	// 缺少 Model 时不应 available
	p3 := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL: "http://localhost:8080",
	})
	if p3.Available(context.Background()) {
		t.Error("缺少 Model 时不应 available")
	}

	// 配置完整但上游不可达（空端口）时不应 available——由 embedBatch 网络错误短路，无外部依赖
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closed.Close() // 关闭后连接必然失败
	p4 := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    closed.URL,
		Model:      "test",
		Dimensions: 128,
	})
	if p4.Available(context.Background()) {
		t.Error("上游不可达时不应 available")
	}
}

func TestOpenAICompatibleProvider_EmptyInput(t *testing.T) {
	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    "http://localhost",
		Model:      "test",
		Dimensions: 128,
	})

	vectors, err := provider.Embed(context.Background(), []string{})
	if err != nil {
		t.Fatalf("空输入不应返回错误: %v", err)
	}
	if len(vectors) != 0 {
		t.Errorf("空输入应返回 0 个向量，得到 %d", len(vectors))
	}
}

// TestOpenAICompatibleProvider_HTMLResponse 验证 200 OK 但 Content-Type 为 HTML 时
// Provider 返回错误，不尝试解析 HTML 为 JSON。
//
// 引入动机：远程验收发现 Provider 在检查 HTTP 状态码/Content-Type 前直接 JSON 解析 HTML 错误页，
// 产生 "invalid character '<'" 解析错误。修复后应在 JSON 解码前拦截非 JSON 响应。
func TestOpenAICompatibleProvider_HTMLResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body><h1>502 Bad Gateway</h1></body></html>"))
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	_, err := provider.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("HTML 响应应返回错误")
	}
	// 错误信息应包含 Content-Type 相关提示
	if !strings.Contains(err.Error(), "Content-Type") {
		t.Errorf("错误信息应包含 Content-Type，实际: %s", err.Error())
	}
}

// TestOpenAICompatibleProvider_Non2xxHTMLResponse 验证非 2xx 状态码 + HTML 响应体时
// Provider 在状态码检查阶段立即失败，不尝试解析响应体，错误信息不泄露响应体内容。
//
// 引入动机：代理/网关返回 502/503 HTML 错误页时，原实现尝试 JSON 解析 HTML 导致
// 混乱错误信息。修复后应在状态码检查阶段失败，错误信息只含状态码和 Content-Type。
func TestOpenAICompatibleProvider_Non2xxHTMLResponse(t *testing.T) {
	htmlBody := "<html><body><h1>Service Unavailable</h1><p>nginx/1.21 upstream timeout</p></body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(htmlBody))
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	_, err := provider.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("非 2xx HTML 响应应返回错误")
	}
	// 错误信息应包含状态码
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("错误信息应包含状态码 502，实际: %s", err.Error())
	}
	// 错误信息不应泄露 HTML 响应体内容
	if strings.Contains(err.Error(), "Service Unavailable") {
		t.Errorf("错误信息不应泄露响应体内容，实际: %s", err.Error())
	}
	if strings.Contains(err.Error(), "nginx") {
		t.Errorf("错误信息不应泄露响应体中的服务器信息，实际: %s", err.Error())
	}
}

// TestOpenAICompatibleProvider_TrailingSlashBaseURL 验证 BaseURL 带尾斜杠时
// 请求路径拼接正确，不会产生双斜杠。
//
// 引入动机：用户配置 BaseURL 时可能带或不带尾斜杠，两种情况请求路径应一致。
func TestOpenAICompatibleProvider_TrailingSlashBaseURL(t *testing.T) {
	var receivedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		var req embeddingRequest
		json.NewDecoder(r.Body).Decode(&req)

		resp := embeddingResponse{}
		for range req.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: make([]float32, req.Dimensions)})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// 测试带尾斜杠的 BaseURL
	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL + "/",
		Model:      "test",
		Dimensions: 128,
	})

	_, err := provider.Embed(context.Background(), []string{"test"})
	if err != nil {
		t.Fatalf("带尾斜杠 BaseURL 时 Embed 应成功: %v", err)
	}
	if receivedPath != "/embeddings" {
		t.Errorf("请求路径应为 /embeddings，实际: %s", receivedPath)
	}
}

// TestOpenAICompatibleProvider_LargeDimensions4096 验证 4096 维向量成功返回。
//
// 引入动机：Qwen3 Embedding 等模型支持高维度向量，需验证大维度配置正确传递和返回。
func TestOpenAICompatibleProvider_LargeDimensions4096(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Dimensions != 4096 {
			t.Errorf("dimensions 应为 4096，得到 %d", req.Dimensions)
		}

		resp := embeddingResponse{}
		for range req.Input {
			vec := make([]float32, 4096)
			for i := range vec {
				vec[i] = 0.01
			}
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: vec})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "qwen3-embedding",
		Dimensions: 4096,
	})

	vectors, err := provider.Embed(context.Background(), []string{"test document"})
	if err != nil {
		t.Fatalf("4096 维 Embed 应成功: %v", err)
	}
	if len(vectors) != 1 {
		t.Fatalf("应返回 1 个向量，得到 %d", len(vectors))
	}
	if len(vectors[0]) != 4096 {
		t.Errorf("向量维度应为 4096，得到 %d", len(vectors[0]))
	}
}

// TestOpenAICompatibleProvider_ErrorNoSecretLeak 验证错误信息不泄露 API Key
// 和 Authorization 头内容。
//
// 引入动机：安全要求错误日志和返回值不得包含 Authorization/key 等敏感信息。
func TestOpenAICompatibleProvider_ErrorNoSecretLeak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("<html>Unauthorized - invalid key: sk-secret-12345</html>"))
	}))
	defer server.Close()

	secretKey := "sk-test-secret-key-abc123"
	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		APIKey:     secretKey,
		Model:      "test",
		Dimensions:  128,
	})

	_, err := provider.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("401 响应应返回错误")
	}
	// 错误信息不得包含 API Key
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("错误信息不应泄露 API Key，实际: %s", err.Error())
	}
	// 错误信息不得包含响应体中的 secret
	if strings.Contains(err.Error(), "sk-secret-12345") {
		t.Errorf("错误信息不应泄露响应体中的密钥信息，实际: %s", err.Error())
	}
	// 错误信息不得包含 "Bearer" 认证头
	if strings.Contains(err.Error(), "Bearer") {
		t.Errorf("错误信息不应泄露 Authorization 头，实际: %s", err.Error())
	}
}

// TestOpenAICompatibleProvider_AcceptHeader 验证请求包含 Accept: application/json 头。
//
// 引入动机：添加 Accept 头让 API 网关/代理知道客户端期望 JSON 响应，
// 有助于在网关层拦截非 JSON 响应。
func TestOpenAICompatibleProvider_AcceptHeader(t *testing.T) {
	var receivedAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAccept = r.Header.Get("Accept")

		resp := embeddingResponse{}
		resp.Data = append(resp.Data, struct {
			Embedding []float32 `json:"embedding"`
		}{Embedding: make([]float32, 128)})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	_, _ = provider.Embed(context.Background(), []string{"test"})
	if receivedAccept != "application/json" {
		t.Errorf("Accept 头应为 application/json，实际: %s", receivedAccept)
	}
}

// TestOpenAICompatibleProvider_APIErrorFieldIn200OK 验证 200 OK 但响应体包含
// API 级别 error 字段时 Provider 返回错误。
//
// 引入动机：某些 API 在 200 OK 中返回 error 字段而非 HTTP 错误码，
// Provider 必须检查 result.Error 并返回错误，不能静默返回空向量。
func TestOpenAICompatibleProvider_APIErrorFieldIn200OK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(embeddingResponse{
			Error: &struct {
				Message string `json:"message"`
			}{Message: "model not found: qwen3-embedding"},
		})
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	_, err := provider.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("200 OK 但含 error 字段时应返回错误")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("错误信息应包含 API 错误消息，实际: %s", err.Error())
	}
}

// TestOpenAICompatibleProvider_VectorCountMismatch 验证 API 返回的向量数量
// 与输入文本数量不匹配时 Provider 返回错误。
//
// 引入动机：API 可能因限流或内部错误返回部分向量，Provider 必须检测数量不匹配
// 并返回错误，不能静默返回不完整结果。
func TestOpenAICompatibleProvider_VectorCountMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embeddingRequest
		json.NewDecoder(r.Body).Decode(&req)

		// 只返回 1 个向量，但输入有 3 个文本
		resp := embeddingResponse{}
		resp.Data = append(resp.Data, struct {
			Embedding []float32 `json:"embedding"`
		}{Embedding: make([]float32, req.Dimensions)})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	_, err := provider.Embed(context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("向量数量不匹配时应返回错误")
	}
	if !strings.Contains(err.Error(), "不匹配") {
		t.Errorf("错误信息应包含数量不匹配提示，实际: %s", err.Error())
	}
}

// TestOpenAICompatibleProvider_BadJSONResponse 验证 200 OK + Content-Type: application/json
// 但响应体为无效 JSON 时 Provider 返回错误。
//
// 引入动机：API 可能因内部错误返回截断或损坏的 JSON，
// Provider 必须在 JSON 解码阶段捕获错误并返回带日志的错误。
func TestOpenAICompatibleProvider_BadJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[{"embedding":[0.1,0.2`)) // 截断的 JSON
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{
		BaseURL:    server.URL,
		Model:      "test",
		Dimensions: 128,
	})

	_, err := provider.Embed(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("无效 JSON 响应应返回错误")
	}
	// 错误信息应包含解析相关提示
	if !strings.Contains(err.Error(), "解析") {
		t.Errorf("错误信息应包含解析提示，实际: %s", err.Error())
	}
}
