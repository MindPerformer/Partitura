// switch_alias_test.go 测试 SwitchAlias 在各种场景下的行为。
//
// 引入动机：远程验收中发现 rebuild_index job 的 alias 切换返回 HTTP 400，
// 根因是 knowledge_current 是具体索引而非 alias，ES 不允许创建同名 alias。
// 此测试验证 SwitchAlias 能正确处理：
// 1. alias 不存在且无同名具体索引：直接创建 alias
// 2. alias 不存在但存在同名具体索引：先删除具体索引再创建 alias
// 3. alias 已存在：原子 remove old + add new
// 4. ES 返回 400 时保持失败（不 silent fallback）
package es

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSwitchAlias_CreateNewAlias 验证 alias 不存在时直接创建 alias。
func TestSwitchAlias_CreateNewAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_alias/knowledge_current":
			// alias 不存在
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"alias [knowledge_current] missing","status":404}`))
		case "/knowledge_current":
			// HEAD 请求检查索引是否存在 → 不存在
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
			}
		case "/_aliases":
			// alias 创建请求 → 成功
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*1e9)
	ctx := context.Background()

	err := SwitchAlias(ctx, client, "knowledge_current", "knowledge_v1")
	if err != nil {
		t.Fatalf("SwitchAlias 应成功创建新 alias: %v", err)
	}
}

// TestSwitchAlias_DeletesConcreteIndexThenCreateAlias 验证 alias 不存在但存在同名具体索引时，
// 先删除具体索引再创建 alias。
//
// 引入动机：远程验收中 knowledge_current 被 ES 自动创建为具体索引，
// SwitchAlias 应检测到并删除它，然后创建 alias。
func TestSwitchAlias_DeletesConcreteIndexThenCreateAlias(t *testing.T) {
	var deletedIndex string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_alias/knowledge_current":
			// alias 不存在
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"alias [knowledge_current] missing","status":404}`))
		case r.URL.Path == "/knowledge_current" && r.Method == http.MethodHead:
			// 索引存在
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/knowledge_current" && r.Method == http.MethodDelete:
			// 删除具体索引
			deletedIndex = "knowledge_current"
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"acknowledged":true}`))
		case r.URL.Path == "/_aliases":
			// alias 创建请求 → 成功
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*1e9)
	ctx := context.Background()

	err := SwitchAlias(ctx, client, "knowledge_current", "knowledge_v1")
	if err != nil {
		t.Fatalf("SwitchAlias 应成功删除具体索引并创建 alias: %v", err)
	}

	if deletedIndex != "knowledge_current" {
		t.Errorf("应删除同名具体索引 knowledge_current，实际删除了 %s", deletedIndex)
	}
}

// TestSwitchAlias_AtomicSwitchExistingAlias 验证 alias 已存在时原子切换。
func TestSwitchAlias_AtomicSwitchExistingAlias(t *testing.T) {
	var capturedActions []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_alias/knowledge_current":
			// alias 已存在，指向 knowledge_v1
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"knowledge_v1":{"aliases":{"knowledge_current":{}}}}`))
		case "/_aliases":
			// 捕获 alias 更新请求
			capturedActions, _ = readBody(r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*1e9)
	ctx := context.Background()

	err := SwitchAlias(ctx, client, "knowledge_current", "knowledge_v2")
	if err != nil {
		t.Fatalf("SwitchAlias 原子切换应成功: %v", err)
	}

	// 验证请求包含 remove + add 操作
	var body struct {
		Actions []map[string]json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal(capturedActions, &body); err != nil {
		t.Fatalf("解析 alias 更新请求失败: %v", err)
	}

	if len(body.Actions) != 2 {
		t.Fatalf("应有 2 个 action（remove + add），实际 %d", len(body.Actions))
	}

	// 验证第一个是 remove
	if _, ok := body.Actions[0]["remove"]; !ok {
		t.Error("第一个 action 应为 remove")
	}

	// 验证第二个是 add
	if _, ok := body.Actions[1]["add"]; !ok {
		t.Error("第二个 action 应为 add")
	}
}

// TestSwitchAlias_ES400_StaysFailed 验证 ES 返回 400 时 SwitchAlias 保持失败。
//
// 引入动机：禁止 silent fallback。当 ES 拒绝 alias 更新（如 400）时，
// SwitchAlias 必须返回 error，不应静默成功。
func TestSwitchAlias_ES400_StaysFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_alias/knowledge_current":
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"alias [knowledge_current] missing","status":404}`))
		case "/knowledge_current":
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK) // 索引存在
			} else if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusOK) // 删除成功
			}
		case "/_aliases":
			// ES 返回 400（如同名索引冲突）
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"type":"invalid_alias_name_exception","reason":"invalid alias name [knowledge_current]"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*1e9)
	ctx := context.Background()

	err := SwitchAlias(ctx, client, "knowledge_current", "knowledge_v1")
	if err == nil {
		t.Fatal("ES 返回 400 时 SwitchAlias 应返回错误，不应 silent fallback")
	}

	// 验证错误信息包含状态码
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("错误信息应包含状态码 400，实际: %s", err.Error())
	}
}

// TestUpdateAlias_400_ErrorBodyParsed 验证 UpdateAlias 在 400 时解析错误体。
func TestUpdateAlias_400_ErrorBodyParsed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_alias_name_exception","reason":"invalid alias name [knowledge_current]"}}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*1e9)
	ctx := context.Background()

	actions := []AliasAction{
		{Action: "add", Index: "knowledge_v1", Alias: "knowledge_current"},
	}

	err := client.UpdateAlias(ctx, actions)
	if err == nil {
		t.Fatal("400 响应应返回错误")
	}

	// 验证错误信息包含 ES 错误类型和原因
	if !strings.Contains(err.Error(), "invalid_alias_name_exception") {
		t.Errorf("错误信息应包含 ES 错误类型，实际: %s", err.Error())
	}
}

// readBody 读取 HTTP 请求体。
func readBody(r *http.Request) ([]byte, error) {
	buf := make([]byte, r.ContentLength)
	r.Body.Read(buf)
	return buf, nil
}
