// es_alias_httptest_test.go 验证 UpdateAlias 发送给 ES8 的 JSON 格式符合 ES8 _aliases API 规范。
//
// 引入动机：Phase 6 修复要求 ES8 _aliases 请求体使用嵌套 JSON 格式：
//   {"actions":[{"add":{"index":"knowledge_v1","alias":"knowledge_current"}}]}
// 而非扁平格式 {"actions":[{"Action":"add","Index":"...","Alias":"..."}]}。
//
// 本测试使用 httptest.Server 作为 ES mock，捕获 UpdateAlias 发送的 HTTP 请求体，
// 验证其 JSON 结构符合 ES8 规范。
package es

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUpdateAlias_ES8JSONFormat 验证 UpdateAlias 发送的 JSON 格式符合 ES8 _aliases API 规范。
// 引入动机：ES8 _aliases 端点要求嵌套 JSON 格式，原实现使用扁平格式导致 ES8 返回 400 错误。
func TestUpdateAlias_ES8JSONFormat(t *testing.T) {
	var capturedBody []byte

	// 创建 httptest server 模拟 ES _aliases 端点
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_aliases" {
			t.Errorf("请求路径应为 /_aliases，实际为 %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("请求方法应为 POST，实际为 %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type 应为 application/json，实际为 %s", r.Header.Get("Content-Type"))
		}

		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"acknowledged":true}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*1e9)
	ctx := context.Background()

	actions := []AliasAction{
		{Action: "add", Index: "knowledge_v1", Alias: "knowledge_current"},
	}

	if err := client.UpdateAlias(ctx, actions); err != nil {
		t.Fatalf("UpdateAlias 失败: %v", err)
	}

	// 解析捕获的请求体并验证 ES8 嵌套 JSON 格式
	var body map[string]json.RawMessage
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("解析请求体 JSON 失败: %v\n请求体: %s", err, string(capturedBody))
	}

	actionsRaw, ok := body["actions"]
	if !ok {
		t.Fatal("请求体应包含 actions 字段")
	}

	var actionsList []map[string]json.RawMessage
	if err := json.Unmarshal(actionsRaw, &actionsList); err != nil {
		t.Fatalf("解析 actions 数组失败: %v", err)
	}

	if len(actionsList) != 1 {
		t.Fatalf("actions 应有 1 个元素，实际有 %d 个", len(actionsList))
	}

	// 验证嵌套结构：{"add":{"index":"...","alias":"..."}}
	addRaw, ok := actionsList[0]["add"]
	if !ok {
		t.Fatal("action 元素应包含 'add' key，但未找到。这证明使用了 ES8 嵌套格式而非扁平格式")
	}

	var addParams struct {
		Index string `json:"index"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(addRaw, &addParams); err != nil {
		t.Fatalf("解析 add 参数失败: %v", err)
	}

	if addParams.Index != "knowledge_v1" {
		t.Errorf("add.index 应为 knowledge_v1，实际为 %s", addParams.Index)
	}
	if addParams.Alias != "knowledge_current" {
		t.Errorf("add.alias 应为 knowledge_current，实际为 %s", addParams.Alias)
	}

	// 验证不存在扁平格式的字段
	if _, exists := actionsList[0]["Action"]; exists {
		t.Error("ES8 格式不应包含扁平的 'Action' 字段")
	}
	if _, exists := actionsList[0]["Index"]; exists {
		t.Error("ES8 格式不应包含扁平的 'Index' 字段")
	}
	if _, exists := actionsList[0]["Alias"]; exists {
		t.Error("ES8 格式不应包含扁平的 'Alias' 字段")
	}
}

// TestUpdateAlias_ES8RemoveAndAdd 验证 remove + add 组合操作的 ES8 JSON 格式。
// 引入动机：alias 原子切换需要 remove old + add new，验证两个操作都使用嵌套格式。
func TestUpdateAlias_ES8RemoveAndAdd(t *testing.T) {
	var capturedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"acknowledged":true}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 30*1e9)
	ctx := context.Background()

	actions := []AliasAction{
		{Action: "remove", Index: "knowledge_v1", Alias: "knowledge_current"},
		{Action: "add", Index: "knowledge_v2", Alias: "knowledge_current"},
	}

	if err := client.UpdateAlias(ctx, actions); err != nil {
		t.Fatalf("UpdateAlias 失败: %v", err)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("解析请求体 JSON 失败: %v", err)
	}

	var actionsList []map[string]json.RawMessage
	if err := json.Unmarshal(body["actions"], &actionsList); err != nil {
		t.Fatalf("解析 actions 数组失败: %v", err)
	}

	if len(actionsList) != 2 {
		t.Fatalf("actions 应有 2 个元素，实际有 %d 个", len(actionsList))
	}

	// 第一个操作：remove
	if _, ok := actionsList[0]["remove"]; !ok {
		t.Fatal("第一个操作应包含 'remove' key")
	}
	var removeParams struct {
		Index string `json:"index"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(actionsList[0]["remove"], &removeParams); err != nil {
		t.Fatalf("解析 remove 参数失败: %v", err)
	}
	if removeParams.Index != "knowledge_v1" {
		t.Errorf("remove.index 应为 knowledge_v1，实际为 %s", removeParams.Index)
	}

	// 第二个操作：add
	if _, ok := actionsList[1]["add"]; !ok {
		t.Fatal("第二个操作应包含 'add' key")
	}
	var addParams struct {
		Index string `json:"index"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(actionsList[1]["add"], &addParams); err != nil {
		t.Fatalf("解析 add 参数失败: %v", err)
	}
	if addParams.Index != "knowledge_v2" {
		t.Errorf("add.index 应为 knowledge_v2，实际为 %s", addParams.Index)
	}
}

// TestAliasAction_MarshalJSON 验证 AliasAction 的 MarshalJSON 输出。
// 引入动机：直接单元测试序列化输出，确保 ES8 嵌套格式正确。
func TestAliasAction_MarshalJSON(t *testing.T) {
	// 测试 add 操作
	addAction := AliasAction{Action: "add", Index: "knowledge_v1", Alias: "knowledge_current"}
	data, err := json.Marshal(addAction)
	if err != nil {
		t.Fatalf("MarshalJSON 失败: %v", err)
	}

	expected := `{"add":{"index":"knowledge_v1","alias":"knowledge_current"}}`
	if !bytes.Equal(data, []byte(expected)) {
		t.Errorf("add 序列化结果不符预期\n期望: %s\n实际: %s", expected, string(data))
	}

	// 测试 remove 操作
	removeAction := AliasAction{Action: "remove", Index: "knowledge_v1", Alias: "knowledge_current"}
	data, err = json.Marshal(removeAction)
	if err != nil {
		t.Fatalf("MarshalJSON 失败: %v", err)
	}

	expected = `{"remove":{"index":"knowledge_v1","alias":"knowledge_current"}}`
	if !bytes.Equal(data, []byte(expected)) {
		t.Errorf("remove 序列化结果不符预期\n期望: %s\n实际: %s", expected, string(data))
	}
}
