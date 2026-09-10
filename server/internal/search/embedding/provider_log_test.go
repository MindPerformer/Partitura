package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	types "partitura/server/internal/search/types"
)

func TestOpenAICompatibleProvider_ErrorLogReadsAndBoundsBody(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(old)

	secret := "sk-embedding-secret"
	responseBody := strings.Repeat("x", 2000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{BaseURL: server.URL, APIKey: secret, Model: "test", Dimensions: 2, TimeoutSeconds: 7})
	_, err := provider.Embed(context.Background(), []string{"hello", "world"})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("非 2xx 应保留状态错误，实际 %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("应记录 JSON 日志: %v", err)
	}
	if record["request_bytes"] == float64(0) || record["input_count"] != float64(2) || record["configured_timeout_seconds"] != float64(7) {
		t.Fatalf("请求诊断字段错误: %s", output.String())
	}
	if len([]byte(record["response_body_summary"].(string))) > 512 {
		t.Fatalf("响应摘要必须有界: %d", len([]byte(record["response_body_summary"].(string))))
	}
	if !strings.Contains(output.String(), "truncated") || strings.Contains(output.String(), secret) || strings.Contains(output.String(), "Authorization") {
		t.Fatalf("日志应截断且不得泄密: %s", output.String())
	}
}

func TestOpenAICompatibleProvider_TimeoutLogsContext(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(old)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer server.Close()
	provider := NewOpenAICompatibleProvider(types.EmbeddingConfig{BaseURL: server.URL, Model: "test", Dimensions: 2, TimeoutSeconds: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := provider.Embed(ctx, []string{"hello"})
	if err == nil {
		t.Fatal("context 超时应返回错误")
	}
	var record map[string]any
	if json.Unmarshal(output.Bytes(), &record) != nil || record["context_deadline"] == "none" || record["context_remaining_ms"] == nil {
		t.Fatalf("超时日志应包含 context deadline/remaining: %s", output.String())
	}
}
