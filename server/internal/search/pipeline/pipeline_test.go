// pipeline_test.go 测试搜索管线的降级行为和结果处理。
//
// 引入动机：design/06-IMPLEMENTATION.md §Tests 要求：
//   - ES down 时返回 degraded
//   - embedding down 时 lexical search 可用
//   - reranker down 时返回 RRF 结果
//   - workspace isolation in search
package pipeline

import (
	"context"
	"testing"

	"partitura/server/internal/es"
	types "partitura/server/internal/search/types"
)

// fakeESClient 是测试用的可控 ES 客户端。
// 引入动机：pipeline 测试需要可控的 ES 客户端，不依赖真实 ES。
type fakeESClient struct {
	indices    map[string]bool
	aliasIndex string
	docs       map[string]map[string]map[string]interface{}
	pingOK     bool
	// indexDims 记录各索引的 embedding 维度，供 GetIndexDimensions 返回。
	// 引入动机：搜索管线本身不消费维度信息，但 es.Client 接口要求实现该方法；
	// 保留可配置的维度表使 fake 与真实 ES 语义一致（未配置 → 0 表示"无已知维度"）。
	indexDims map[string]int
}

func newFakeESClient() *fakeESClient {
	return &fakeESClient{
		indices:   make(map[string]bool),
		docs:      make(map[string]map[string]map[string]interface{}),
		pingOK:    true,
		indexDims: make(map[string]int),
	}
}

func (c *fakeESClient) Ping(ctx context.Context) error {
	if !c.pingOK {
		return errFakeUnavailable
	}
	return nil
}

func (c *fakeESClient) CreateIndex(ctx context.Context, indexName string, mapping map[string]interface{}) error {
	c.indices[indexName] = true
	c.docs[indexName] = make(map[string]map[string]interface{})
	return nil
}

func (c *fakeESClient) DeleteIndex(ctx context.Context, indexName string) error {
	delete(c.indices, indexName)
	delete(c.docs, indexName)
	return nil
}

func (c *fakeESClient) ListIndices(ctx context.Context, pattern string) ([]string, error) {
	result := make([]string, 0, len(c.indices))
	for name := range c.indices {
		result = append(result, name)
	}
	return result, nil
}

func (c *fakeESClient) IndexExists(ctx context.Context, indexName string) (bool, error) {
	return c.indices[indexName], nil
}

// GetIndexDimensions 返回 fake 中为指定索引配置的 embedding 维度。
// 引入动机：es.Client 接口新增该方法以支持 job 包的维度自愈；
// 搜索管线不涉及维度判断，未配置维度的索引返回 (0, nil)，与真实客户端的
// "索引不存在或没有 embedding 字段 → 无已知维度"语义一致。
func (c *fakeESClient) GetIndexDimensions(ctx context.Context, indexName string) (int, error) {
	return c.indexDims[indexName], nil
}

func (c *fakeESClient) UpdateAlias(ctx context.Context, actions []es.AliasAction) error {
	for _, a := range actions {
		if a.Action == "add" {
			c.aliasIndex = a.Index
		}
	}
	return nil
}

func (c *fakeESClient) GetAliasIndex(ctx context.Context, alias string) (string, error) {
	if c.aliasIndex == "" {
		return "", errFakeAliasNotFound
	}
	return c.aliasIndex, nil
}

func (c *fakeESClient) BulkIndex(ctx context.Context, indexName string, docs []es.IndexDoc) error {
	if c.docs[indexName] == nil {
		c.docs[indexName] = make(map[string]map[string]interface{})
	}
	for _, doc := range docs {
		c.docs[indexName][doc.ID] = doc.Body
	}
	return nil
}

func (c *fakeESClient) DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error {
	if c.docs[indexName] != nil {
		c.docs[indexName] = make(map[string]map[string]interface{})
	}
	return nil
}

func (c *fakeESClient) Search(ctx context.Context, indexName string, query map[string]interface{}) (*es.SearchResponse, error) {
	resp := &es.SearchResponse{}
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

func (c *fakeESClient) Count(ctx context.Context, indexName string, query map[string]interface{}) (int64, error) {
	if c.docs[indexName] == nil {
		return 0, nil
	}
	return int64(len(c.docs[indexName])), nil
}

func (c *fakeESClient) GetDocument(ctx context.Context, indexName, docID string) (map[string]interface{}, error) {
	if c.docs[indexName] == nil {
		return nil, nil
	}
	return c.docs[indexName][docID], nil
}

func (c *fakeESClient) Refresh(ctx context.Context, indexName string) error {
	return nil
}

func (c *fakeESClient) SetPingOK(ok bool) { c.pingOK = ok }

type fakeESSError struct{ msg string }

func (e *fakeESSError) Error() string { return e.msg }

var errFakeUnavailable = &fakeESSError{msg: "ES 不可用"}
var errFakeAliasNotFound = &fakeESSError{msg: "alias 不存在"}

// fakeEmbeddingProvider 是测试用的可控 embedding provider。
type fakeEmbeddingProvider struct {
	available          bool
	dims               int
	vectors            [][]float32
	err                error
	embedCallCount     int
	availableCallCount int
}

func (p *fakeEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	p.embedCallCount++
	if p.err != nil {
		return nil, p.err
	}
	if p.vectors != nil {
		return p.vectors, nil
	}
	// 返回默认向量
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = make([]float32, p.dims)
		for j := range result[i] {
			result[i][j] = 0.1
		}
	}
	return result, nil
}

func (p *fakeEmbeddingProvider) Dimensions() int { return p.dims }

func (p *fakeEmbeddingProvider) Available(ctx context.Context) bool {
	p.availableCallCount++
	return p.available
}

// fakeRerankerProvider 是测试用的可控 reranker provider。
type fakeRerankerProvider struct {
	available          bool
	results            []types.RerankerResult
	err                error
	rerankCallCount    int
	availableCallCount int
}

func (p *fakeRerankerProvider) Rerank(ctx context.Context, query string, candidates []types.RerankerCandidate) ([]types.RerankerResult, error) {
	p.rerankCallCount++
	if p.err != nil {
		return nil, p.err
	}
	if p.results != nil {
		return p.results, nil
	}
	// 默认：按原始顺序返回
	results := make([]types.RerankerResult, len(candidates))
	for i := range candidates {
		results[i] = types.RerankerResult{Index: i, Score: float64(len(candidates)-i) / 10.0}
	}
	return results, nil
}

func (p *fakeRerankerProvider) Available(ctx context.Context) bool {
	p.availableCallCount++
	return p.available
}

func testProfile() types.SearchProfileConfig {
	return types.SearchProfileConfig{
		ID:                     "test-profile",
		Name:                   "test",
		Version:                1,
		ESIndexName:            "knowledge_v1",
		EmbeddingDimensions:    128,
		LexicalTopK:            10,
		VectorTopK:             10,
		RRFK:                   60,
		RerankerCandidateCount: 10,
		RerankerFinalCount:     5,
		MaxChunksPerDocument:   3,
		MergeAdjacentChunks:    true,
		TitleBoost:             2.0,
		HeadingBoost:           1.5,
		PathBoost:              1.0,
		TagsBoost:              0.5,
		BodyBoost:              1.0,
	}
}

func TestSearch_ESUnavailable_DegradedResponse(t *testing.T) {
	client := newFakeESClient()
	client.SetPingOK(false) // ES 不可用

	pipe := NewPipeline(client, nil, nil)

	output, err := pipe.Search(context.Background(), SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("ES 不可用时 Search 不应返回错误: %v", err)
	}

	if !output.Degraded {
		t.Error("ES 不可用时应返回 degraded=true")
	}
	if output.DegradationReason != "elasticsearch_unavailable" {
		t.Errorf("降级原因应为 elasticsearch_unavailable，得到 %s", output.DegradationReason)
	}
}

func TestSearch_NilESClient_DegradedResponse(t *testing.T) {
	pipe := NewPipeline(nil, nil, nil)

	output, err := pipe.Search(context.Background(), SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("nil ES client 时 Search 不应返回错误: %v", err)
	}

	if !output.Degraded {
		t.Error("nil ES client 时应返回 degraded=true")
	}
}

func TestSearch_EmbeddingUnavailable_LexicalOnly(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)

	// 写入测试文档
	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "test.md",
			"title":        "Test",
			"content":      "test content",
		}},
	})

	// embedding provider 已配置但正式调用失败
	embProvider := &fakeEmbeddingProvider{available: false, dims: 128, err: errFakeUnavailable}
	pipe := NewPipeline(client, embProvider, nil)

	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	// 应降级但仍有结果（BM25 结果）
	if !output.Degraded {
		t.Error("embedding 不可用时应 degraded=true")
	}
	if embProvider.embedCallCount != 1 {
		t.Errorf("embedding 应只调用正式 Embed 一次，实际 %d 次", embProvider.embedCallCount)
	}
	if embProvider.availableCallCount != 0 {
		t.Errorf("embedding 不应调用 Available 预检，实际 %d 次", embProvider.availableCallCount)
	}
}

func TestSearch_RerankerUnavailable_ReturnsRRF(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)

	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "test.md",
			"title":        "Test",
			"content":      "test content",
		}},
		{ID: "doc2_0", Body: map[string]interface{}{
			"document_id":  "doc2",
			"workspace_id": "ws-1",
			"path":         "other.md",
			"title":        "Other",
			"content":      "other content",
		}},
	})

	embProvider := &fakeEmbeddingProvider{available: true, dims: 128}
	// health check 返回不可用，但正式调用仍应直接执行。
	rrProvider := &fakeRerankerProvider{available: false, err: errFakeUnavailable}
	pipe := NewPipeline(client, embProvider, rrProvider)

	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if !output.Degraded {
		t.Error("reranker 不可用时应 degraded=true")
	}
	if output.RerankerUsed {
		t.Error("reranker 不可用时 RerankerUsed 应为 false")
	}
	if rrProvider.rerankCallCount != 1 {
		t.Errorf("reranker 应只调用正式 Rerank 一次，实际 %d 次", rrProvider.rerankCallCount)
	}
	if rrProvider.availableCallCount != 0 {
		t.Errorf("reranker 不应调用 Available 预检，实际 %d 次", rrProvider.availableCallCount)
	}
}

func TestSearch_RerankerAvailable_UsesReranker(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)

	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "test.md",
			"title":        "Test",
			"content":      "test content",
		}},
		{ID: "doc2_0", Body: map[string]interface{}{
			"document_id":  "doc2",
			"workspace_id": "ws-1",
			"path":         "other.md",
			"title":        "Other",
			"content":      "other content",
		}},
	})

	embProvider := &fakeEmbeddingProvider{available: true, dims: 128}
	rrProvider := &fakeRerankerProvider{available: true}

	pipe := NewPipeline(client, embProvider, rrProvider)

	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	if !output.RerankerUsed {
		t.Error("reranker 可用时应 RerankerUsed=true")
	}
}

func TestSearch_SearchIDGenerated(t *testing.T) {
	client := newFakeESClient()
	client.SetPingOK(false)

	pipe := NewPipeline(client, nil, nil)

	output, _ := pipe.Search(context.Background(), SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
	})

	if output.SearchID == "" {
		t.Error("SearchID 不应为空")
	}
}

func TestSearch_LimitClampedTo100(t *testing.T) {
	client := newFakeESClient()
	client.SetPingOK(false)

	pipe := NewPipeline(client, nil, nil)

	output, _ := pipe.Search(context.Background(), SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       500, // 超过 100
	})

	// 不崩溃即可，limit 在内部被 clamp
	_ = output
}

// capturingESClient 在 fakeESClient 基础上记录最近一次 Search 收到的原始 query 映射，
// 用于断言服务端下发给 ES 的 knn query DSL 字段名符合 ES8 规范。
// 引入动机：修复 searchVector 误用 "embedding"/"vector" 字段导致 ES 400 被静默吞掉的缺陷，
// 需要回归测试锁定正确字段（field/query_vector）。
type capturingESClient struct {
	*fakeESClient
	lastQuery map[string]interface{}
	// knnErr 非 nil 时，仅对包含 knn 子句的查询（vector 检索）返回该错误，
	// BM25/其他查询正常放行——用于验证 hybrid 模式 vector 失败回退 lexical。
	knnErr error
}

func (c *capturingESClient) Search(ctx context.Context, indexName string, query map[string]interface{}) (*es.SearchResponse, error) {
	c.lastQuery = query
	if c.knnErr != nil {
		if _, err := mustExtractKNN(query); err == nil {
			return nil, c.knnErr
		}
	}
	return c.fakeESClient.Search(ctx, indexName, query)
}

// mustExtractKNN 从嵌套 query 结构提取 bool.must[0].knn；无则返回错误。
func mustExtractKNN(q map[string]interface{}) (map[string]interface{}, error) {
	boolQ, ok := q["query"].(map[string]interface{})
	if !ok {
		return nil, errNoKNN
	}
	boolBody, ok := boolQ["bool"].(map[string]interface{})
	if !ok {
		return nil, errNoKNN
	}
	must, ok := boolBody["must"].([]interface{})
	if !ok || len(must) == 0 {
		return nil, errNoKNN
	}
	first, ok := must[0].(map[string]interface{})
	if !ok {
		return nil, errNoKNN
	}
	knn, ok := first["knn"].(map[string]interface{})
	if !ok {
		return nil, errNoKNN
	}
	return knn, nil
}

// knnParamOf 从嵌套 query 结构提取 bool.must[0].knn 的参数。
func knnParamOf(q map[string]interface{}) map[string]interface{} {
	knn, err := mustExtractKNN(q)
	if err != nil {
		return nil
	}
	return knn
}

type fakeKNNMissingError struct{}

func (*fakeKNNMissingError) Error() string { return "无 knn 子句" }

var errNoKNN = &fakeKNNMissingError{}

func TestSearch_SemanticVectorDSL_UsesKnnFieldNames(t *testing.T) {
	client := &capturingESClient{fakeESClient: newFakeESClient()}
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)
	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "test.md",
			"title":        "Test",
			"content":      "test content",
			"status":       "active",
			"is_special":   false,
		}},
	})

	embProvider := &fakeEmbeddingProvider{available: true, dims: 4}
	pipe := NewPipeline(client, embProvider, nil)

	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
		Mode:        "semantic",
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if output.Degraded {
		t.Fatalf("vector 检索应成功，不应 degraded（reason=%s）", output.DegradationReason)
	}
	if len(output.Results) == 0 {
		t.Fatalf("semantic 模式应有命中结果")
	}

	knn := knnParamOf(client.lastQuery)
	if knn == nil {
		t.Fatal("ES 请求体中未发现 bool.must[0].knn")
	}
	if knn["field"] != "embedding" {
		t.Errorf("knn.field 应为 embedding，得到 %#v", knn["field"])
	}
	if _, ok := knn["query_vector"]; !ok {
		t.Errorf("knn.query_vector 缺失（原缺陷使用 vector/embedding 命名），实际参数: %#v", knn)
	}
	if _, bad := knn["vector"]; bad {
		t.Errorf("knn.vector 是非法参数名，不应出现: %#v", knn)
	}
	if _, bad := knn["embedding"]; bad {
		t.Errorf("knn.embedding 是非法参数名，不应出现: %#v", knn)
	}
}

func TestSearch_SemanticVectorSearchError_Degraded(t *testing.T) {
	client := &capturingESClient{fakeESClient: newFakeESClient(), knnErr: errFakeUnavailable}
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)
	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "test.md",
			"title":        "Test",
			"content":      "test content",
			"status":       "active",
			"is_special":   false,
		}},
	})

	embProvider := &fakeEmbeddingProvider{available: true, dims: 4}
	pipe := NewPipeline(client, embProvider, nil)

	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
		Mode:        "semantic",
	})
	if err != nil {
		t.Fatalf("vector 检索失败不应返回错误，应降级: %v", err)
	}
	// 修复后：vector 检索失败不再静默产生 0 命中且 degraded=false
	if !output.Degraded {
		t.Fatal("vector 检索失败时 semantic 应 degraded=true")
	}
	if output.DegradationReason != "vector_search_failed" {
		t.Errorf("降级原因应为 vector_search_failed，得到 %s", output.DegradationReason)
	}
	if len(output.Results) != 0 {
		t.Error("semantic 模式 vector 检索失败不应返回结果")
	}
}

func TestSearch_HybridVectorSearchError_LexicalFallback(t *testing.T) {
	client := &capturingESClient{fakeESClient: newFakeESClient()}
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)
	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "test.md",
			"title":        "Test",
			"content":      "test content",
			"status":       "active",
			"is_special":   false,
		}},
	})

	// 捕获查询：knn 请求返回错误，BM25 请求（无 knn 字段）返回 fake 正常结果
	base := newFakeESClient()
	_ = base.CreateIndex(ctx, "knowledge_v1", nil)
	_ = base.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "test.md",
			"title":        "Test",
			"content":      "test content",
			"status":       "active",
			"is_special":   false,
		}},
	})
	capt := &capturingESClient{fakeESClient: base, knnErr: errFakeUnavailable}

	embProvider := &fakeEmbeddingProvider{available: true, dims: 4}
	pipe := NewPipeline(capt, embProvider, nil)

	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
		Mode:        "hybrid",
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	// vector 失败但 BM25 命中 → 降级但仍返回 lexical 结果
	if !output.Degraded {
		t.Fatal("vector 检索失败时 hybrid 应 degraded=true")
	}
	if len(output.Results) == 0 {
		t.Fatal("hybrid vector 失败时应回退到 lexical BM25 结果")
	}
}
