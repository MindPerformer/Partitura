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
func (m *mockProfileRepo) ActivateProfile(ctx context.Context, id string) error { return nil }
func (m *mockProfileRepo) DeactivateProfile(ctx context.Context, id string) error { return nil }
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

// 确保未使用的导入不报编译错误
var _ = auth.IdentityFromContext
var _ = httpmw.RequestIDFromContext
