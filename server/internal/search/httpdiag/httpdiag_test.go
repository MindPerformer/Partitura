package httpdiag

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestBodySummary_IsBoundedAndValidUTF8(t *testing.T) {
	body := bytes.Repeat([]byte("中"), 1000)
	summary := BodySummary(body, "")
	if len([]byte(summary)) > MaxBodySummaryBytes {
		t.Fatalf("摘要字节数应不超过 %d，实际 %d", MaxBodySummaryBytes, len([]byte(summary)))
	}
	if !IsValidUTF8(summary) {
		t.Fatal("摘要必须是有效 UTF-8")
	}
	if !strings.HasSuffix(summary, "…[truncated]") {
		t.Fatalf("超长摘要应带截断标记，实际后缀 %q", summary[len(summary)-len("…[truncated"):])
	}
}

func TestRedactSensitive_RemovesAuthorization(t *testing.T) {
	value := `Authorization: Bearer abc123, authorization = Bearer xyz789`
	redacted := RedactSensitive(value, "")
	if strings.Contains(redacted, "abc123") || strings.Contains(redacted, "xyz789") {
		t.Fatalf("Authorization token 不应出现在脱敏结果中: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("脱敏结果应包含占位符: %s", redacted)
	}
}

func TestLogFailure_ContainsDiagnosticsWithoutSecret(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(old)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	LogFailure(ctx, slog.LevelWarn, "request failed", Diagnostics{
		StartedAt: time.Now().Add(-20 * time.Millisecond), URL: "https://example.test/v1/embeddings?api_key=hidden",
		Model: "model", InputCount: 2, RequestBytes: 17, ConfiguredTimeout: 5 * time.Second,
		HTTPStatus: 502, ContentType: "text/html", RequestBody: []byte(`{"model":"model"}`),
		ResponseBody: []byte("gateway failure"), Err: context.DeadlineExceeded, Secret: "hidden",
	})

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("日志应是有效 JSON: %v", err)
	}
	for _, key := range []string{"duration_ms", "url", "model", "input_count", "request_bytes", "configured_timeout", "context_deadline", "context_remaining_ms", "status", "content_type", "request_body_summary", "response_body_summary", "error"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("结构化日志缺少字段 %q: %s", key, output.String())
		}
	}
	if record["url"] != "https://example.test/v1/embeddings" {
		t.Fatalf("URL 应移除 query，实际 %v", record["url"])
	}
	if strings.Contains(output.String(), "hidden") || strings.Contains(output.String(), "Authorization") {
		t.Fatalf("日志不得包含 secret 或 Authorization: %s", output.String())
	}
	if record["status"] != float64(502) || record["input_count"] != float64(2) || record["request_bytes"] != float64(17) {
		t.Fatalf("基础诊断字段不正确: %s", output.String())
	}
}
