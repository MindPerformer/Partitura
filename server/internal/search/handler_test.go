// handler_test.go 测试搜索 HTTP handler 的请求边界校验行为。
//
// 引入动机：design/00-MASTER.md 禁止 silent overwrite，
// design/04-WEB-API.md §Search 要求搜索 API 接受 query/mode/limit。
// 原实现对 limit 越界静默重置为 10，offset 负数静默重置为 0，
// 违反了"禁止静默伪成功"原则。修复后越界返回 400 并记录审计日志。
package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"partitura/server/internal/auth"
	"partitura/server/internal/es"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/job"
	"partitura/server/internal/profile"
	"partitura/server/internal/search/pipeline"
	types "partitura/server/internal/search/types"
	"partitura/server/internal/workspace"
)

// mockProfileRepo 用于测试的 profile repository mock。
type mockProfileRepo struct {
	activeProfile *profile.ProfileRecord
	err           error
}

func (m *mockProfileRepo) CreateProfile(ctx context.Context, p *profile.CreateProfileInput) (*profile.ProfileRecord, error) {
	return nil, nil
}
func (m *mockProfileRepo) GetProfileByID(ctx context.Context, id string) (*profile.ProfileRecord, error) {
	return nil, nil
}
func (m *mockProfileRepo) ListProfiles(ctx context.Context, statusFilter string, limit, offset int) (*profile.ListProfilesResult, error) {
	return nil, nil
}
func (m *mockProfileRepo) GetActiveProfile(ctx context.Context) (*profile.ProfileRecord, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.activeProfile, nil
}
func (m *mockProfileRepo) ActivateProfile(ctx context.Context, id string) error   { return nil }
func (m *mockProfileRepo) DeactivateProfile(ctx context.Context, id string) error { return nil }

// ArchiveProfile 补齐 profile.Repository 接口；搜索 handler 不涉及归档，返回 nil。
func (m *mockProfileRepo) ArchiveProfile(ctx context.Context, id string) error { return nil }
func (m *mockProfileRepo) UpdateProfileESIndex(ctx context.Context, id, indexName string) error {
	return nil
}
func (m *mockProfileRepo) CreateCandidate(ctx context.Context, c *profile.CandidateRecord) error {
	return nil
}
func (m *mockProfileRepo) GetCandidateByID(ctx context.Context, id string) (*profile.CandidateRecord, error) {
	return nil, nil
}
func (m *mockProfileRepo) ListCandidates(ctx context.Context, statusFilter string, limit, offset int) (*profile.ListCandidatesResult, error) {
	return nil, nil
}
func (m *mockProfileRepo) UpdateCandidateStatus(ctx context.Context, id, status string, gatePassed bool, gateDetails json.RawMessage, evaluationResult json.RawMessage) error {
	return nil
}
func (m *mockProfileRepo) ConfirmCandidate(ctx context.Context, id, adminUserID string) error {
	return nil
}

// mockMetricsRepo 用于测试的 metrics repository mock。
type mockMetricsRepo struct{}

func (m *mockMetricsRepo) RecordMetrics(ctx context.Context, params job.RecordMetricsParams) error {
	return nil
}
func (m *mockMetricsRepo) RecordFeedback(ctx context.Context, params job.RecordFeedbackParams) error {
	return nil
}

// newTestHandler 创建用于测试的搜索 handler，使用 nil pipeline（搜索会降级）。
func newTestHandler(profileRepo profile.Repository) *Handler {
	pipe := pipeline.NewPipeline(nil, nil, nil)
	return NewHandler(pipe, profileRepo, &mockMetricsRepo{})
}

// fakeSearchESClient 是搜索 handler 测试用的可控 ES 客户端，按调用顺序记录检索目标索引名。
//
// 引入动机：design/01-SEARCH.md §Index Version 要求普通搜索通过 alias
// knowledge_current 检索；必须真实观察 handler → pipeline → ES 下发的索引名，
// 而不是对源码做字符串包含式断言。
// 本测试驱动到的路径只有 Ping 与 Search，其余 es.Client 方法按接口契约返回零值，
// 使 fake 与真实客户端在未被覆盖的方法上不产生副作用。
type fakeSearchESClient struct {
	// pingErr 非 nil 时 Ping 返回该错误，用于验证 ES 不可用时的降级路径。
	pingErr error
	// searchedIndices 按调用顺序记录每次 Search 收到的目标索引名。
	searchedIndices []string
}

func (c *fakeSearchESClient) Ping(ctx context.Context) error { return c.pingErr }

func (c *fakeSearchESClient) CreateIndex(ctx context.Context, indexName string, mapping map[string]interface{}) error {
	return nil
}

func (c *fakeSearchESClient) DeleteIndex(ctx context.Context, indexName string) error { return nil }

func (c *fakeSearchESClient) IndexExists(ctx context.Context, indexName string) (bool, error) {
	return false, nil
}

func (c *fakeSearchESClient) GetIndexDimensions(ctx context.Context, indexName string) (int, error) {
	return 0, nil
}

func (c *fakeSearchESClient) UpdateAlias(ctx context.Context, actions []es.AliasAction) error {
	return nil
}

func (c *fakeSearchESClient) GetAliasIndex(ctx context.Context, alias string) (string, error) {
	return "", nil
}

func (c *fakeSearchESClient) BulkIndex(ctx context.Context, indexName string, docs []es.IndexDoc) error {
	return nil
}

func (c *fakeSearchESClient) DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error {
	return nil
}

func (c *fakeSearchESClient) Search(ctx context.Context, indexName string, query map[string]interface{}) (*es.SearchResponse, error) {
	c.searchedIndices = append(c.searchedIndices, indexName)
	return &es.SearchResponse{}, nil
}

func (c *fakeSearchESClient) Count(ctx context.Context, indexName string, query map[string]interface{}) (int64, error) {
	return 0, nil
}

func (c *fakeSearchESClient) GetDocument(ctx context.Context, indexName, docID string) (map[string]interface{}, error) {
	return nil, nil
}

func (c *fakeSearchESClient) Refresh(ctx context.Context, indexName string) error { return nil }

func (c *fakeSearchESClient) ListIndices(ctx context.Context, pattern string) ([]string, error) {
	return nil, nil
}

// newTestHandlerWithES 创建使用指定 ES 客户端的搜索 handler。
//
// 引入动机：需要让 pipeline 真实走到 esClient.Search，以观察普通搜索实际打的目标索引。
func newTestHandlerWithES(profileRepo profile.Repository, esClient es.Client) *Handler {
	pipe := pipeline.NewPipeline(esClient, nil, nil)
	return NewHandler(pipe, profileRepo, &mockMetricsRepo{})
}

// makeSearchRequest 构建带 workspace context 的搜索 HTTP 请求。
func makeSearchRequest(body string) *http.Request {
	r := httptest.NewRequest("POST", "/api/workspaces/ws-1/search", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	wsc := &workspace.WorkspaceContext{WorkspaceID: "ws-1", MemberRole: "viewer"}
	r = r.WithContext(workspace.WithWorkspaceContext(r.Context(), wsc))
	return r
}

// TestSearch_LimitNegative_Returns400 验证 limit 为负数时返回 400 而非静默重置。
func TestSearch_LimitNegative_Returns400(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v1",
		},
	})

	body := `{"query":"test","limit":-1}`
	r := makeSearchRequest(body)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("limit=-1 应返回 400，实际 %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !strings.Contains(resp["error"], "limit") {
		t.Errorf("错误信息应包含 limit，实际: %s", resp["error"])
	}
}

// TestSearch_LimitExceedsMax_Returns400 验证 limit 超过 100 时返回 400。
func TestSearch_LimitExceedsMax_Returns400(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v1",
		},
	})

	body := `{"query":"test","limit":101}`
	r := makeSearchRequest(body)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("limit=101 应返回 400，实际 %d", w.Code)
	}
}

// TestSearch_LimitZero_DefaultsTo10 验证 limit=0（未设置）使用默认值 10。
func TestSearch_LimitZero_DefaultsTo10(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v1",
		},
	})

	body := `{"query":"test","limit":0}`
	r := makeSearchRequest(body)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	// limit=0 应使用默认值 10，不应返回 400
	if w.Code == http.StatusBadRequest {
		t.Errorf("limit=0 应使用默认值 10，不应返回 400")
	}

	var resp types.SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 10 {
		t.Errorf("limit=0 应默认为 10，实际 %d", resp.Limit)
	}
}

// TestSearch_LimitOmitted_DefaultsTo10 验证省略 limit 时使用默认值 10。
func TestSearch_LimitOmitted_DefaultsTo10(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v1",
		},
	})

	body := `{"query":"test"}`
	r := makeSearchRequest(body)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code == http.StatusBadRequest {
		t.Errorf("省略 limit 应使用默认值 10，不应返回 400")
	}

	var resp types.SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 10 {
		t.Errorf("省略 limit 应默认为 10，实际 %d", resp.Limit)
	}
}

// TestSearch_OffsetNegative_Returns400 验证 offset 为负数时返回 400 而非静默重置。
func TestSearch_OffsetNegative_Returns400(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v1",
		},
	})

	body := `{"query":"test","offset":-1}`
	r := makeSearchRequest(body)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("offset=-1 应返回 400，实际 %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !strings.Contains(resp["error"], "offset") {
		t.Errorf("错误信息应包含 offset，实际: %s", resp["error"])
	}
}

// TestSearch_LimitAtBoundary_Passes 验证 limit=100（边界值）通过校验。
func TestSearch_LimitAtBoundary_Passes(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v1",
		},
	})

	body := `{"query":"test","limit":100}`
	r := makeSearchRequest(body)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code == http.StatusBadRequest {
		t.Errorf("limit=100 是合法边界值，不应返回 400")
	}

	var resp types.SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if resp.Limit != 100 {
		t.Errorf("limit=100 应保留，实际 %d", resp.Limit)
	}
}

// TestSearch_NoWorkspaceContext_Returns500 验证缺少 workspace 上下文时返回 500。
func TestSearch_NoWorkspaceContext_Returns500(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{})

	body := `{"query":"test"}`
	r := httptest.NewRequest("POST", "/api/workspaces/ws-1/search", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("缺少 workspace 上下文应返回 500，实际 %d", w.Code)
	}
}

// TestSearch_EmptyQuery_Returns400 验证空 query 返回 400。
func TestSearch_EmptyQuery_Returns400(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{})

	body := `{"query":""}`
	r := makeSearchRequest(body)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("空 query 应返回 400，实际 %d", w.Code)
	}
}

func TestSearch_WhitespaceQuery_Returns400(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{})
	w := httptest.NewRecorder()
	handler.Search(w, makeSearchRequest(`{"query":"  \t\n"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空白 query 应返回 400，实际 %d", w.Code)
	}
}

func TestSearch_OverlongQuery_Returns400(t *testing.T) {
	handler := newTestHandler(&mockProfileRepo{})
	w := httptest.NewRecorder()
	query := strings.Repeat("中", 4097)
	handler.Search(w, makeSearchRequest(`{"query":"`+query+`"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("超长 query 应返回 400，实际 %d", w.Code)
	}
}

// TestSearch_TargetsAliasIndex 验证普通搜索真实下发给 ES 的目标索引是 alias knowledge_current。
//
// 引入动机：修复线上故障——handler 原先把 profile.ESIndexName（如 knowledge_v1）交给管线，
// 一旦该字段与实际索引不一致，ES 返回 400/404 导致搜索降级为 vector_search_failed。
// design/01-SEARCH.md §Index Version 要求索引 versioned 且通过 alias 原子切换，
// 因此普通搜索必须打 alias，与 profile 记录的具体索引名解耦。
func TestSearch_TargetsAliasIndex(t *testing.T) {
	esClient := &fakeSearchESClient{}
	handler := newTestHandlerWithES(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v1",
		},
	}, esClient)

	r := makeSearchRequest(`{"query":"test"}`)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("搜索应返回 200，实际 %d，body=%s", w.Code, w.Body.String())
	}
	// 先锁定 alias 常量值，避免常量被改动后测试随之失效（design/01-SEARCH.md §Index Version）。
	if es.AliasName != "knowledge_current" {
		t.Fatalf("alias 常量应为 knowledge_current，实际 %s", es.AliasName)
	}
	if len(esClient.searchedIndices) == 0 {
		t.Fatal("handler 未向 ES 发起任何检索请求")
	}
	for i, idx := range esClient.searchedIndices {
		if idx != es.AliasName {
			t.Errorf("第 %d 次 ES 检索的目标索引应为 alias %s，实际 %s", i+1, es.AliasName, idx)
		}
	}
}

// TestSearch_StaleProfileIndex_StillTargetsAlias 验证 profile 索引名过期时搜索仍打 alias。
//
// 引入动机：真实故障场景是 profile.ESIndexName 指向已不存在的索引
// （手动重建后换了索引名/换了 alias）。此时普通搜索不能因为该脏字段而打到错误索引，
// 断言实际下发的索引名严格等于 alias 且从不出现在 profile 的具体索引名上。
func TestSearch_StaleProfileIndex_StillTargetsAlias(t *testing.T) {
	esClient := &fakeSearchESClient{}
	handler := newTestHandlerWithES(&mockProfileRepo{
		activeProfile: &profile.ProfileRecord{
			ID:          "p1",
			Name:        "default",
			Version:     1,
			Status:      "active",
			ESIndexName: "knowledge_v999_stale",
		},
	}, esClient)

	r := makeSearchRequest(`{"query":"test","mode":"lexical"}`)
	w := httptest.NewRecorder()

	handler.Search(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("搜索应返回 200，实际 %d，body=%s", w.Code, w.Body.String())
	}
	if len(esClient.searchedIndices) == 0 {
		t.Fatal("handler 未向 ES 发起任何检索请求")
	}
	if esClient.searchedIndices[0] != es.AliasName {
		t.Errorf("lexical 检索目标索引应为 %s，实际 %s", es.AliasName, esClient.searchedIndices[0])
	}
	for _, idx := range esClient.searchedIndices {
		if idx == "knowledge_v999_stale" {
			t.Error("搜索不应打 profile 中过期的具体索引名 knowledge_v999_stale")
		}
	}
}

// 确保未使用的导入不报编译错误
var _ = auth.IdentityFromContext
var _ = httpmw.RequestIDFromContext
