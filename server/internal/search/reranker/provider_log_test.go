package reranker

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

func TestHTTPProvider_ErrorLogContainsRequestAndResponseSummary(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(old)

	secret := "sk-reranker-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream failed"}`))
	}))
	defer server.Close()
	provider := NewHTTPProvider(types.RerankerConfig{BaseURL: server.URL, APIKey: secret, Model: "rerank", MaxCandidates: 3, TimeoutSeconds: 8})
	_, err := provider.Rerank(context.Background(), "query", []types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("非 2xx 应返回状态错误: %v", err)
	}
	var record map[string]any
	if json.Unmarshal(output.Bytes(), &record) != nil {
		t.Fatalf("应记录结构化 JSON 日志: %s", output.String())
	}
	for _, key := range []string{"duration_ms", "url", "model", "input_count", "request_bytes", "configured_timeout", "context_deadline", "status", "content_type", "request_body_summary", "response_body_summary"} {
		if _, ok := record[key]; !ok {
			t.Fatalf("日志缺少 %q: %s", key, output.String())
		}
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "Authorization") {
		t.Fatalf("日志不得泄露认证信息: %s", output.String())
	}
}

func TestHTTPProvider_TimeoutLogsError(t *testing.T) {
	var output bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(old)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer server.Close()
	provider := NewHTTPProvider(types.RerankerConfig{BaseURL: server.URL, Model: "test", TimeoutSeconds: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := provider.Rerank(ctx, "query", []types.RerankerCandidate{{Text: "doc", Index: 0}})
	if err == nil {
		t.Fatal("context 超时应返回错误")
	}
	if !strings.Contains(output.String(), "context_remaining_ms") {
		t.Fatalf("日志应包含 context 剩余时间: %s", output.String())
	}
}
