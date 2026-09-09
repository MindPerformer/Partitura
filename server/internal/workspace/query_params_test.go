// query_params_test.go 测试后端分页查询参数解析。
//
// 引入动机：远程错误 `offset 参数非法: 包含非数字字符: "0?limit=20"` 表明
// 代理层曾损坏 query string，导致后端收到 offset="0?limit=20" 而非 offset="0"。
// 根因已在 Nginx 配置层修复（proxy_pass 不含 URI 部分，原样透传）。
// 后端采用 Fail Fast 策略：畸形 RawQuery（含 '?'）直接返回 400 并记录日志，
// 不做静默清洁。
//
// 本测试验证 parsePagination（委托至 queryutil.ParsePagination）正确解析
// 各种合法和非法 query string。
//
// 测试覆盖：
//   - offset=0&limit=20 正常解析
//   - 多参数共存
//   - 编码值
//   - 无 query 参数使用默认值
//   - 非法 offset 值拒绝
//   - 非法 limit 值拒绝
package workspace

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParsePagination_OffsetZeroLimitTwenty(t *testing.T) {
	// 验证 offset=0&limit=20 正确解析——这是远程错误报告的场景
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=20", nil)
	limit, offset, err := parsePagination(req)
	if err != nil {
		t.Fatalf("offset=0&limit=20 应正确解析，错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("limit = %d, 期望 20", limit)
	}
	if offset != 0 {
		t.Errorf("offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_MultipleParams(t *testing.T) {
	// 验证多参数共存时各参数独立解析
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=40&limit=10&status=active", nil)
	limit, offset, err := parsePagination(req)
	if err != nil {
		t.Fatalf("多参数应正确解析，错误: %v", err)
	}
	if limit != 10 {
		t.Errorf("limit = %d, 期望 10", limit)
	}
	if offset != 40 {
		t.Errorf("offset = %d, 期望 40", offset)
	}
}

func TestParsePagination_EncodedValues(t *testing.T) {
	// 验证 URL 编码的值能正确解析
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=5", nil)
	limit, offset, err := parsePagination(req)
	if err != nil {
		t.Fatalf("编码值应正确解析，错误: %v", err)
	}
	if limit != 5 {
		t.Errorf("limit = %d, 期望 5", limit)
	}
	if offset != 0 {
		t.Errorf("offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_NoQueryParams(t *testing.T) {
	// 验证无 query 参数时使用默认值
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	limit, offset, err := parsePagination(req)
	if err != nil {
		t.Fatalf("无参数应使用默认值，错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("默认 limit = %d, 期望 20", limit)
	}
	if offset != 0 {
		t.Errorf("默认 offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_CorruptedOffsetValue(t *testing.T) {
	// 验证当 offset 值被损坏为 "0?limit=20" 时（代理层曾出现的 bug），
	// 后端 Fail Fast 拒绝并返回错误，不做静默清洁。
	// %3F 是 '?' 的 URL 编码，r.URL.Query() 解码后 offset 值为 "0?limit=20"，
	// parseIntStrict 会因 '?' 返回错误。
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0%3Flimit%3D20&limit=20", nil)
	_, _, err := parsePagination(req)
	if err == nil {
		t.Fatal("损坏的 offset 值应被拒绝（Fail Fast）")
	}
}

func TestParsePagination_NegativeOffset(t *testing.T) {
	// 验证负数 offset 被拒绝
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=-1&limit=10", nil)
	_, _, err := parsePagination(req)
	if err == nil {
		t.Fatal("负数 offset 应被拒绝")
	}
}

func TestParsePagination_ZeroLimit(t *testing.T) {
	// 验证 limit=0 被拒绝（必须为正整数）
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=0", nil)
	_, _, err := parsePagination(req)
	if err == nil {
		t.Fatal("limit=0 应被拒绝")
	}
}

func TestParsePagination_LimitExceedsMax(t *testing.T) {
	// 验证 limit 超过 100 被拒绝
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=101", nil)
	_, _, err := parsePagination(req)
	if err == nil {
		t.Fatal("limit=101 应被拒绝")
	}
}

func TestParsePagination_LargeOffset(t *testing.T) {
	// 验证大 offset 值正确解析
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=999999&limit=50", nil)
	limit, offset, err := parsePagination(req)
	if err != nil {
		t.Fatalf("大 offset 应正确解析，错误: %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, 期望 50", limit)
	}
	if offset != 999999 {
		t.Errorf("offset = %d, 期望 999999", offset)
	}
}
