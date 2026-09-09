// queryutil_test.go 测试公共查询参数解析工具。
//
// 引入动机：统一 workspace、admin、document 三个模块的分页参数解析逻辑。
// 本测试验证 queryutil.ParsePagination 能正确处理各种合法和非法 query string。
//
// 测试覆盖：
//   - 标准 & 分隔的多参数
//   - 反序参数
//   - 单参数
//   - 无query参数
//   - 非法值（非数字、负数、零limit、超限limit）
//   - 畸形RawQuery（包含 '?'）——Fail Fast返回错误而非静默清洁
//   - 空值
//   - 重复参数
//   - 未知参数
package queryutil

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParsePagination_OffsetZeroLimitTwenty(t *testing.T) {
	// 验证 offset=0&limit=20 正确解析——这是远程错误报告的场景
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=20", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("offset=0&limit=20 应正确解析, 错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("limit = %d, 期望 20", limit)
	}
	if offset != 0 {
		t.Errorf("offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_ReversedOrder(t *testing.T) {
	// 验证反序参数正确解析
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?limit=20&offset=0", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("反序参数应正确解析, 错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("limit = %d, 期望 20", limit)
	}
	if offset != 0 {
		t.Errorf("offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_SingleParam_LimitOnly(t *testing.T) {
	// 验证单参数 limit 正确解析，offset 使用默认值
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?limit=50", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("单参数 limit 应正确解析, 错误: %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, 期望 50", limit)
	}
	if offset != 0 {
		t.Errorf("offset = %d, 期望 0 (默认值)", offset)
	}
}

func TestParsePagination_SingleParam_OffsetOnly(t *testing.T) {
	// 验证单参数 offset 正确解析，limit 使用默认值
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=100", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("单参数 offset 应正确解析, 错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("limit = %d, 期望 20 (默认值)", limit)
	}
	if offset != 100 {
		t.Errorf("offset = %d, 期望 100", offset)
	}
}

func TestParsePagination_NoQueryParams(t *testing.T) {
	// 验证无query参数时使用默认值
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("无参数应使用默认值, 错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("默认 limit = %d, 期望 20", limit)
	}
	if offset != 0 {
		t.Errorf("默认 offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_InvalidOffset(t *testing.T) {
	// 验证非数字 offset 被拒绝
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=abc&limit=20", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("非数字 offset 应被拒绝")
	}
}

func TestParsePagination_InvalidLimit(t *testing.T) {
	// 验证非数字 limit 被拒绝
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=xyz", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("非数字 limit 应被拒绝")
	}
}

func TestParsePagination_NegativeOffset(t *testing.T) {
	// 验证负数 offset 被拒绝
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=-1&limit=10", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("负数 offset 应被拒绝")
	}
}

func TestParsePagination_ZeroLimit(t *testing.T) {
	// 验证 limit=0 被拒绝（必须为正整数）
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=0", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("limit=0 应被拒绝")
	}
}

func TestParsePagination_LimitExceedsMax(t *testing.T) {
	// 验证 limit 超过 100 被拒绝
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=101", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("limit=101 应被拒绝")
	}
}

func TestParsePagination_LargeOffset(t *testing.T) {
	// 验证大 offset 值正确解析
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=999999&limit=50", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("大 offset 应正确解析, 错误: %v", err)
	}
	if limit != 50 {
		t.Errorf("limit = %d, 期望 50", limit)
	}
	if offset != 999999 {
		t.Errorf("offset = %d, 期望 999999", offset)
	}
}

func TestParsePagination_PollutedQuery_LimitPolluted(t *testing.T) {
	// 验证代理污染的query string（RawQuery包含 '?'）被Fail Fast拒绝。
	// 代理污染模式：RawQuery = "offset=0&limit=20?offset=0&limit=20"
	// 不应静默清洁，应返回错误让运维发现代理配置问题。
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0&limit=20?offset=0&limit=20", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("包含 '?' 的畸形RawQuery应被拒绝（Fail Fast），不应静默清洁")
	}
}

func TestParsePagination_PollutedQuery_OffsetPolluted(t *testing.T) {
	// 验证代理污染的query string（offset值包含 '?'）被Fail Fast拒绝。
	// 代理污染模式：RawQuery = "offset=0?offset=0&limit=20"
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=0?offset=0&limit=20", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("包含 '?' 的畸形RawQuery应被拒绝（Fail Fast），不应静默清洁")
	}
}

func TestParsePagination_PollutedQuery_ReversedOrder(t *testing.T) {
	// 验证代理污染 + 反序参数被Fail Fast拒绝。
	// 代理污染模式：RawQuery = "limit=20&offset=0?limit=20&offset=0"
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?limit=20&offset=0?limit=20&offset=0", nil)
	_, _, err := ParsePagination(req)
	if err == nil {
		t.Fatal("包含 '?' 的畸形RawQuery应被拒绝（Fail Fast），不应静默清洁")
	}
}

func TestParsePagination_EmptyValues(t *testing.T) {
	// 验证空值参数使用默认值
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?limit=&offset=", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("空值参数应使用默认值, 错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("空 limit 应使用默认值 20, got %d", limit)
	}
	if offset != 0 {
		t.Errorf("空 offset 应使用默认值 0, got %d", offset)
	}
}

func TestParsePagination_UnknownParams(t *testing.T) {
	// 验证未知参数被忽略（与现有契约一致）
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=40&limit=10&status=active", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("未知参数应被忽略, 错误: %v", err)
	}
	if limit != 10 {
		t.Errorf("limit = %d, 期望 10", limit)
	}
	if offset != 40 {
		t.Errorf("offset = %d, 期望 40", offset)
	}
}

func TestParsePagination_PathParam_NoQuery(t *testing.T) {
	// 验证路径参数不含query时正确解析
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/123/documents", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("无query路径参数应使用默认值, 错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("默认 limit = %d, 期望 20", limit)
	}
	if offset != 0 {
		t.Errorf("默认 offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_PathParam_WithQuery(t *testing.T) {
	// 验证路径参数+query正确解析
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/123/documents?offset=10&limit=30", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("路径参数+query应正确解析, 错误: %v", err)
	}
	if limit != 30 {
		t.Errorf("limit = %d, 期望 30", limit)
	}
	if offset != 10 {
		t.Errorf("offset = %d, 期望 10", offset)
	}
}

func TestParsePagination_DuplicateParams(t *testing.T) {
	// 验证重复参数：Go标准库 url.Values.Get 返回第一个值
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?limit=10&limit=20&offset=0", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("重复参数应取第一个值, 错误: %v", err)
	}
	if limit != 10 {
		t.Errorf("重复 limit 应取第一个值 10, got %d", limit)
	}
	if offset != 0 {
		t.Errorf("offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_URLEncodedValues(t *testing.T) {
	// 验证URL编码的值能正确解析
	// %30 = '0', %32%30 = '20'
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?offset=%30&limit=%32%30", nil)
	limit, offset, err := ParsePagination(req)
	if err != nil {
		t.Fatalf("URL编码值应正确解析, 错误: %v", err)
	}
	if limit != 20 {
		t.Errorf("limit = %d, 期望 20", limit)
	}
	if offset != 0 {
		t.Errorf("offset = %d, 期望 0", offset)
	}
}

func TestParsePagination_QuestionMarkOnly(t *testing.T) {
	// 验证 RawQuery 只有 '?' 时被拒绝
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces?", nil)
	_, _, err := ParsePagination(req)
	// httptest.NewRequest 会将 "?" 后的空串设为 RawQuery=""，
	// 所以这里实际不会触发 '?' 检测。验证它不报错并使用默认值。
	if err != nil {
		t.Fatalf("空RawQuery应使用默认值, 错误: %v", err)
	}
}
