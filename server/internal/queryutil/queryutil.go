// Package queryutil 提供HTTP查询参数解析的公共工具函数。
//
// 引入动机：workspace、admin、document三个模块各自实现了几乎相同的
// parsePagination/parseDocPagination函数。本包提供统一的分页解析逻辑
// 供所有handler复用。
//
// 设计原则（Fail Fast）：
//   - 标准HTTP query ?offset=0&limit=20、单参数、顺序变化均正确解析
//   - 不允许把query串混入值——RawQuery中包含 '?' 视为畸形请求，返回错误
//   - 非法数字返回错误（由调用方决定返回400）
//   - 缺省值：limit=20, offset=0
//   - limit范围 1..100，offset >= 0
//   - 不做代理污染清洁（CleanRawQuery已移除）——代理配置错误应在代理层修复，
//     后端收到畸形RawQuery应Fail Fast返回400并记录日志，而非静默兜底
package queryutil

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// ParsePagination 从HTTP请求解析分页参数 limit 和 offset。
//
// 引入动机：workspace、admin、document三个模块各自实现了几乎相同的
// parsePagination/parseDocPagination函数。本函数提供统一的分页解析逻辑。
//
// 行为：
//   - 使用 r.URL.Query() 解析标准查询参数（Go标准库正确处理 ?offset=0&limit=20）
//   - 如果 RawQuery 包含 '?' 字符，视为代理层污染，记录警告日志并返回错误（Fail Fast）
//   - limit 默认20，范围 1..100
//   - offset 默认0，不允许负数
//   - 非法数字（包含非数字字符）返回错误
//   - 空值使用默认值
//   - 未知参数被忽略（与现有契约一致）
//
// 参数：
//   - r：HTTP请求
//
// 返回 limit, offset 和错误（非法参数时）。
func ParsePagination(r *http.Request) (int, int, error) {
	// Fail Fast：如果 RawQuery 包含 '?'，说明代理层（Nginx/Nitro）配置错误，
	// 将完整URI（含query）转发时导致 query 被拼接到 path 中。
	// 不做静默清洁，直接拒绝并记录日志，让运维发现问题。
	if strings.Contains(r.URL.RawQuery, "?") {
		slog.Warn("拒绝畸形RawQuery：包含 '?' 字符，疑似代理层配置错误",
			"raw_query", r.URL.RawQuery,
			"path", r.URL.Path,
		)
		return 0, 0, fmt.Errorf("畸形query参数：RawQuery不应包含 '?' 字符")
	}

	q := r.URL.Query()

	limit := 20
	offset := 0

	if v := q.Get("limit"); v != "" {
		n, err := parseIntStrict(v)
		if err != nil {
			return 0, 0, fmt.Errorf("limit 参数非法: %w", err)
		}
		if n <= 0 {
			return 0, 0, fmt.Errorf("limit 必须为正整数")
		}
		if n > 100 {
			return 0, 0, fmt.Errorf("limit 不能超过 100")
		}
		limit = n
	}

	if v := q.Get("offset"); v != "" {
		n, err := parseIntStrict(v)
		if err != nil {
			return 0, 0, fmt.Errorf("offset 参数非法: %w", err)
		}
		if n < 0 {
			return 0, 0, fmt.Errorf("offset 不能为负数")
		}
		offset = n
	}

	return limit, offset, nil
}

// parseIntStrict 严格解析整数字符串，拒绝非数字输入。
// 引入动机：分页参数必须严格验证，拒绝畸形输入。
// 与各模块原有的 parseIntStrict 行为一致：仅接受 0-9 字符，拒绝负号、小数点、字母等。
func parseIntStrict(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("空字符串")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("包含非数字字符: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
