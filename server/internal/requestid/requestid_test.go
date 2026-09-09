// requestid_test.go 测试 request ID 的生成、注入和提取。
//
// 测试覆盖：
//   - 缺失 X-Request-ID 时生成不可预测的 32 字符十六进制 ID
//   - 非法 X-Request-ID 时生成新 ID
//   - 合法 X-Request-ID 被保留
//   - context 可提取 request ID
//   - 未经过 middleware 的 context 返回空字符串
//   - 响应始终包含 X-Request-ID 头
package requestid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testHandler 返回一个简单的 handler，用于验证 middleware 行为。
// 它从 context 读取 request ID 并写入响应体，以便测试验证 context 中的 request ID。
func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := RequestIDFromContext(r.Context())
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(rid))
	})
}

// TestMiddleware_MissingGenerates 验证缺失 X-Request-ID 时服务器生成 request ID。
func TestMiddleware_MissingGenerates(t *testing.T) {
	handler := Middleware(testHandler())
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	rid := rr.Header().Get(HeaderName)
	if rid == "" {
		t.Fatal("缺失 X-Request-ID 时应生成并回写 request ID")
	}
	if len(rid) != 32 {
		t.Errorf("生成的 request ID 应为 32 字符十六进制，实际 %d 字符: %q", len(rid), rid)
	}
	// 响应体应包含 context 中的 request ID
	if rr.Body.String() != rid {
		t.Errorf("context 中的 request ID = %q, 响应头 = %q, 应一致", rr.Body.String(), rid)
	}
}

// TestMiddleware_InvalidGenerates 验证非法 X-Request-ID 时服务器生成新的 request ID。
func TestMiddleware_InvalidGenerates(t *testing.T) {
	handler := Middleware(testHandler())

	invalidIDs := []string{
		"contains spaces",
		"has/slashes",
		"has<special>chars",
		strings.Repeat("a", 129), // 超长
		";drop table;",
	}

	for _, invalid := range invalidIDs {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(HeaderName, invalid)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		rid := rr.Header().Get(HeaderName)
		if rid == invalid {
			t.Errorf("非法 request ID %q 不应被保留", invalid)
		}
		if rid == "" {
			t.Errorf("非法 request ID %q 应触发生成新 ID，但响应头为空", invalid)
		}
		if len(rid) != 32 {
			t.Errorf("生成的 request ID 应为 32 字符十六进制，实际 %d 字符: %q", len(rid), rid)
		}
	}
}

// TestMiddleware_ValidPreserved 验证合法 X-Request-ID 被保留并回写。
func TestMiddleware_ValidPreserved(t *testing.T) {
	handler := Middleware(testHandler())

	validIDs := []string{
		"abc123",
		"550e8400-e29b-41d4-a716-446655440000",
		"request_id_with_underscores",
		"req-123-456-789",
		strings.Repeat("a", 128), // 最大允许长度
	}

	for _, valid := range validIDs {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set(HeaderName, valid)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		rid := rr.Header().Get(HeaderName)
		if rid != valid {
			t.Errorf("合法 request ID %q 应被保留，实际 %q", valid, rid)
		}
		if rr.Body.String() != valid {
			t.Errorf("context 中的 request ID = %q, 期望 %q", rr.Body.String(), valid)
		}
	}
}

// TestRequestIDFromContext_EmptyContextReturnsEmpty 验证未经过 middleware 的 context 返回空字符串。
func TestRequestIDFromContext_EmptyContextReturnsEmpty(t *testing.T) {
	rid := RequestIDFromContext(context.Background())
	if rid != "" {
		t.Errorf("未经过 middleware 的 context 应返回空字符串，实际 %q", rid)
	}
}

// TestMiddleware_AlwaysInResponse 验证响应始终包含 X-Request-ID 头。
func TestMiddleware_AlwaysInResponse(t *testing.T) {
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	// 无入站 header
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Header().Get(HeaderName) == "" {
		t.Error("无入站 header 时响应应包含 X-Request-ID")
	}

	// 有入站 header
	req2 := httptest.NewRequest(http.MethodGet, "/test", nil)
	req2.Header.Set(HeaderName, "test-req-123")
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Header().Get(HeaderName) != "test-req-123" {
		t.Errorf("响应应保留入站 X-Request-ID，实际 %q", rr2.Header().Get(HeaderName))
	}
}

// TestIsValid 验证 request ID 的安全验证逻辑。
func TestIsValid(t *testing.T) {
	tests := []struct {
		id    string
		valid bool
	}{
		{"", false},
		{"abc123", true},
		{"req-123-456", true},
		{"request_id", true},
		{"has spaces", false},
		{"has/slashes", false},
		{"has<special>", false},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
		{";drop table;", false},
	}

	for _, tt := range tests {
		got := IsValid(tt.id)
		if got != tt.valid {
			t.Errorf("IsValid(%q) = %v, 期望 %v", tt.id, got, tt.valid)
		}
	}
}
