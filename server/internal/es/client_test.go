// es_test.go 测试 ES 客户端的 fake 实现和工具函数。
//
// 引入动机：design/06-IMPLEMENTATION.md §Tests 要求使用可控 ES client contract/fake
// 验证 server 生成 workspace filter、special files 排除、alias 切换/rebuild/integrity repair。
package es

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FakeClient 是 ES Client 接口的内存 fake 实现，用于测试。
// 引入动机：测试搜索管线和 integrity check 时需要可控的 ES 客户端，
// 不依赖真实 ES 实例。
type FakeClient struct {
	indices    map[string]bool
	aliasIndex string
	docs       map[string]map[string]map[string]interface{} // index -> docID -> body
	pingOK     bool
}

// NewFakeClient 创建测试用 fake ES 客户端。
func NewFakeClient() *FakeClient {
	return &FakeClient{
		indices: make(map[string]bool),
		docs:    make(map[string]map[string]map[string]interface{}),
		pingOK:  true,
	}
}

func (c *FakeClient) Ping(ctx context.Context) error {
	if !c.pingOK {
		return errFakeESUnavailable
	}
	return nil
}

func (c *FakeClient) CreateIndex(ctx context.Context, indexName string, mapping map[string]interface{}) error {
	c.indices[indexName] = true
	c.docs[indexName] = make(map[string]map[string]interface{})
	return nil
}

func (c *FakeClient) DeleteIndex(ctx context.Context, indexName string) error {
	delete(c.indices, indexName)
	delete(c.docs, indexName)
	return nil
}

func (c *FakeClient) IndexExists(ctx context.Context, indexName string) (bool, error) {
	return c.indices[indexName], nil
}

func (c *FakeClient) UpdateAlias(ctx context.Context, actions []AliasAction) error {
	for _, a := range actions {
		if a.Action == "add" {
			c.aliasIndex = a.Index
		}
	}
	return nil
}

func (c *FakeClient) GetAliasIndex(ctx context.Context, alias string) (string, error) {
	if c.aliasIndex == "" {
		return "", errFakeAliasNotFound
	}
	return c.aliasIndex, nil
}

func (c *FakeClient) BulkIndex(ctx context.Context, indexName string, docs []IndexDoc) error {
	if c.docs[indexName] == nil {
		c.docs[indexName] = make(map[string]map[string]interface{})
	}
	for _, doc := range docs {
		c.docs[indexName][doc.ID] = doc.Body
	}
	return nil
}

func (c *FakeClient) DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error {
	// 简化：删除所有文档
	if c.docs[indexName] != nil {
		c.docs[indexName] = make(map[string]map[string]interface{})
	}
	return nil
}

func (c *FakeClient) Search(ctx context.Context, indexName string, query map[string]interface{}) (*SearchResponse, error) {
	resp := &SearchResponse{}
	if c.docs[indexName] == nil {
		return resp, nil
	}
	for id, body := range c.docs[indexName] {
		hit := struct {
			ID     string                 `json:"_id"`
			Score  float64                `json:"_score"`
			Source map[string]interface{} `json:"_source"`
		}{ID: id, Score: 1.0, Source: body}
		resp.Hits.Hits = append(resp.Hits.Hits, hit)
	}
	resp.Hits.Total.Value = int64(len(resp.Hits.Hits))
	return resp, nil
}

func (c *FakeClient) Count(ctx context.Context, indexName string, query map[string]interface{}) (int64, error) {
	if c.docs[indexName] == nil {
		return 0, nil
	}
	return int64(len(c.docs[indexName])), nil
}

func (c *FakeClient) GetDocument(ctx context.Context, indexName, docID string) (map[string]interface{}, error) {
	if c.docs[indexName] == nil {
		return nil, nil
	}
	return c.docs[indexName][docID], nil
}

func (c *FakeClient) Refresh(ctx context.Context, indexName string) error {
	return nil
}

func (c *FakeClient) ListIndices(ctx context.Context, pattern string) ([]string, error) {
	// 简化：返回所有已创建的索引名，不做 pattern 匹配
	// 测试场景中索引数量有限，直接返回全部即可
	result := make([]string, 0, len(c.indices))
	for name := range c.indices {
		result = append(result, name)
	}
	return result, nil
}

// SetPingOK 设置 ping 是否成功（模拟 ES 不可用）。
func (c *FakeClient) SetPingOK(ok bool) {
	c.pingOK = ok
}

// 错误定义
var errFakeESUnavailable = &fakeError{msg: "ES 不可用"}
var errFakeAliasNotFound = &fakeError{msg: "alias 不存在"}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

// --- 测试 ---

func TestGenerateIndexName(t *testing.T) {
	if name := GenerateIndexName(12); name != "knowledge_v12" {
		t.Errorf("GenerateIndexName(12) 应为 knowledge_v12，得到 %s", name)
	}
	if name := GenerateIndexName(1); name != "knowledge_v1" {
		t.Errorf("GenerateIndexName(1) 应为 knowledge_v1，得到 %s", name)
	}
}

func TestBuildIndexMapping(t *testing.T) {
	mapping := BuildIndexMapping(1024, "standard")

	settings, ok := mapping["settings"].(map[string]interface{})
	if !ok {
		t.Fatal("mapping 应包含 settings")
	}
	index, ok := settings["index"].(map[string]interface{})
	if !ok {
		t.Fatal("settings 应包含 index")
	}
	if index["number_of_shards"] != 1 {
		t.Error("number_of_shards 应为 1")
	}

	mappings, ok := mapping["mappings"].(map[string]interface{})
	if !ok {
		t.Fatal("mapping 应包含 mappings")
	}
	props, ok := mappings["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("mappings 应包含 properties")
	}

	embedding, ok := props["embedding"].(map[string]interface{})
	if !ok {
		t.Fatal("应包含 embedding 字段")
	}
	if embedding["type"] != "dense_vector" {
		t.Error("embedding type 应为 dense_vector")
	}
	if embedding["dims"] != 1024 {
		t.Errorf("embedding dims 应为 1024，得到 %v", embedding["dims"])
	}

	// 验证 BM25 字段
	for _, field := range []string{"title", "heading", "content", "section_path"} {
		if _, ok := props[field]; !ok {
			t.Errorf("mapping 应包含 %s 字段", field)
		}
	}
}

func TestSwitchAlias_NewAlias(t *testing.T) {
	client := NewFakeClient()
	ctx := context.Background()

	// 创建索引
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)

	// 切换 alias（alias 不存在时应直接创建）
	err := SwitchAlias(ctx, client, AliasName, "knowledge_v1")
	if err != nil {
		t.Fatalf("SwitchAlias 失败: %v", err)
	}

	aliasIndex, err := client.GetAliasIndex(ctx, AliasName)
	if err != nil {
		t.Fatalf("获取 alias 失败: %v", err)
	}
	if aliasIndex != "knowledge_v1" {
		t.Errorf("alias 应指向 knowledge_v1，得到 %s", aliasIndex)
	}
}

func TestSwitchAlias_ExistingAlias(t *testing.T) {
	client := NewFakeClient()
	ctx := context.Background()

	_ = client.CreateIndex(ctx, "knowledge_v1", nil)
	_ = client.CreateIndex(ctx, "knowledge_v2", nil)

	// 首次切换
	_ = SwitchAlias(ctx, client, AliasName, "knowledge_v1")

	// 再次切换到 v2
	err := SwitchAlias(ctx, client, AliasName, "knowledge_v2")
	if err != nil {
		t.Fatalf("SwitchAlias 失败: %v", err)
	}

	aliasIndex, _ := client.GetAliasIndex(ctx, AliasName)
	if aliasIndex != "knowledge_v2" {
		t.Errorf("alias 应切换到 knowledge_v2，得到 %s", aliasIndex)
	}
}

func TestFakeClient_BulkIndexAndSearch(t *testing.T) {
	client := NewFakeClient()
	ctx := context.Background()

	_ = client.CreateIndex(ctx, "test_index", nil)

	docs := []IndexDoc{
		{ID: "doc1", Body: map[string]interface{}{"title": "Doc 1", "workspace_id": "ws1"}},
		{ID: "doc2", Body: map[string]interface{}{"title": "Doc 2", "workspace_id": "ws1"}},
	}

	if err := client.BulkIndex(ctx, "test_index", docs); err != nil {
		t.Fatalf("BulkIndex 失败: %v", err)
	}

	resp, err := client.Search(ctx, "test_index", nil)
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	if resp.Hits.Total.Value != 2 {
		t.Errorf("应有 2 个文档，得到 %d", resp.Hits.Total.Value)
	}
}

func TestFakeClient_PingUnavailable(t *testing.T) {
	client := NewFakeClient()
	client.SetPingOK(false)

	err := client.Ping(context.Background())
	if err == nil {
		t.Error("ES 不可用时 Ping 应返回错误")
	}
}

func TestHTTPClient_BulkIndexIncludesItemErrorDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_bulk" {
			t.Fatalf("请求路径 = %s, 期望 /_bulk", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":true,"items":[{"index":{"_index":"safe-index","_id":"doc-42","status":400,"error":{"type":"mapper_parsing_exception","reason":"field rejected"}}}]}`))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, time.Second)
	err := client.BulkIndex(context.Background(), "safe-index", []IndexDoc{{ID: "doc-42", Body: map[string]interface{}{"token": "do-not-log"}}})
	if err == nil {
		t.Fatal("BulkIndex 应返回 item 错误")
	}
	message := err.Error()
	for _, expected := range []string{"mapper_parsing_exception", "field rejected", "doc-42", "400"} {
		if !strings.Contains(message, expected) {
			t.Errorf("错误 %q 应包含 %q", message, expected)
		}
	}
	if strings.Contains(message, "do-not-log") {
		t.Error("BulkIndex 错误不应泄露请求文档内容")
	}
}
