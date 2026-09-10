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
