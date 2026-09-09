// handler_test.go 测试 admin handler 的评测和 job 管理逻辑。
//
// 引入动机：验证 RunEvaluation 不再是空壳，而是真正入队 evaluate_profile job 并返回 job id；
// 验证 ListEvaluationResults 可查询评测结果。
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"partitura/server/internal/auth"
	"partitura/server/internal/evaluation"
	"partitura/server/internal/job"
	"partitura/server/internal/profile"
	types "partitura/server/internal/search/types"
)

// mockProfileRepo 用于测试的 profile repository mock。
type mockProfileRepo struct {
	profiles map[string]*profile.ProfileRecord
}

func (m *mockProfileRepo) CreateProfile(ctx context.Context, p *profile.CreateProfileInput) (*profile.ProfileRecord, error) {
	return nil, nil
}
func (m *mockProfileRepo) GetProfileByID(ctx context.Context, id string) (*profile.ProfileRecord, error) {
	if p, ok := m.profiles[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("profile 不存在: %s", id)
}
func (m *mockProfileRepo) ListProfiles(ctx context.Context, statusFilter string, limit, offset int) (*profile.ListProfilesResult, error) {
	// 与 PGRepository 修复后的行为一致：空列表返回空 slice 而非 nil，
	// 保证 JSON 序列化为 [] 而非 null。
	profiles := make([]profile.ProfileRecord, 0, len(m.profiles))
	for _, p := range m.profiles {
		if p != nil {
			profiles = append(profiles, *p)
		}
	}
	return &profile.ListProfilesResult{Profiles: profiles, Total: len(profiles)}, nil
}
func (m *mockProfileRepo) GetActiveProfile(ctx context.Context) (*profile.ProfileRecord, error) {
	return nil, nil
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
	return &profile.ListCandidatesResult{}, nil
}
func (m *mockProfileRepo) UpdateCandidateStatus(ctx context.Context, id, status string, gatePassed bool, gateDetails json.RawMessage, evaluationResult json.RawMessage) error {
	return nil
}
func (m *mockProfileRepo) ConfirmCandidate(ctx context.Context, id, adminUserID string) error { return nil }

// mockJobRepo 用于测试的 job repository mock。
type mockJobRepo struct {
	enqueuedJobs []enqueuedJob
}

type enqueuedJob struct {
	id      string
	jobType string
	payload map[string]interface{}
}

func (m *mockJobRepo) Enqueue(ctx context.Context, jobType string, payload map[string]interface{}) (string, error) {
	id := "test-job-id"
	m.enqueuedJobs = append(m.enqueuedJobs, enqueuedJob{id: id, jobType: jobType, payload: payload})
	return id, nil
}
func (m *mockJobRepo) Claim(ctx context.Context, workerID string) (*job.Job, error) {
	return nil, nil
}
func (m *mockJobRepo) Complete(ctx context.Context, jobID string) error { return nil }
func (m *mockJobRepo) Fail(ctx context.Context, jobID string, errMsg string) error {
	return nil
}
func (m *mockJobRepo) List(ctx context.Context, statusFilter string, limit, offset int) (*job.ListResult, error) {
	return &job.ListResult{}, nil
}
func (m *mockJobRepo) GetByID(ctx context.Context, id string) (*job.Job, error) {
	return nil, nil
}
func (m *mockJobRepo) Retry(ctx context.Context, id string) error { return nil }
func (m *mockJobRepo) EnqueueInTx(ctx context.Context, tx *sql.Tx, jobType string, payload map[string]interface{}) (string, error) {
	return "", nil
}
func (m *mockJobRepo) RecoverStaleJobs(ctx context.Context, staleTimeout time.Duration) (int, error) {
	return 0, nil
}

// mockEvalRepo 用于测试的 evaluation repository mock。
type mockEvalRepo struct {
	datasets map[string]*evaluation.Dataset
	items    map[string][]evaluation.Item
}

func (m *mockEvalRepo) CreateDataset(ctx context.Context, name, description, createdBy string) (*evaluation.Dataset, error) {
	return nil, nil
}
func (m *mockEvalRepo) ListDatasets(ctx context.Context, statusFilter string, limit, offset int) (*evaluation.ListDatasetsResult, error) {
	return &evaluation.ListDatasetsResult{}, nil
}
func (m *mockEvalRepo) GetDataset(ctx context.Context, id string) (*evaluation.Dataset, error) {
	if ds, ok := m.datasets[id]; ok {
		return ds, nil
	}
	return nil, fmt.Errorf("数据集不存在: %s", id)
}
func (m *mockEvalRepo) AddItem(ctx context.Context, datasetID, query string, expectedDocs []evaluation.ExpectedDocument, relevanceGrade int, queryClass string) (*evaluation.Item, error) {
	return nil, nil
}
func (m *mockEvalRepo) ListItems(ctx context.Context, datasetID string, limit, offset int) (*evaluation.ListItemsResult, error) {
	return &evaluation.ListItemsResult{}, nil
}
func (m *mockEvalRepo) ListAllItems(ctx context.Context, datasetID string) ([]evaluation.Item, error) {
	if items, ok := m.items[datasetID]; ok {
		return items, nil
	}
	return nil, nil
}

// mockEvalResultRepo 用于测试的 evaluation result repository mock。
type mockEvalResultRepo struct {
	results []evaluation.EvaluationRunResult
}

func (m *mockEvalResultRepo) SaveResult(ctx context.Context, datasetID, profileID, jobID string, metrics json.RawMessage, itemCount int) (*evaluation.EvaluationRunResult, error) {
	r := evaluation.EvaluationRunResult{
		ID:        "result-id",
		DatasetID: datasetID,
		ProfileID: profileID,
		JobID:     jobID,
		Metrics:   metrics,
		ItemCount: itemCount,
	}
	m.results = append(m.results, r)
	return &r, nil
}
func (m *mockEvalResultRepo) ListResults(ctx context.Context, datasetID string, limit, offset int) (*evaluation.ListResultsResult, error) {
	var filtered []evaluation.EvaluationRunResult
	for _, r := range m.results {
		if r.DatasetID == datasetID {
			filtered = append(filtered, r)
		}
	}
	return &evaluation.ListResultsResult{Results: filtered, Total: len(filtered)}, nil
}
func (m *mockEvalResultRepo) GetLatestResult(ctx context.Context, datasetID, profileID string) (*evaluation.EvaluationRunResult, error) {
	return nil, nil
}

// TestListProfiles_EmptyResponseNotNull 验证空 profile 列表返回 profiles: [] 而非 null。
func TestListProfiles_EmptyResponseNotNull(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/search-profiles", nil)
	w := httptest.NewRecorder()
	handler.ListProfiles(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	profiles, ok := resp["profiles"].([]interface{})
	if !ok {
		t.Fatalf("profiles 不是数组，类型为 %T", resp["profiles"])
	}
	if len(profiles) != 0 {
		t.Errorf("期望 0 条 profile，得到 %d 条", len(profiles))
	}

	total, ok := resp["total"].(float64)
	if !ok || total != 0 {
		t.Errorf("期望 total=0，得到 %v", resp["total"])
	}
}

// TestListProfiles_SnakeCaseFields 验证 profile 记录字段为 snake_case。
// 引入动机：Phase6 WP1 修复 — web SearchProfile 期望 snake_case，防止 undefined。
func TestListProfiles_SnakeCaseFields(t *testing.T) {
	profileRepo := &mockProfileRepo{
		profiles: map[string]*profile.ProfileRecord{
			"profile-1": {ID: "profile-1", Name: "test-profile"},
		},
	}
	handler := NewHandler(profileRepo, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/search-profiles", nil)
	w := httptest.NewRecorder()
	handler.ListProfiles(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", w.Code)
	}

	body, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"embedding_model"`) {
		t.Error("响应中缺少 snake_case 字段 embedding_model")
	}
	if !strings.Contains(bodyStr, `"lexical_top_k"`) {
		t.Error("响应中缺少 snake_case 字段 lexical_top_k")
	}
	if !strings.Contains(bodyStr, `"max_p95_latency_ms"`) {
		t.Error("响应中缺少 snake_case 字段 max_p95_latency_ms")
	}
	if strings.Contains(bodyStr, `"EmbeddingModel"`) {
		t.Error("响应中不应出现 PascalCase 字段 EmbeddingModel")
	}
}

// mockAuditRepo 用于测试的 audit repository mock。
type mockAuditRepo struct{}

func (m *mockAuditRepo) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	return nil
}

// TestRunEvaluation_EnqueuesJob 验证 RunEvaluation 真正入队 evaluate_profile job 并返回 job id。
// 引入动机：修复 RunEvaluation 空壳问题——必须真正触发可观察的 evaluation 执行。
func TestRunEvaluation_EnqueuesJob(t *testing.T) {
	profileRepo := &mockProfileRepo{
		profiles: map[string]*profile.ProfileRecord{
			"profile-1": {ID: "profile-1", Name: "test-profile"},
		},
	}
	jobRepo := &mockJobRepo{}
	evalRepo := &mockEvalRepo{
		datasets: map[string]*evaluation.Dataset{
			"ds-1": {ID: "ds-1", Name: "test-dataset"},
		},
	}
	evalResultRepo := &mockEvalResultRepo{}
	auditRepo := &mockAuditRepo{}

	handler := NewHandler(profileRepo, jobRepo, auditRepo, evalRepo, evalResultRepo)

	body := `{"dataset_id":"ds-1","profile_id":"profile-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/evaluation/run", strings.NewReader(body))
	req.SetPathValue("id", "")

	// 注入认证身份
	ctx := auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.RunEvaluation(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("期望状态码 202，得到 %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// 验证返回了 job_id
	jobID, ok := result["job_id"]
	if !ok || jobID == "" {
		t.Error("响应中缺少 job_id 或 job_id 为空")
	}

	// 验证返回了 status=enqueued
	status, _ := result["status"].(string)
	if status != "enqueued" {
		t.Errorf("期望 status=enqueued，得到 %q", status)
	}

	// 验证真正入队了 evaluate_profile job
	if len(jobRepo.enqueuedJobs) != 1 {
		t.Fatalf("期望入队 1 个 job，实际入队 %d 个", len(jobRepo.enqueuedJobs))
	}
	enqueued := jobRepo.enqueuedJobs[0]
	if enqueued.jobType != types.JobEvaluateProfile {
		t.Errorf("期望 job 类型 %s，得到 %s", types.JobEvaluateProfile, enqueued.jobType)
	}
	if enqueued.payload["dataset_id"] != "ds-1" {
		t.Errorf("payload dataset_id 期望 ds-1，得到 %v", enqueued.payload["dataset_id"])
	}
	if enqueued.payload["profile_id"] != "profile-1" {
		t.Errorf("payload profile_id 期望 profile-1，得到 %v", enqueued.payload["profile_id"])
	}
}

// TestRunEvaluation_MissingDatasetID 验证缺少 dataset_id 时返回 400。
func TestRunEvaluation_MissingDatasetID(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	body := `{"profile_id":"profile-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/evaluation/run", strings.NewReader(body))
	ctx := auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.RunEvaluation(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("期望 400，得到 %d", w.Code)
	}
}

// TestRunEvaluation_DatasetNotFound 验证数据集不存在时返回 404。
func TestRunEvaluation_DatasetNotFound(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	body := `{"dataset_id":"nonexistent","profile_id":"profile-1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/evaluation/run", strings.NewReader(body))
	ctx := auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.RunEvaluation(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("期望 404，得到 %d", w.Code)
	}
}

// TestRunEvaluation_ProfileNotFound 验证 profile 不存在时返回 404。
func TestRunEvaluation_ProfileNotFound(t *testing.T) {
	profileRepo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{}}
	evalRepo := &mockEvalRepo{
		datasets: map[string]*evaluation.Dataset{
			"ds-1": {ID: "ds-1", Name: "test-dataset"},
		},
	}
	handler := NewHandler(profileRepo, &mockJobRepo{}, &mockAuditRepo{}, evalRepo, &mockEvalResultRepo{})

	body := `{"dataset_id":"ds-1","profile_id":"nonexistent"}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/evaluation/run", strings.NewReader(body))
	ctx := auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"})
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.RunEvaluation(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("期望 404，得到 %d", w.Code)
	}
}

// TestListEvaluationResults 验证评测结果列表查询。
func TestListEvaluationResults(t *testing.T) {
	evalResultRepo := &mockEvalResultRepo{
		results: []evaluation.EvaluationRunResult{
			{ID: "r1", DatasetID: "ds-1", ProfileID: "p1", ItemCount: 5},
			{ID: "r2", DatasetID: "ds-1", ProfileID: "p2", ItemCount: 5},
			{ID: "r3", DatasetID: "ds-2", ProfileID: "p1", ItemCount: 3},
		},
	}
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, evalResultRepo)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/evaluation/results?dataset_id=ds-1", nil)
	w := httptest.NewRecorder()
	handler.ListEvaluationResults(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	results := resp["results"].([]interface{})
	if len(results) != 2 {
		t.Errorf("期望 2 条结果（ds-1），得到 %d 条", len(results))
	}
}

// TestListEvaluationResults_MissingDatasetID 验证缺少 dataset_id 时返回 400。
func TestListEvaluationResults_MissingDatasetID(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/evaluation/results", nil)
	w := httptest.NewRecorder()
	handler.ListEvaluationResults(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("期望 400，得到 %d", w.Code)
	}
}

// --- 分页 query 解析测试 ---
//
// 引入动机：远程发现后端 query 解析 bug——代理层损坏 query string 导致后端收到
// offset="0?offset=0" 或 limit="20?offset=0"。以下测试直接调用 admin handler
// 并构造 httptest.NewRequest 验证各种 query string 场景。

// TestListJobs_QueryOffsetLimit 正确解析 offset=0&limit=20。
func TestListJobs_QueryOffsetLimit(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs?offset=0&limit=20", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("offset=0&limit=20 应返回 200，实际 %d，body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if int(resp["limit"].(float64)) != 20 {
		t.Errorf("limit = %v, 期望 20", resp["limit"])
	}
	if int(resp["offset"].(float64)) != 0 {
		t.Errorf("offset = %v, 期望 0", resp["offset"])
	}
}

// TestListJobs_QueryReversedOrder 正确解析反序参数 limit=20&offset=0。
func TestListJobs_QueryReversedOrder(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs?limit=20&offset=0", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("反序参数应返回 200，实际 %d，body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if int(resp["limit"].(float64)) != 20 {
		t.Errorf("limit = %v, 期望 20", resp["limit"])
	}
	if int(resp["offset"].(float64)) != 0 {
		t.Errorf("offset = %v, 期望 0", resp["offset"])
	}
}

// TestListJobs_QuerySingleParam_LimitOnly 正确解析单参数 limit。
func TestListJobs_QuerySingleParam_LimitOnly(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs?limit=10", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("单参数 limit 应返回 200，实际 %d，body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if int(resp["limit"].(float64)) != 10 {
		t.Errorf("limit = %v, 期望 10", resp["limit"])
	}
	if int(resp["offset"].(float64)) != 0 {
		t.Errorf("offset = %v, 期望 0 (默认值)", resp["offset"])
	}
}

// TestListJobs_QueryNoParams 无 query 参数时使用默认值。
func TestListJobs_QueryNoParams(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("无 query 参数应返回 200，实际 %d，body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if int(resp["limit"].(float64)) != 20 {
		t.Errorf("默认 limit = %v, 期望 20", resp["limit"])
	}
	if int(resp["offset"].(float64)) != 0 {
		t.Errorf("默认 offset = %v, 期望 0", resp["offset"])
	}
}

// TestListJobs_QueryInvalidLimit 非法 limit 返回 400。
func TestListJobs_QueryInvalidLimit(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs?limit=abc", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 limit 应返回 400，实际 %d", w.Code)
	}
}

// TestListJobs_QueryInvalidOffset 非法 offset 返回 400。
func TestListJobs_QueryInvalidOffset(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs?offset=xyz", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 offset 应返回 400，实际 %d", w.Code)
	}
}

// TestListJobs_PathParamNoQuery 路径参数不含 query 时正确处理。
func TestListJobs_PathParamNoQuery(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("路径参数不含 query 应返回 200，实际 %d，body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if int(resp["limit"].(float64)) != 20 {
		t.Errorf("默认 limit = %v, 期望 20", resp["limit"])
	}
	if int(resp["offset"].(float64)) != 0 {
		t.Errorf("默认 offset = %v, 期望 0", resp["offset"])
	}
}

// 确保 io 包被使用（用于 httptest）
var _ = io.EOF
