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

	// indexDimensions 记录各索引的 embedding 维度，供 GetIndexDimensions 返回。
	// 未记录的索引（含不存在的索引）返回 (0, nil)，与 HTTPClient 对 404
	// 和缺少 embedding 字段的语义保持一致。
	indexDimensions map[string]int
	// listIndicesErr 非 nil 时 ListIndices 返回该错误，用于验证调用方不吞错误。
	listIndicesErr error
	// lastListPattern 记录最后一次 ListIndices 使用的 pattern，便于测试断言调用约定。
	lastListPattern string
}

// NewFakeClient 创建测试用 fake ES 客户端。
func NewFakeClient() *FakeClient {
	return &FakeClient{
		indices:         make(map[string]bool),
		docs:            make(map[string]map[string]map[string]interface{}),
		pingOK:          true,
		indexDimensions: make(map[string]int),
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

// GetIndexDimensions 返回 fake 中记录的索引 embedding 维度。
// 引入动机：测试需要在不依赖真实 ES 的前提下模拟"已存在索引的 mapping 维度为 N"，
// 以及"索引不存在/无 embedding 字段 → (0, nil)"，用于验证调用方的维度不一致判断。
func (c *FakeClient) GetIndexDimensions(ctx context.Context, indexName string) (int, error) {
	return c.indexDimensions[indexName], nil
}

// SetIndexDimensions 设置某索引的 embedding 维度，模拟已存在索引的 mapping dims。
func (c *FakeClient) SetIndexDimensions(indexName string, dims int) {
	c.indexDimensions[indexName] = dims
}

// SetListIndicesErr 设置 ListIndices 是否返回错误，用于验证调用方不吞错误。
func (c *FakeClient) SetListIndicesErr(err error) {
	c.listIndicesErr = err
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
	c.lastListPattern = pattern
	if c.listIndicesErr != nil {
		return nil, c.listIndicesErr
	}
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
var errFakeListIndices = &fakeError{msg: "ListIndices 失败"}

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

// var _ Client 在编译期断言 FakeClient 完整实现 Client 接口。
// 引入动机：Client 接口新增方法后，若 fake 未同步实现，会让依赖该 fake 的测试静默失配。
var _ Client = (*FakeClient)(nil)

// TestGetIndexDimensions 验证 HTTPClient 从 _mapping 响应读取真实 embedding 维度。
// 引入动机：index_document 需要据此检测 search profile 的 embedding 维度与既有索引
// mapping 维度不一致，从而创建新索引重建，而不是把向量写入维度不可变的旧索引。
func TestGetIndexDimensions(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantDims int
		wantErr  bool
	}{
		{
			name:     "200 且 dims=4096 返回真实维度",
			status:   http.StatusOK,
			body:     `{"knowledge_v1":{"mappings":{"properties":{"embedding":{"type":"dense_vector","dims":4096}}}}}`,
			wantDims: 4096,
		},
		{
			name:     "404 索引不存在返回 0 且无错误",
			status:   http.StatusNotFound,
			body:     `{"error":{"type":"index_not_found_exception","reason":"no such index [knowledge_v1]"}}`,
			wantDims: 0,
		},
		{
			name:     "200 但无 embedding 字段返回 0 且无错误",
			status:   http.StatusOK,
			body:     `{"knowledge_current":{"mappings":{"properties":{"title":{"type":"text"}}}}}`,
			wantDims: 0,
		},
		{
			name:    "200 但非法 JSON 返回错误",
			status:  http.StatusOK,
			body:    `{"knowledge_v1":{"mappings":`,
			wantErr: true,
		},
		{
			name:    "200 但 dims 非正数返回错误",
			status:  http.StatusOK,
			body:    `{"knowledge_v1":{"mappings":{"properties":{"embedding":{"type":"dense_vector","dims":0}}}}}`,
			wantErr: true,
		},
		{
			name:    "200 但响应无索引条目返回错误",
			status:  http.StatusOK,
			body:    `{}`,
			wantErr: true,
		},
		{
			name:    "200 但含多个索引条目返回错误",
			status:  http.StatusOK,
			body:    `{"knowledge_v1":{"mappings":{"properties":{"embedding":{"dims":1024}}}},"knowledge_v2":{"mappings":{"properties":{"embedding":{"dims":4096}}}}}`,
			wantErr: true,
		},
		{
			name:    "500 返回错误",
			status:  http.StatusInternalServerError,
			body:    `{"error":"boom"}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("请求方法 = %s, 期望 GET", r.Method)
				}
				if r.URL.Path != "/knowledge_v1/_mapping" {
					t.Errorf("请求路径 = %s, 期望 /knowledge_v1/_mapping", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := NewHTTPClient(server.URL, time.Second)
			dims, err := client.GetIndexDimensions(context.Background(), "knowledge_v1")

			if tc.wantErr {
				if err == nil {
					t.Fatalf("应返回错误，但得到 dims=%d", dims)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应返回错误: %v", err)
			}
			if dims != tc.wantDims {
				t.Errorf("dims = %d, 期望 %d", dims, tc.wantDims)
			}
		})
	}
}

// TestNextIndexName 验证版本化索引名的递增计算。
// 引入动机：维度不一致时需要一个新的 knowledge_v{n} 索引名承载正确维度的 mapping，
// 必须与既有索引集不冲突。
func TestNextIndexName(t *testing.T) {
	t.Run("索引集为空返回 knowledge_v1", func(t *testing.T) {
		client := NewFakeClient()

		name, err := NextIndexName(context.Background(), client)
		if err != nil {
			t.Fatalf("NextIndexName 失败: %v", err)
		}
		if name != "knowledge_v1" {
			t.Errorf("索引集为空应返回 knowledge_v1，得到 %s", name)
		}
		if client.lastListPattern != IndexNamePrefix+"*" {
			t.Errorf("ListIndices pattern = %q, 期望 %q", client.lastListPattern, IndexNamePrefix+"*")
		}
	})

	t.Run("返回最大版本号加一", func(t *testing.T) {
		client := NewFakeClient()
		ctx := context.Background()
		for _, index := range []string{"knowledge_v1", "knowledge_v3"} {
			if err := client.CreateIndex(ctx, index, nil); err != nil {
				t.Fatalf("CreateIndex(%s) 失败: %v", index, err)
			}
		}

		name, err := NextIndexName(ctx, client)
		if err != nil {
			t.Fatalf("NextIndexName 失败: %v", err)
		}
		if name != "knowledge_v4" {
			t.Errorf("最大版本号为 3 时应返回 knowledge_v4，得到 %s", name)
		}
	})

	t.Run("忽略非版本化索引名", func(t *testing.T) {
		client := NewFakeClient()
		ctx := context.Background()
		// fake 的 ListIndices 不按 pattern 过滤，因此这里同时放入不匹配 pattern 的
		// knowledge_current、other_v9，以及匹配 pattern 但非 knowledge_v{n} 形式的
		// knowledge_v1_backup，三者都必须被忽略。
		for _, index := range []string{"knowledge_v1", "knowledge_current", "knowledge_v1_backup", "other_v9"} {
			if err := client.CreateIndex(ctx, index, nil); err != nil {
				t.Fatalf("CreateIndex(%s) 失败: %v", index, err)
			}
		}

		name, err := NextIndexName(ctx, client)
		if err != nil {
			t.Fatalf("NextIndexName 失败: %v", err)
		}
		if name != "knowledge_v2" {
			t.Errorf("只有 knowledge_v1 是有效版本名，应返回 knowledge_v2，得到 %s", name)
		}
	})

	t.Run("ListIndices 出错时不吞错误", func(t *testing.T) {
		client := NewFakeClient()
		client.SetListIndicesErr(errFakeListIndices)

		name, err := NextIndexName(context.Background(), client)
		if err == nil {
			t.Fatal("ListIndices 出错时 NextIndexName 应返回错误")
		}
		if name != "" {
			t.Errorf("出错时应返回空索引名，得到 %s", name)
		}
		if !strings.Contains(err.Error(), errFakeListIndices.Error()) {
			t.Errorf("错误 %q 应包装底层错误 %q", err, errFakeListIndices.Error())
		}
	})
}

// TestParseIndexVersion 直接验证索引名版本号解析，包括必须被拒绝的畸形名字。
func TestParseIndexVersion(t *testing.T) {
	tests := []struct {
		index       string
		wantVersion int
		wantOK      bool
	}{
		{index: "knowledge_v1", wantVersion: 1, wantOK: true},
		{index: "knowledge_v12", wantVersion: 12, wantOK: true},
		{index: "knowledge_current", wantOK: false},
		{index: "knowledge_v", wantOK: false},
		{index: "knowledge_v1_backup", wantOK: false},
		{index: "knowledge_v-1", wantOK: false},
		{index: "knowledge_vone", wantOK: false},
		{index: "other_v2", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.index, func(t *testing.T) {
			version, ok := parseIndexVersion(tc.index)
			if ok != tc.wantOK {
				t.Fatalf("parseIndexVersion(%q) ok = %v, 期望 %v", tc.index, ok, tc.wantOK)
			}
			if version != tc.wantVersion {
				t.Errorf("parseIndexVersion(%q) version = %d, 期望 %d", tc.index, version, tc.wantVersion)
			}
		})
	}
}

// TestFakeClient_GetIndexDimensions 验证 fake 能表达"某索引 dims 为 N"和"索引不存在"。
func TestFakeClient_GetIndexDimensions(t *testing.T) {
	client := NewFakeClient()
	ctx := context.Background()

	if err := client.CreateIndex(ctx, "knowledge_v1", nil); err != nil {
		t.Fatalf("CreateIndex 失败: %v", err)
	}
	client.SetIndexDimensions("knowledge_v1", 4096)

	dims, err := client.GetIndexDimensions(ctx, "knowledge_v1")
	if err != nil {
		t.Fatalf("GetIndexDimensions 失败: %v", err)
	}
	if dims != 4096 {
		t.Errorf("dims = %d, 期望 4096", dims)
	}

	// 未设置维度的索引（含不存在的索引）返回 (0, nil)，与 HTTPClient 的 404 语义一致。
	dims, err = client.GetIndexDimensions(ctx, "knowledge_v2")
	if err != nil {
		t.Fatalf("索引不存在时不应返回错误: %v", err)
	}
	if dims != 0 {
		t.Errorf("索引不存在时 dims = %d, 期望 0", dims)
	}
}
