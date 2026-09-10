// Package httpdiag 提供 HTTP Provider 失败诊断所需的安全结构化日志工具。
package httpdiag

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxBodySummaryBytes 限制请求和响应 body 摘要的字节数，避免异常 body 撑爆日志。
const MaxBodySummaryBytes = 512

var authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s,;]+`)

// Diagnostics 描述一次 Provider HTTP 请求，供所有 HTTP Provider 统一记录失败上下文。
type Diagnostics struct {
	StartedAt         time.Time
	URL               string
	Model             string
	InputCount        int
	RequestBytes      int
	ConfiguredTimeout time.Duration
	HTTPStatus        int
	ContentType       string
	RequestBody       []byte
	ResponseBody      []byte
	Err               error
	Secret            string
}

// LogFailure 以统一字段记录 HTTP 请求/响应失败，绝不记录认证头。
func LogFailure(ctx context.Context, level slog.Level, message string, d Diagnostics) {
	attrs := []slog.Attr{
		slog.Int64("duration_ms", time.Since(d.StartedAt).Milliseconds()),
		slog.String("url", SafeURL(d.URL)),
		slog.String("model", d.Model),
		slog.Int("input_count", d.InputCount),
		slog.Int("request_bytes", d.RequestBytes),
		slog.Duration("configured_timeout", d.ConfiguredTimeout),
		slog.Int("configured_timeout_seconds", int(d.ConfiguredTimeout/time.Second)),
		slog.Int("status", d.HTTPStatus),
		slog.Int("http_status", d.HTTPStatus),
		slog.String("content_type", d.ContentType),
		slog.String("request_body_summary", BodySummary(d.RequestBody, d.Secret)),
		slog.String("response_body_summary", BodySummary(d.ResponseBody, d.Secret)),
	}
	if d.Err != nil {
		attrs = append(attrs, slog.String("error", RedactSensitive(d.Err.Error(), d.Secret)))
	}
	attrs = append(attrs, ContextAttrs(ctx)...)
	slog.LogAttrs(ctx, level, message, attrs...)
}

// ContextAttrs 返回 context deadline 及调用日志时的剩余时间。
func ContextAttrs(ctx context.Context) []slog.Attr {
	deadline, ok := ctx.Deadline()
	if !ok {
		return []slog.Attr{
			slog.String("context_deadline", "none"),
			slog.Int64("context_remaining_ms", -1),
		}
	}
	remaining := time.Until(deadline)
	return []slog.Attr{
		slog.Time("context_deadline", deadline),
		slog.Int64("context_remaining_ms", remaining.Milliseconds()),
	}
}

// SafeURL 删除 URL 中可能承载凭据的 userinfo、query 和 fragment，仅保留请求定位信息。
func SafeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return parsed.Path
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.EscapedPath()
}

// RedactSensitive 删除指定 secret，供日志和上游错误文本使用。
func RedactSensitive(value, secret string) string {
	if secret != "" {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return authorizationPattern.ReplaceAllString(value, `${1}[REDACTED]`)
}

// BodySummary 返回有限长度且保证 UTF-8 有效的 body 摘要。
func BodySummary(body []byte, secret string) string {
	if len(body) == 0 {
		return "<empty>"
	}
	const suffix = "…[truncated]"
	maxContent := MaxBodySummaryBytes - len(suffix)
	content := body
	truncated := len(content) > maxContent
	if truncated {
		content = content[:maxContent]
	}
	result := strings.ToValidUTF8(string(content), "�")
	result = RedactSensitive(result, secret)
	if truncated {
		result += suffix
	}
	return result
}

// IsValidUTF8 是测试和调用方可复用的 body 摘要不变量检查。
func IsValidUTF8(value string) bool { return utf8.ValidString(value) }
