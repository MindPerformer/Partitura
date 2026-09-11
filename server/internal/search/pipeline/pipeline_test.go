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
	"sort"
	"strings"
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
	// 引入动机：vector 检索失败需要结合索引真实维度细分降级原因
	// （维度不一致 / 无 embedding 字段 / 原因不明），fake 必须能表达这三种情形。
	// 未配置 → 0，表示"无已知维度"（索引不存在或没有 embedding 字段）。
	indexDims map[string]int
	// indexDimsErr 非 nil 时 GetIndexDimensions 直接返回该错误。
	// 引入动机：需要验证"维度读取自身失败"时管线仍保持 vector_search_failed，
	// 且该错误不会被静默忽略。
	indexDimsErr error
	// searchedIndices 按调用顺序记录 Search 收到的目标索引名。
	// 引入动机：design/01-SEARCH.md §Index Version 要求普通搜索打 alias
	// knowledge_current；需要真实观察管线下发给 ES 的索引名来锁定该行为。
	searchedIndices []string
	// highlights 记录各文档 ID 对应的高亮片段，Search 时挂到对应 hit 上。
	// 引入动机：buildSnippet 依赖 ES 返回的 highlight 生成命中点 snippet，
	// fake 需要能按文档可控地返回高亮，才能驱动 snippet 的命中/降级路径。
	highlights map[string]map[string][]string
	// lastQuery 记录最近一次 Search 收到的原始 query 映射。
	// 引入动机：需要断言 BM25 查询体包含 highlight 子句，
	// 必须观察真实下发给 ES 的请求体而非仅校验返回值。
	lastQuery map[string]interface{}
	// scores 记录各文档 ID 的 BM25 _score，Search 时作为 hit.Score 返回。
	// 引入动机：默认所有 hit Score=1.0 会导致同文档 chunk 同分，
	// 组内主 chunk 选举依赖 map 遍历顺序而不稳定；按 ID 可控 score
	// 让聚合的主结果与排序确定可断言。
	scores map[string]float64
}

// highlightFor 返回指定文档 ID 配置的高亮片段，未配置时返回 nil。
func (c *fakeESClient) highlightFor(docID string) map[string][]string {
	if c.highlights == nil {
		return nil
	}
	return c.highlights[docID]
}

// scoreFor 返回指定文档 ID 配置的 _score，未配置时返回默认 1.0。
func (c *fakeESClient) scoreFor(docID string) float64 {
	if s, ok := c.scores[docID]; ok {
		return s
	}
	return 1.0
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
// 引入动机：es.Client 接口要求实现该方法；搜索管线的 vector 失败分类需要依据
// 索引真实维度，因此 fake 必须能表达"某索引 dims=N"与"无已知维度"两种状态。
// 未配置维度的索引返回 (0, nil)，与真实客户端的
// "索引不存在或没有 embedding 字段 → 无已知维度"语义一致。
func (c *fakeESClient) GetIndexDimensions(ctx context.Context, indexName string) (int, error) {
	if c.indexDimsErr != nil {
		return 0, c.indexDimsErr
	}
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
	c.searchedIndices = append(c.searchedIndices, indexName)
	c.lastQuery = query
	resp := &es.SearchResponse{}
	if c.docs[indexName] == nil {
		return resp, nil
	}
	for id, body := range c.docs[indexName] {
		hit := struct {
			ID        string                 `json:"_id"`
			Score     float64                `json:"_score"`
			Source    map[string]interface{} `json:"_source"`
			Highlight map[string][]string    `json:"highlight"`
		}{ID: id, Score: c.scoreFor(id), Source: body, Highlight: c.highlightFor(id)}
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
	// 细分降级原因后：本用例的 fake 未为 knowledge_v1 登记 embedding 维度，
	// 对应真实客户端"索引不存在或没有 embedding 字段 → (0, nil)"的语义，
	// 因此降级原因应为 vector_field_missing，而不是笼统的 vector_search_failed。
	if output.DegradationReason != "vector_field_missing" {
		t.Errorf("降级原因应为 vector_field_missing，得到 %s", output.DegradationReason)
	}
	if len(output.Results) != 0 {
		t.Error("semantic 模式 vector 检索失败不应返回结果")
	}
}

// newVectorFailureFixture 构造"vector 检索必然失败、lexical 检索仍可命中"的测试装置。
//
// 引入动机：细分降级原因需要真实驱动 Pipeline.Search 的 vector 失败分支，
// 并可控地表达索引维度（indexDims）与维度读取失败（dimsErr）两种输入状态。
// knnErr 只作用于携带 knn 子句的查询，因此 BM25 查询正常放行，
// 可以同时验证 hybrid 模式失败后继续 lexical-only 融合的行为。
func newVectorFailureFixture(t *testing.T, ctx context.Context, indexDims int, dimsErr error) *capturingESClient {
	t.Helper()
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
	if indexDims > 0 {
		base.indexDims["knowledge_v1"] = indexDims
	}
	base.indexDimsErr = dimsErr
	return &capturingESClient{fakeESClient: base, knnErr: errFakeUnavailable}
}

// TestSearch_VectorDimensionMismatch_ClassifiedAsMismatch 覆盖线上事故的真实成因分类：
// 查询向量维度（4096）与索引 embedding 维度（1024）不一致，ES knn 请求返回 400。
//
// 引入动机：该情形此前的降级原因是笼统的 vector_search_failed，与"ES 抖动"无法区分，
// 排查时看不出根因。本测试分别驱动 semantic 与 hybrid 两条路径，锁定：
//   - 降级原因细分为 vector_dimension_mismatch；
//   - semantic 模式仍然直接返回（不继续走 RRF），无结果；
//   - hybrid 模式仍然丢弃 vectorResults 并继续 lexical-only 融合，返回 BM25 结果。
func TestSearch_VectorDimensionMismatch_ClassifiedAsMismatch(t *testing.T) {
	const (
		indexDims = 1024
		queryDims = 4096
	)

	t.Run("semantic 直接降级返回", func(t *testing.T) {
		ctx := context.Background()
		client := newVectorFailureFixture(t, ctx, indexDims, nil)
		pipe := NewPipeline(client, &fakeEmbeddingProvider{available: true, dims: queryDims}, nil)

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
		if !output.Degraded {
			t.Fatal("维度不一致时 semantic 应 degraded=true")
		}
		if output.DegradationReason != "vector_dimension_mismatch" {
			t.Errorf("降级原因应为 vector_dimension_mismatch，得到 %s", output.DegradationReason)
		}
		if len(output.Results) != 0 {
			t.Errorf("semantic 模式维度不一致不应返回结果，实际 %d 条", len(output.Results))
		}
	})

	t.Run("hybrid 继续 lexical-only 融合", func(t *testing.T) {
		ctx := context.Background()
		client := newVectorFailureFixture(t, ctx, indexDims, nil)
		pipe := NewPipeline(client, &fakeEmbeddingProvider{available: true, dims: queryDims}, nil)

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
		if !output.Degraded {
			t.Fatal("维度不一致时 hybrid 应 degraded=true")
		}
		if output.DegradationReason != "vector_dimension_mismatch" {
			t.Errorf("降级原因应为 vector_dimension_mismatch，得到 %s", output.DegradationReason)
		}
		if len(output.Results) == 0 {
			t.Fatal("hybrid 模式维度不一致时应继续返回 lexical BM25 结果")
		}
		if output.Results[0].DocumentID != "doc1" {
			t.Errorf("hybrid 回退结果应来自 BM25 命中的 doc1，实际 %s", output.Results[0].DocumentID)
		}
	})
}

// TestSearch_VectorFieldMissing_ClassifiedAsFieldMissing 覆盖"索引没有 dense_vector 的
// embedding 字段"这一分类：GetIndexDimensions 返回 (0, nil)。
//
// 引入动机：该情形与"索引维度不一致"的修复动作完全不同（前者需要重建 mapping/索引，
// 后者需要检查 embedding provider 维度配置），必须能与 vector_dimension_mismatch 区分。
func TestSearch_VectorFieldMissing_ClassifiedAsFieldMissing(t *testing.T) {
	ctx := context.Background()
	// indexDims=0：与真实客户端"索引不存在或没有 embedding 字段"语义一致
	client := newVectorFailureFixture(t, ctx, 0, nil)
	pipe := NewPipeline(client, &fakeEmbeddingProvider{available: true, dims: 4}, nil)

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
	if !output.Degraded {
		t.Fatal("embedding 字段缺失时 hybrid 应 degraded=true")
	}
	if output.DegradationReason != "vector_field_missing" {
		t.Errorf("降级原因应为 vector_field_missing，得到 %s", output.DegradationReason)
	}
	if len(output.Results) == 0 {
		t.Fatal("hybrid 模式 embedding 字段缺失时应继续返回 lexical BM25 结果")
	}
}

// TestSearch_VectorSearchFailed_GenericReasonKept 覆盖保持笼统归因的两条分支：
// 维度一致但检索仍失败（原因不明），以及维度读取自身报错。
//
// 引入动机：细分不能把"原因不明"误判成具体故障，否则会误导排查方向；
// 同时维度读取失败不能静默忽略，必须回落到 vector_search_failed。
func TestSearch_VectorSearchFailed_GenericReasonKept(t *testing.T) {
	t.Run("维度一致但检索失败", func(t *testing.T) {
		ctx := context.Background()
		client := newVectorFailureFixture(t, ctx, 4, nil) // 索引维度 == 查询向量维度 4
		pipe := NewPipeline(client, &fakeEmbeddingProvider{available: true, dims: 4}, nil)

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
		if output.DegradationReason != "vector_search_failed" {
			t.Errorf("维度一致但检索失败时应保持 vector_search_failed，得到 %s", output.DegradationReason)
		}
	})

	t.Run("维度读取失败", func(t *testing.T) {
		ctx := context.Background()
		client := newVectorFailureFixture(t, ctx, 0, errFakeUnavailable)
		pipe := NewPipeline(client, &fakeEmbeddingProvider{available: true, dims: 4}, nil)

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
		if !output.Degraded {
			t.Fatal("维度读取失败时 hybrid 应 degraded=true")
		}
		if output.DegradationReason != "vector_search_failed" {
			t.Errorf("维度读取失败时应保持 vector_search_failed，得到 %s", output.DegradationReason)
		}
		if len(output.Results) == 0 {
			t.Fatal("维度读取失败时 hybrid 仍应返回 lexical BM25 结果")
		}
	})
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

// assertSearchedIndices 断言 fake ES 依次收到的目标索引名与期望完全一致。
// 引入动机：目标索引断言必须校验真实下发给 ES 的参数序列，不能退化为字符串包含判断。
func assertSearchedIndices(t *testing.T, client *fakeESClient, want []string) {
	t.Helper()
	if len(client.searchedIndices) != len(want) {
		t.Fatalf("ES 检索调用次数应为 %d，实际 %d（索引序列: %v）",
			len(want), len(client.searchedIndices), client.searchedIndices)
	}
	for i, idx := range want {
		if client.searchedIndices[i] != idx {
			t.Errorf("第 %d 次 ES 检索目标索引应为 %q，实际 %q", i+1, idx, client.searchedIndices[i])
		}
	}
}

// TestSearch_ExplicitIndexName_OverridesProfileIndex 验证显式 IndexName（alias）优先于 Profile.ESIndexName。
//
// 引入动机：design/01-SEARCH.md §Index Version 要求普通搜索通过 alias knowledge_current 检索，
// 以保证索引切换（alias 原子切换）后搜索自动指向新索引。本测试构造 profile 的
// ESIndexName="knowledge_v1"（与分析索引不一致的典型场景），文档只写入 alias 指向的索引，
// 断言管线真实下发给 ES 的索引是 alias 且能取回结果。
func TestSearch_ExplicitIndexName_OverridesProfileIndex(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()
	_ = client.CreateIndex(ctx, es.AliasName, nil)
	_ = client.BulkIndex(ctx, es.AliasName, []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "alias.md",
			"title":        "Alias",
			"content":      "alias content",
			"status":       "active",
			"is_special":   false,
		}},
	})

	pipe := NewPipeline(client, nil, nil)
	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(), // ESIndexName = knowledge_v1
		IndexName:   es.AliasName,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	assertSearchedIndices(t, client, []string{es.AliasName})
	if len(output.Results) == 0 {
		t.Fatal("应以 alias 索引为数据源取回结果")
	}
	if output.Results[0].DocumentID != "doc1" {
		t.Errorf("结果应来自 alias 索引中的文档 doc1，实际 %s", output.Results[0].DocumentID)
	}
	if client.searchedIndices[0] == "knowledge_v1" {
		t.Error("显式 IndexName 生效时不应再打 profile 的具体索引 knowledge_v1")
	}
}

// TestSearch_EvalPath_EmptyIndexName_UsesProfileIndex 锁定评测路径不受 alias 改造影响。
//
// 引入动机：evaluate_profile / optimize_profile（job/eval_handler.go）用 SearchInput 搜索
// 候选 profile 自己的索引，且不设置 IndexName。该索引通常不是 alias 当前指向的索引，
// 因此本测试以与评测完全相同的输入组装方式（IndexName 留空 + Profile.ESIndexName 具体索引）
// 断言管线仍然真实打具体索引，防止后续有人把 alias 强制写进管线内部。
func TestSearch_EvalPath_EmptyIndexName_UsesProfileIndex(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)
	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id":  "doc1",
			"workspace_id": "ws-1",
			"path":         "candidate.md",
			"title":        "Candidate",
			"content":      "candidate content",
			"status":       "active",
			"is_special":   false,
		}},
	})

	pipe := NewPipeline(client, nil, nil)
	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "", // 评测不限定 workspace，与 eval_handler.go 一致
		Query:       "test",
		Profile:     testProfile(), // 候选 profile 的具体索引 knowledge_v1
		Limit:       10,
		Mode:        "hybrid",
		// IndexName 留空：评测路径必须落到 Profile.ESIndexName
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	assertSearchedIndices(t, client, []string{"knowledge_v1"})
	if len(output.Results) == 0 {
		t.Fatal("评测路径应从候选 profile 的具体索引取回结果")
	}
	if output.Results[0].DocumentID != "doc1" {
		t.Errorf("结果应来自候选索引中的文档 doc1，实际 %s", output.Results[0].DocumentID)
	}
}

// TestSearch_EmptyIndexNameAndProfileIndex_FallsBackToAlias 验证保留既有回退语义。
//
// 引入动机：管线原有语义为 indexName 为空时回退 es.AliasName，本次改造必须保留该行为，
// 避免 profile 未设置索引名时目标索引为空导致 ES 请求非法。
func TestSearch_EmptyIndexNameAndProfileIndex_FallsBackToAlias(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()

	pipe := NewPipeline(client, nil, nil)
	profileConfig := testProfile()
	profileConfig.ESIndexName = ""

	_, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     profileConfig,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	assertSearchedIndices(t, client, []string{es.AliasName})
}

// scoringReranker 是测试用的 reranker，按候选文本内容给分。
// 引入动机：reranker 路径下 Score 直接来自 reranker 返回值，需要按文档可控地
// 制造分数差异以驱动归一化裁剪；候选的 DocumentID 不在 RerankerCandidate 中暴露，
// 只能依据 Text（chunk.Content）内容匹配来区分文档给分。
type scoringReranker struct {
	// scoreByContentSubstr 按"候选 Text 包含的子串"→ 分数 映射给分。
	scoreByContentSubstr map[string]float64
	// defaultScore 是未匹配任何子串时的默认分。
	defaultScore float64
}

func (s *scoringReranker) Rerank(ctx context.Context, query string, candidates []types.RerankerCandidate) ([]types.RerankerResult, error) {
	results := make([]types.RerankerResult, len(candidates))
	for i, c := range candidates {
		score := s.defaultScore
		for substr, sc := range s.scoreByContentSubstr {
			if strings.Contains(c.Text, substr) {
				score = sc
				break
			}
		}
		results[i] = types.RerankerResult{Index: i, Score: score}
	}
	// reranker 通常返回按分数降序的结果；按分数降序排列模拟真实行为。
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	return results, nil
}

func (s *scoringReranker) Available(ctx context.Context) bool { return true }

// ===== 搜索改造（highlight snippet / 文档级聚合 / score 归一化裁剪）测试 =====

// TestSearchBM25_QueryContainsHighlight 验证 BM25 查询体携带 highlight 子句。
//
// 引入动机：buildSnippet 依赖 ES 返回的高亮 fragment 生成命中点 snippet，
// 必须确认管线真实地把 highlight 请求下发给 ES，否则 snippet 永远走不到高亮路径。
// 断言结构字段值而非源码字符串，保证验证的是真实下发的请求体。
func TestSearchBM25_QueryContainsHighlight(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)
	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id": "doc1",
			"content":     "content",
			"status":      "active",
		}},
	})

	pipe := NewPipeline(client, nil, nil)
	_, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
		Mode:        "lexical",
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	highlight, ok := client.lastQuery["highlight"].(map[string]interface{})
	if !ok {
		t.Fatalf("BM25 查询体应包含 highlight 子句，实际 query: %#v", client.lastQuery)
	}
	fields, ok := highlight["fields"].(map[string]interface{})
	if !ok {
		t.Fatalf("highlight 应包含 fields，实际: %#v", highlight)
	}
	// content fragment 应取 2 个、order=score，以覆盖多个命中点。
	content, ok := fields["content"].(map[string]interface{})
	if !ok {
		t.Fatalf("highlight.fields 应包含 content，实际: %#v", fields)
	}
	if content["number_of_fragments"] != 2 {
		t.Errorf("content.number_of_fragments 应为 2，得到 %#v", content["number_of_fragments"])
	}
	if content["order"] != "score" {
		t.Errorf("content.order 应为 score，得到 %#v", content["order"])
	}
	// pre_tags/post_tags 必须是 <em></em>，buildSnippet 依赖该标记识别命中点。
	preTags, ok := highlight["pre_tags"].([]string)
	if !ok || len(preTags) == 0 || preTags[0] != "<em>" {
		t.Errorf("highlight.pre_tags 应为 [<em>]，得到 %#v", highlight["pre_tags"])
	}
	postTags, ok := highlight["post_tags"].([]string)
	if !ok || len(postTags) == 0 || postTags[0] != "</em>" {
		t.Errorf("highlight.post_tags 应为 [</em>]，得到 %#v", highlight["post_tags"])
	}
	if highlight["require_field_match"] != false {
		t.Errorf("highlight.require_field_match 应为 false，得到 %#v", highlight["require_field_match"])
	}
}

// TestBuildSnippet_ContentHighlightPreferred 验证 content 高亮优先且保留 <em>。
//
// 引入动机：snippet 应定位到真实命中点而非开头套话；content 高亮是正文命中，
// 优先级最高，且保留 <em> 标记让模型/前端能识别命中词。
func TestBuildSnippet_ContentHighlightPreferred(t *testing.T) {
	chunk := types.Chunk{Content: "这是 chunk 开头的内容，与命中点无关。"}
	highlights := map[string][]string{
		"content": {"这是包含 <em>关键词</em> 的正文片段。"},
		"heading": {"标题 <em>关键词</em>"},
	}
	got := buildSnippet(chunk, highlights)
	if got != "这是包含 <em>关键词</em> 的正文片段。" {
		t.Errorf("应优先返回 content 高亮 fragment，得到 %q", got)
	}
}

// TestBuildSnippet_MultipleContentFragmentsJoined 验证多 content fragment 用 " … " 连接。
//
// 引入动机：content 配置了 number_of_fragments=2，多命中点应合并呈现，
// 让模型看到多个命中上下文；连接符 " … " 表达片段间存在省略。
func TestBuildSnippet_MultipleContentFragmentsJoined(t *testing.T) {
	chunk := types.Chunk{Content: "正文"}
	highlights := map[string][]string{
		"content": {"片段一 <em>词</em>", "片段二 <em>词</em>"},
	}
	got := buildSnippet(chunk, highlights)
	want := "片段一 <em>词</em> … 片段二 <em>词</em>"
	if got != want {
		t.Errorf("多 fragment 应用 … 连接，得到 %q，期望 %q", got, want)
	}
}

// TestBuildSnippet_FallbackToHeadingThenTitle 验证 content 缺失时降级到 heading/title。
//
// 引入动机：命中可能落在 heading 或 title 而非正文，此时 snippet 仍应反映命中处；
// heading 比 title 更具体（指向小节），优先于 title。
func TestBuildSnippet_FallbackToHeadingThenTitle(t *testing.T) {
	chunk := types.Chunk{Content: "正文内容"}

	// 仅 heading 高亮
	got := buildSnippet(chunk, map[string][]string{"heading": {"小节 <em>词</em>"}})
	if got != "小节 <em>词</em>" {
		t.Errorf("content 缺失时应降级到 heading，得到 %q", got)
	}

	// 仅 title 高亮
	got = buildSnippet(chunk, map[string][]string{"title": {"标题 <em>词</em>"}})
	if got != "标题 <em>词</em>" {
		t.Errorf("content/heading 缺失时应降级到 title，得到 %q", got)
	}
}

// TestBuildSnippet_NoHighlight_FallsBackToContent 验证无高亮时回退 chunk 开头截断。
//
// 引入动机：vector-only 命中没有高亮片段，snippet 仍需给出可读内容，
// 回退到 content 开头截断是合理的兜底，且必须按 rune 截断不乱码。
func TestBuildSnippet_NoHighlight_FallsBackToContent(t *testing.T) {
	// 构造超过 200 rune 的中文 content，验证按 rune 截断而非按字节乱码。
	long := ""
	for i := 0; i < 250; i++ {
		long += "中"
	}
	chunk := types.Chunk{Content: long}
	got := buildSnippet(chunk, nil)
	runes := []rune(got)
	// 200 个 '中' + "..."(3 个 ASCII rune) = 203 rune
	if len(runes) != contentFallbackRunes+3 {
		t.Errorf("无高亮时应回退 content 截断到 %d rune+省略号，得到 %d rune", contentFallbackRunes, len(runes))
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("截断后应以 ... 结尾，得到末尾 %q", got[len(got)-3:])
	}
}

// TestBuildSnippet_TruncatesByRune 验证高亮片段总长超限按 rune 截断。
//
// 引入动机：多 fragment 连接后可能超过 snippetMaxRunes，
// 必须按 rune 截断保证中文输出合法且长度受控。
func TestBuildSnippet_TruncatesByRune(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "汉"
	}
	chunk := types.Chunk{Content: "x"}
	highlights := map[string][]string{"content": {long}}
	got := buildSnippet(chunk, highlights)
	if n := len([]rune(got)); n != snippetMaxRunes+3 {
		t.Errorf("超长 snippet 应截断到 %d rune+省略号，得到 %d rune", snippetMaxRunes, n)
	}
}

// TestGroupByDocument_SingleResultPerDoc 验证每个文档只产出一条主结果。
//
// 引入动机：文档级聚合要求"一个文档一条"，主结果是组内最高分 chunk，
// MatchedChunks 记录命中总数，其余命中段折叠进 OtherRanges。
func TestGroupByDocument_SingleResultPerDoc(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1", Score: 0.9, StartLine: 1, EndLine: 10, SectionPath: []string{"A"}},
		{DocumentID: "d1", Score: 0.7, StartLine: 20, EndLine: 30, SectionPath: []string{"B"}},
		{DocumentID: "d1", Score: 0.5, StartLine: 40, EndLine: 50, SectionPath: []string{"C"}},
		{DocumentID: "d2", Score: 0.8, StartLine: 1, EndLine: 10, SectionPath: []string{"X"}},
	}

	grouped := groupByDocument(results)
	if len(grouped) != 2 {
		t.Fatalf("聚合后应有 2 条文档级结果，得到 %d", len(grouped))
	}

	// d1 主结果应是最高分 chunk（0.9, lines 1-10）
	var d1 types.SearchResult
	for _, r := range grouped {
		if r.DocumentID == "d1" {
			d1 = r
		}
	}
	if d1.Score != 0.9 {
		t.Errorf("d1 主 chunk 应为最高分 0.9，得到 %v", d1.Score)
	}
	if d1.MatchedChunks != 3 {
		t.Errorf("d1 MatchedChunks 应为 3，得到 %d", d1.MatchedChunks)
	}
	if len(d1.OtherRanges) != 2 {
		t.Fatalf("d1 应有 2 条 OtherRanges，得到 %d", len(d1.OtherRanges))
	}
	// OtherRanges 按组内 score 降序：先 0.7(lines 20-30,B)，再 0.5(lines 40-50,C)。
	if d1.OtherRanges[0].StartLine != 20 || d1.OtherRanges[0].EndLine != 30 || d1.OtherRanges[0].Heading != "B" {
		t.Errorf("d1.OtherRanges[0] 应为 {20,30,B}，得到 %+v", d1.OtherRanges[0])
	}
	if d1.OtherRanges[1].StartLine != 40 || d1.OtherRanges[1].Heading != "C" {
		t.Errorf("d1.OtherRanges[1] 应为 {40,50,C}，得到 %+v", d1.OtherRanges[1])
	}
}

// TestGroupByDocument_OtherRangesCappedAt3 验证 OtherRanges 上限为 3。
//
// 引入动机：OtherRanges 是可导航提示而非完整列表，限制 3 条防止
// 单文档命中过多时响应体积膨胀。
func TestGroupByDocument_OtherRangesCappedAt3(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d1", Score: 0.95, StartLine: 1, EndLine: 5},
		{DocumentID: "d1", Score: 0.9, StartLine: 10, EndLine: 15},
		{DocumentID: "d1", Score: 0.8, StartLine: 20, EndLine: 25},
		{DocumentID: "d1", Score: 0.7, StartLine: 30, EndLine: 35},
		{DocumentID: "d1", Score: 0.6, StartLine: 40, EndLine: 45},
	}
	grouped := groupByDocument(results)
	if len(grouped) != 1 {
		t.Fatalf("应聚合成 1 条结果，得到 %d", len(grouped))
	}
	if grouped[0].MatchedChunks != 5 {
		t.Errorf("MatchedChunks 应为 5（含主 chunk），得到 %d", grouped[0].MatchedChunks)
	}
	if len(grouped[0].OtherRanges) != 3 {
		t.Errorf("OtherRanges 应截断到 3 条，得到 %d", len(grouped[0].OtherRanges))
	}
}

// TestGroupByDocument_InterDocOrderingAndRank 验证组间按主 chunk 分数降序并重排 Rank。
//
// 引入动机：聚合后结果顺序必须由各文档主 chunk 的相关性决定，
// Rank 需连续，保证下游分页/展示一致。
func TestGroupByDocument_InterDocOrderingAndRank(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "d-low", Score: 0.4, StartLine: 1, EndLine: 5},
		{DocumentID: "d-high", Score: 0.95, StartLine: 1, EndLine: 5},
		{DocumentID: "d-high", Score: 0.5, StartLine: 10, EndLine: 15},
		{DocumentID: "d-mid", Score: 0.7, StartLine: 1, EndLine: 5},
	}
	grouped := groupByDocument(results)
	if len(grouped) != 3 {
		t.Fatalf("应有 3 个文档，得到 %d", len(grouped))
	}
	wantOrder := []string{"d-high", "d-mid", "d-low"}
	for i, id := range wantOrder {
		if grouped[i].DocumentID != id {
			t.Errorf("第 %d 位应为 %s，得到 %s", i, id, grouped[i].DocumentID)
		}
		if grouped[i].Rank != i+1 {
			t.Errorf("第 %d 位 Rank 应为 %d，得到 %d", i, i+1, grouped[i].Rank)
		}
	}
}

// TestNormalizeAndTrimByScore_Normalizes 验证 score 归一化到 0~1。
//
// 引入动机：RRF 分数是不可读小数，归一化后 top1=1.0、其余按比例缩放，
// 模型可直接理解相对相关性。
func TestNormalizeAndTrimByScore_Normalizes(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "a", Score: 0.04},
		{DocumentID: "b", Score: 0.02},
		{DocumentID: "c", Score: 0.01},
	}
	kept, truncated := normalizeAndTrimByScore(results, false)
	// top1=0.04，归一化：a=1.0, b=0.5, c=0.25，均 >= 0.15，不裁剪。
	if truncated != 0 {
		t.Errorf("不应有裁剪，得到 %d", truncated)
	}
	if len(kept) != 3 {
		t.Fatalf("应保留 3 条，得到 %d", len(kept))
	}
	if kept[0].Score != 1.0 {
		t.Errorf("top1 归一化应为 1.0，得到 %v", kept[0].Score)
	}
	if kept[1].Score != 0.5 {
		t.Errorf("b 归一化应为 0.5，得到 %v", kept[1].Score)
	}
	if kept[2].Score != 0.25 {
		t.Errorf("c 归一化应为 0.25，得到 %v", kept[2].Score)
	}
}

// TestNormalizeAndTrimByScore_TrimsTail 验证低分尾部被裁剪并计数。
//
// 引入动机：归一化后 < minNormalizedScore 的结果是与主结果脱节的噪声，
// 应被丢弃且数量可观测（返回 truncated 计数供响应透传）。
func TestNormalizeAndTrimByScore_TrimsTail(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "a", Score: 0.10},  // 归一化 1.0
		{DocumentID: "b", Score: 0.05},  // 归一化 0.5
		{DocumentID: "c", Score: 0.01},  // 归一化 0.1 < 0.15 → 裁
		{DocumentID: "d", Score: 0.005}, // 归一化 0.05 < 0.15 → 裁
	}
	kept, truncated := normalizeAndTrimByScore(results, false)
	if truncated != 2 {
		t.Errorf("应裁剪 2 条，得到 %d", truncated)
	}
	if len(kept) != 2 {
		t.Fatalf("应保留 2 条，得到 %d", len(kept))
	}
	if kept[0].DocumentID != "a" || kept[1].DocumentID != "b" {
		t.Errorf("保留的应为 a,b，得到 %v", []string{kept[0].DocumentID, kept[1].DocumentID})
	}
	// 裁剪后 Rank 连续。
	for i := range kept {
		if kept[i].Rank != i+1 {
			t.Errorf("kept[%d].Rank 应为 %d，得到 %d", i, i+1, kept[i].Rank)
		}
	}
}

// TestNormalizeAndTrimByScore_Top1Zero_RRF 验证 RRF 全零分数时归一化为 0 且不裁剪。
//
// 引入动机：top1==0 时除以 0 会产生 NaN/Inf，必须显式置 0；
// 全零分数不表示"低相关"而是"无区分度"，不应触发裁剪。
func TestNormalizeAndTrimByScore_Top1Zero_RRF(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "a", Score: 0},
		{DocumentID: "b", Score: 0},
	}
	kept, truncated := normalizeAndTrimByScore(results, false)
	if truncated != 0 {
		t.Errorf("全零分数不应裁剪，得到 %d", truncated)
	}
	if len(kept) != 2 {
		t.Fatalf("应保留全部 2 条，得到 %d", len(kept))
	}
	for _, r := range kept {
		if r.Score != 0 {
			t.Errorf("top1==0 时所有 score 应置 0，得到 %v", r.Score)
		}
	}
}

// TestNormalizeAndTrimByScore_RerankerZeroSkipsTrim 验证 reranker 全零/负分跳过裁剪。
//
// 引入动机：reranker 若返回全 0 或负分属于异常输出，此时归一化会把所有
// 结果置 0 再被阈值全裁掉，造成结果全灭。必须识别该情形跳过裁剪保护结果。
func TestNormalizeAndTrimByScore_RerankerZeroSkipsTrim(t *testing.T) {
	results := []types.SearchResult{
		{DocumentID: "a", Score: 0},
		{DocumentID: "b", Score: -0.5},
	}
	kept, truncated := normalizeAndTrimByScore(results, true)
	if truncated != 0 {
		t.Errorf("reranker 异常分数不应裁剪，得到 %d", truncated)
	}
	if len(kept) != 2 {
		t.Fatalf("reranker 异常时应原样保留全部结果，得到 %d", len(kept))
	}
}

// TestSearch_EndToEnd_DocAggregationAndSnippet 端到端验证管线：
// BM25 高亮 → snippet 命中点 → 文档级聚合 → 归一化。
//
// 引入动机：单元测试分别覆盖了各环节，本测试驱动完整 Search 验证它们
// 在真实管线里串联生效：同文档多 chunk 折叠为一条、snippet 来自高亮、
// score 归一化到 0~1。
func TestSearch_EndToEnd_DocAggregationAndSnippet(t *testing.T) {
	client := newFakeESClient()
	ctx := context.Background()
	_ = client.CreateIndex(ctx, "knowledge_v1", nil)

	// 同一文档两个 chunk + 另一文档一个 chunk。fake Search 给每个 hit Score=1.0。
	// 注意必须设置不同的 chunk_index：rrf.chunkKey 用 document_id+path+chunk_index
	// 作为候选唯一键，缺省同为 0 会被 RRF 合并成同一候选，无法覆盖聚合路径。
	_ = client.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "d1_c0", Body: map[string]interface{}{
			"document_id": "d1", "path": "a.md", "title": "DocA",
			"content": "开头套话", "status": "active", "is_special": false,
			"start_line": 1, "end_line": 10, "chunk_index": 0, "section_path": []interface{}{"S1"},
		}},
		{ID: "d1_c1", Body: map[string]interface{}{
			"document_id": "d1", "path": "a.md", "title": "DocA",
			"content": "其他段", "status": "active", "is_special": false,
			"start_line": 20, "end_line": 30, "chunk_index": 1, "section_path": []interface{}{"S2"},
		}},
		{ID: "d2_c0", Body: map[string]interface{}{
			"document_id": "d2", "path": "b.md", "title": "DocB",
			"content": "另一个文档", "status": "active", "is_special": false,
			"start_line": 1, "end_line": 10, "chunk_index": 0, "section_path": []interface{}{"T1"},
		}},
	})
	// 给 d1_c0 配高亮，验证 snippet 走高亮路径。
	client.highlights = map[string]map[string][]string{
		"d1_c0": {"content": {"这是 <em>test</em> 命中的正文片段"}},
	}

	// 使用 reranker 路径制造可裁剪的分数差异：RRF 分数被压扁（rank 只差 1/(60+rank)），
	// 无法触发 0.15 阈值；reranker 直接给 Score，给 d2 极低分使其归一化后被裁。
	// candidates 顺序即 RRF 排序后的索引；fake Rerank 按 Index 引用候选，
	// 但我们无法预知 map 遍历决定的候选顺序，因此给"内容含 d2 的候选"打低分
	// 不可行——改为给所有候选高分，只把排最后的 d2 候选压低。
	// 由于候选顺序不确定，改用 reranker results 显式指定每个候选 index 的分数：
	// 先跑一遍拿到候选顺序太脆弱。更稳妥：用 fakeRerankerProvider 默认实现按
	// Index 递减给分不可靠。这里直接构造一个按候选内容返回分数的 reranker。
	// d1 两 chunk 的 content 是"开头套话"/"其他段"，d2 是"另一个文档"。
	// 给 d1 高分、d2 极低分（归一化 0.05/0.9≈0.056 < 0.15 → 裁剪）。
	rrProvider := &scoringReranker{scoreByContentSubstr: map[string]float64{
		"开头套话":   0.9,
		"其他段":    0.6,
		"另一个文档": 0.05,
	}}

	pipe := NewPipeline(client, nil, rrProvider)
	output, err := pipe.Search(ctx, SearchInput{
		WorkspaceID: "ws-1",
		Query:       "test",
		Profile:     testProfile(),
		Limit:       10,
		Mode:        "lexical",
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}

	// 文档级聚合：d1 两 chunk 折叠为一条；d2 归一化 score=0.1<0.15 被裁剪 → 仅 1 条结果。
	if len(output.Results) != 1 {
		t.Fatalf("应聚合并裁剪为 1 条文档级结果，得到 %d: %+v", len(output.Results), output.Results)
	}
	if output.Total != 1 {
		t.Errorf("Total 应为裁剪后文档数 1，得到 %d", output.Total)
	}
	if output.TruncatedByScore != 1 {
		t.Errorf("应裁掉 1 条低分结果，TruncatedByScore=%d", output.TruncatedByScore)
	}

	// 唯一结果即 d1，验证 matched_chunks / other_ranges / snippet / score。
	d1 := output.Results[0]
	if d1.DocumentID != "d1" {
		t.Fatalf("唯一结果应为 d1，得到 %s", d1.DocumentID)
	}
	if d1.MatchedChunks != 2 {
		t.Errorf("d1 MatchedChunks 应为 2，得到 %d", d1.MatchedChunks)
	}
	if len(d1.OtherRanges) != 1 {
		t.Fatalf("d1 应有 1 条 OtherRanges，得到 %d", len(d1.OtherRanges))
	}
	if d1.OtherRanges[0].StartLine != 20 || d1.OtherRanges[0].EndLine != 30 || d1.OtherRanges[0].Heading != "S2" {
		t.Errorf("d1.OtherRanges[0] 应为 {20,30,S2}，得到 %+v", d1.OtherRanges[0])
	}
	// snippet 应来自高亮片段而非 content 开头"开头套话"。
	if d1.Snippet != "这是 <em>test</em> 命中的正文片段" {
		t.Errorf("d1 snippet 应来自高亮片段，得到 %q", d1.Snippet)
	}
	// score 归一化：d1_c0 是 top1，归一化后应为 1.0。
	if d1.Score != 1.0 {
		t.Errorf("归一化后 top score 应为 1.0，得到 %v", d1.Score)
	}
}
