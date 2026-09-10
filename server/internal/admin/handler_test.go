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
	"sort"
	"strings"
	"testing"
	"time"

	"partitura/server/internal/auth"
	"partitura/server/internal/es"
	"partitura/server/internal/evaluation"
	"partitura/server/internal/job"
	"partitura/server/internal/profile"
	types "partitura/server/internal/search/types"
)

// mockProfileRepo 用于测试的 profile repository mock。
type mockProfileRepo struct {
	profiles map[string]*profile.ProfileRecord
	// createCalls 记录每次 CreateProfile 收到的输入。
	// 引入动机："新建版本"的继承/覆盖语义只能通过实际传入仓储层的参数来断言，
	// 需要能观察到 handler 合并后的结果。
	createCalls []*profile.CreateProfileInput
	// activateCalls 记录每次 ActivateProfile 的目标 ID。
	// 引入动机：新建版本绝不允许自动激活，必须能观察到"没有发生激活"。
	activateCalls []string
	// archiveCalls 记录每次 ArchiveProfile 的目标 ID。
	// 引入动机：active profile 的 409 必须在触达仓储层之前返回，需要观察到调用为零。
	archiveCalls []string
	// createdSeq 为 CreateProfile 生成自增 ID，避免同一测试内多次创建冲突。
	createdSeq int
	// candidates 是 ListCandidates 返回的数据源。
	// 引入动机：/api/admin/search-profiles/candidates 直接序列化 []profile.CandidateRecord，
	// 需要真实的一段候选数据才能断言元素级 JSON 契约。
	candidates []profile.CandidateRecord
}

// CreateProfile 模拟 PGRepository.CreateProfile 的服务端语义：
// version = 同名 profile 最大 version + 1，es_index_name 由服务端按版本号生成，
// status 固定为 draft（绝不自动激活）。
//
// 引入动机：新建版本测试必须断言 version 递增、es_index_name 非客户端可控、status 为 draft，
// 这些语义由仓储层承担，因此 fake 必须复现同一契约而不是返回零值。
func (m *mockProfileRepo) CreateProfile(ctx context.Context, input *profile.CreateProfileInput) (*profile.ProfileRecord, error) {
	if m.profiles == nil {
		m.profiles = map[string]*profile.ProfileRecord{}
	}
	m.createCalls = append(m.createCalls, input)

	maxVersion := 0
	for _, p := range m.profiles {
		if p != nil && p.Name == input.Name && p.Version > maxVersion {
			maxVersion = p.Version
		}
	}
	newVersion := maxVersion + 1
	m.createdSeq++
	rec := &profile.ProfileRecord{
		ID:                        fmt.Sprintf("created-profile-%d", m.createdSeq),
		Name:                      input.Name,
		Version:                   newVersion,
		Status:                    "draft",
		EmbeddingProvider:         input.EmbeddingProvider,
		EmbeddingModel:            input.EmbeddingModel,
		EmbeddingDimensions:       input.EmbeddingDimensions,
		EmbeddingQueryInstruction: input.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   input.EmbeddingDocInstruction,
		ChunkTargetSize:           input.ChunkTargetSize,
		ChunkOverlap:              input.ChunkOverlap,
		TitleBoost:                input.TitleBoost,
		HeadingBoost:              input.HeadingBoost,
		PathBoost:                 input.PathBoost,
		TagsBoost:                 input.TagsBoost,
		BodyBoost:                 input.BodyBoost,
		Analyzer:                  input.Analyzer,
		LexicalTopK:               input.LexicalTopK,
		VectorTopK:                input.VectorTopK,
		RRFK:                      input.RRFK,
		RerankerProvider:          input.RerankerProvider,
		RerankerModel:             input.RerankerModel,
		RerankerCandidateCount:    input.RerankerCandidateCount,
		RerankerFinalCount:        input.RerankerFinalCount,
		MaxChunksPerDocument:      input.MaxChunksPerDocument,
		MergeAdjacentChunks:       input.MergeAdjacentChunks,
		ESIndexName:               es.GenerateIndexName(newVersion),
		MaxP95LatencyMs:           input.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   input.MaxRerankerCostPerQuery,
		CreatedBy:                 input.CreatedBy,
		CreatedAt:                 "2026-03-09T00:00:00Z",
	}
	m.profiles[rec.ID] = rec
	return rec, nil
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

// ActivateProfile 模拟 PGRepository.ActivateProfile：当前 active 转 inactive，目标转 active。
// 引入动机：需要真实反映"激活是可见副作用"，否则"新建版本未激活"的断言会失去意义。
func (m *mockProfileRepo) ActivateProfile(ctx context.Context, id string) error {
	m.activateCalls = append(m.activateCalls, id)
	for _, p := range m.profiles {
		if p != nil && p.Status == "active" {
			p.Status = "inactive"
		}
	}
	if p, ok := m.profiles[id]; ok {
		p.Status = "active"
		p.ActivatedAt = "2026-03-09T00:00:00Z"
	}
	return nil
}
func (m *mockProfileRepo) DeactivateProfile(ctx context.Context, id string) error { return nil }

// ArchiveProfile 模拟仓储层带守卫的归档：active profile 拒绝归档并返回 ErrProfileNotArchivable。
func (m *mockProfileRepo) ArchiveProfile(ctx context.Context, id string) error {
	m.archiveCalls = append(m.archiveCalls, id)
	p, ok := m.profiles[id]
	if !ok {
		return profile.ErrProfileNotArchivable
	}
	if p.Status == "active" {
		return profile.ErrProfileNotArchivable
	}
	p.Status = "archived"
	return nil
}
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
	// 与 PGRepository.ListCandidates 行为一致：按 status 过滤、按 limit/offset 分页，
	// 且空结果返回空 slice 而非 nil，保证 JSON 序列化为 [] 而非 null。
	filtered := make([]profile.CandidateRecord, 0, len(m.candidates))
	for _, c := range m.candidates {
		if statusFilter == "" || c.Status == statusFilter {
			filtered = append(filtered, c)
		}
	}
	start := offset
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return &profile.ListCandidatesResult{Candidates: filtered[start:end], Total: len(filtered)}, nil
}
func (m *mockProfileRepo) UpdateCandidateStatus(ctx context.Context, id, status string, gatePassed bool, gateDetails json.RawMessage, evaluationResult json.RawMessage) error {
	return nil
}
func (m *mockProfileRepo) ConfirmCandidate(ctx context.Context, id, adminUserID string) error { return nil }

// mockJobRepo 用于测试的 job repository mock。
type mockJobRepo struct {
	enqueuedJobs []enqueuedJob
	// listJobs 是 List 返回的固定任务列表。
	// 引入动机：/api/admin/jobs 直接序列化 job.Job，需要真实的一段数据来断言 JSON 契约。
	listJobs []job.Job
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
	// 与 PGRepository.List 修复后的行为一致：空列表返回空 slice 而非 nil，
	// 保证 JSON 序列化为 [] 而不是 null（前端对 null 做 .length 会抛异常）。
	jobs := m.listJobs
	if jobs == nil {
		jobs = make([]job.Job, 0)
	}
	return &job.ListResult{Jobs: jobs, Total: len(jobs)}, nil
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

// TestListCandidates_SnakeCaseFields 验证候选记录的元素内部字段为 snake_case。
// 引入动机：CandidateRecord 缺少 JSON tag 时，GET /api/admin/search-profiles/candidates
// 的顶层 key 正确、元素内部却退化为 Go 字段名（ID/BaseProfileID/...），
// 前端读取这些字段得到 undefined 并抛 TypeError 导致管理页面白屏。
// 该测试驱动真实 ListCandidates handler 与真实 encoding/json 序列化来锁定元素级契约。
func TestListCandidates_SnakeCaseFields(t *testing.T) {
	profileRepo := &mockProfileRepo{
		candidates: []profile.CandidateRecord{
			{
				ID:                 "cand-1",
				BaseProfileID:      "profile-1",
				CandidateProfileID: "profile-2",
				TuningLevel:        2,
				ParameterChanges:   json.RawMessage(`{"title_boost":1.2}`),
				EvaluationResult:   json.RawMessage(`{"ndcg10":0.5}`),
				GatePassed:         true,
				GateDetails:        json.RawMessage(`{"passed":true}`),
				Status:             "passed",
				AdminConfirmed:     true,
				AdminConfirmedBy:   "user-1",
				AdminConfirmedAt:   "2024-01-02T03:04:05Z",
				CreatedAt:          "2024-01-01T00:00:00Z",
			},
		},
	}
	handler := NewHandler(profileRepo, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/search-profiles/candidates", nil)
	w := httptest.NewRecorder()
	handler.ListCandidates(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	for _, key := range []string{"candidates", "total", "limit", "offset"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("响应顶层缺少字段 %s", key)
		}
	}

	candidates, ok := resp["candidates"].([]interface{})
	if !ok {
		t.Fatalf("期望 candidates 为数组，类型为 %T", resp["candidates"])
	}
	if len(candidates) != 1 {
		t.Fatalf("期望 1 条候选，得到 %d 条", len(candidates))
	}
	first, ok := candidates[0].(map[string]interface{})
	if !ok {
		t.Fatalf("candidates[0] 期望为对象，类型为 %T", candidates[0])
	}

	// snake_case 字段必须全部存在。
	snakeKeys := []string{
		"id", "base_profile_id", "candidate_profile_id", "tuning_level",
		"parameter_changes", "evaluation_result", "gate_passed", "gate_details",
		"status", "admin_confirmed", "admin_confirmed_by", "admin_confirmed_at",
		"created_at",
	}
	for _, key := range snakeKeys {
		if _, ok := first[key]; !ok {
			t.Errorf("candidates[0] 缺少 snake_case 字段 %s", key)
		}
	}

	// Go 字段名不得泄漏到响应中。
	pascalKeys := []string{
		"ID", "BaseProfileID", "CandidateProfileID", "TuningLevel",
		"ParameterChanges", "EvaluationResult", "GatePassed", "GateDetails",
		"Status", "AdminConfirmed", "AdminConfirmedBy", "AdminConfirmedAt",
		"CreatedAt",
	}
	for _, key := range pascalKeys {
		if _, ok := first[key]; ok {
			t.Errorf("candidates[0] 不应出现 PascalCase 字段 %s", key)
		}
	}

	// 字段值必须经真实序列化后仍然可读，且 json.RawMessage 以对象形式透传而非被编码成字符串。
	if first["id"] != "cand-1" {
		t.Errorf("期望 id=cand-1，得到 %v", first["id"])
	}
	if first["base_profile_id"] != "profile-1" {
		t.Errorf("期望 base_profile_id=profile-1，得到 %v", first["base_profile_id"])
	}
	if first["tuning_level"] != float64(2) {
		t.Errorf("期望 tuning_level=2，得到 %v", first["tuning_level"])
	}
	if first["gate_passed"] != true {
		t.Errorf("期望 gate_passed=true，得到 %v", first["gate_passed"])
	}
	if first["admin_confirmed"] != true {
		t.Errorf("期望 admin_confirmed=true，得到 %v", first["admin_confirmed"])
	}
	changes, ok := first["parameter_changes"].(map[string]interface{})
	if !ok {
		t.Fatalf("期望 parameter_changes 为对象，类型为 %T", first["parameter_changes"])
	}
	if changes["title_boost"] != 1.2 {
		t.Errorf("期望 parameter_changes.title_boost=1.2，得到 %v", changes["title_boost"])
	}
	gateDetails, ok := first["gate_details"].(map[string]interface{})
	if !ok {
		t.Fatalf("期望 gate_details 为对象，类型为 %T", first["gate_details"])
	}
	if gateDetails["passed"] != true {
		t.Errorf("期望 gate_details.passed=true，得到 %v", gateDetails["passed"])
	}
}

// TestListCandidates_EmptyResponseNotNull 验证无候选时 candidates 为 [] 而非 null。
// 引入动机：前端对 candidates 直接做数组操作，null 会抛 TypeError 白屏。
func TestListCandidates_EmptyResponseNotNull(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/search-profiles/candidates", nil)
	w := httptest.NewRecorder()
	handler.ListCandidates(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	candidates, ok := resp["candidates"].([]interface{})
	if !ok {
		t.Fatalf("期望 candidates 为数组（非 null），类型为 %T", resp["candidates"])
	}
	if len(candidates) != 0 {
		t.Errorf("期望 0 条候选，得到 %d 条", len(candidates))
	}
	if total, ok := resp["total"].(float64); !ok || total != 0 {
		t.Errorf("期望 total=0，得到 %v", resp["total"])
	}
}

// mockAuditRepo 用于测试的 audit repository mock。
type mockAuditRepo struct {
	// records 按顺序保存所有审计写入。
	// 引入动机：新建版本与归档的审计动作名是需求契约的一部分，必须被真实验证而不是只看源码。
	records []auditRecord
}

// auditRecord 是一次审计写入的快照。
type auditRecord struct {
	userID       string
	action       string
	resourceType string
	resourceID   string
	detail       json.RawMessage
}

func (m *mockAuditRepo) Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error {
	m.records = append(m.records, auditRecord{
		userID:       userID,
		action:       action,
		resourceType: resourceType,
		resourceID:   resourceID,
		detail:       detail,
	})
	return nil
}

// actions 返回已记录的审计动作名。
func (m *mockAuditRepo) actions() []string {
	actions := make([]string, 0, len(m.records))
	for _, r := range m.records {
		actions = append(actions, r.action)
	}
	return actions
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

// keysOf 返回 map 的 key 列表（已排序），用于断言失败时输出实际字段集合。
func keysOf(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestListJobs_SnakeCaseJSONContract 验证 /api/admin/jobs 的 job 字段是 snake_case，
// 与 web/types/api.ts 的 Job 契约逐字段对齐。
//
// 引入动机：job.Job 原先没有任何 json tag，encoding/json 会输出 Go 字段名
// （ID/AttemptCount/LastError/CreatedAt/...），而前端读取的是 snake_case。
// 于是 job.id === undefined，页面执行 job.id.substring(0, 8) 时在渲染期抛
// TypeError，整个列表渲染中断、页面空白且没有任何错误提示（线上现象）。
//
// 本测试驱动真实 ListJobs handler 与真实 encoding/json 序列化，
// 通过 json.Unmarshal 到 map 后逐 key 断言，而不是对源码做字符串包含断言。
func TestListJobs_SnakeCaseJSONContract(t *testing.T) {
	jobRepo := &mockJobRepo{
		listJobs: []job.Job{
			{
				ID:           "0123456789abcdef",
				Type:         types.JobIndexDocument,
				Payload:      map[string]interface{}{"document_id": "doc-1"},
				Status:       types.JobPending,
				AttemptCount: 2,
				MaxAttempts:  3,
				LastError:    "索引失败: 连接超时",
				CreatedAt:    "2026-03-08T10:00:00Z",
				UpdatedAt:    "2026-03-08T10:05:00Z",
				StartedAt:    "2026-03-08T10:01:00Z",
				CompletedAt:  "",
			},
		},
	}
	handler := NewHandler(&mockProfileRepo{}, jobRepo, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs?limit=20&offset=0", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d，body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	// 顶层必须包含 jobs/total/limit/offset
	for _, key := range []string{"jobs", "total", "limit", "offset"} {
		if _, ok := resp[key]; !ok {
			t.Errorf("响应顶层缺少字段 %q，实际字段: %v", key, keysOf(resp))
		}
	}

	rawJobs, ok := resp["jobs"].([]interface{})
	if !ok {
		t.Fatalf("jobs 不是数组，类型为 %T", resp["jobs"])
	}
	if len(rawJobs) != 1 {
		t.Fatalf("期望 1 条 job，得到 %d 条", len(rawJobs))
	}
	first, ok := rawJobs[0].(map[string]interface{})
	if !ok {
		t.Fatalf("jobs[0] 不是对象，类型为 %T", rawJobs[0])
	}

	// jobs[0] 必须存在前端契约声明的 snake_case 字段
	for _, key := range []string{"id", "type", "payload", "status", "locked_by", "attempts", "max_attempts", "error", "created_at", "updated_at", "started_at", "completed_at"} {
		if _, exists := first[key]; !exists {
			t.Errorf("jobs[0] 缺少 snake_case 字段 %q，实际字段: %v", key, keysOf(first))
		}
	}

	// jobs[0] 不得出现 Go 字段名（原先的契约破坏形态）
	for _, key := range []string{"ID", "Type", "Payload", "Status", "LockedBy", "AttemptCount", "MaxAttempts", "LastError", "CreatedAt", "UpdatedAt", "StartedAt", "CompletedAt"} {
		if _, exists := first[key]; exists {
			t.Errorf("jobs[0] 不应出现 Go 字段名 %q，实际字段: %v", key, keysOf(first))
		}
	}

	// key 存在但值错位同样会毁掉页面，因此逐一比对取值
	if got := first["id"]; got != "0123456789abcdef" {
		t.Errorf("id = %v，期望 0123456789abcdef", got)
	}
	if got := first["type"]; got != types.JobIndexDocument {
		t.Errorf("type = %v，期望 %s", got, types.JobIndexDocument)
	}
	if got := first["status"]; got != types.JobPending {
		t.Errorf("status = %v，期望 %s", got, types.JobPending)
	}
	// attempts 与 max_attempts 不是字段名的简单小写化，必须验证真实映射
	if got := first["attempts"]; got != float64(2) {
		t.Errorf("attempts = %v，期望 2", got)
	}
	if got := first["max_attempts"]; got != float64(3) {
		t.Errorf("max_attempts = %v，期望 3", got)
	}
	// error 同样不是 LastError 的小写化
	if got := first["error"]; got != "索引失败: 连接超时" {
		t.Errorf("error = %v，期望 last_error 的值", got)
	}
	if got := first["created_at"]; got != "2026-03-08T10:00:00Z" {
		t.Errorf("created_at = %v，期望 2026-03-08T10:00:00Z", got)
	}
	if got := first["updated_at"]; got != "2026-03-08T10:05:00Z" {
		t.Errorf("updated_at = %v，期望 2026-03-08T10:05:00Z", got)
	}

	if total, ok := resp["total"].(float64); !ok || total != 1 {
		t.Errorf("total = %v，期望 1", resp["total"])
	}
}

// TestListJobs_EmptyJobsIsArray 验证无任务时 jobs 序列化为 [] 而不是 null。
//
// 引入动机：nil slice 会被序列化成 "jobs": null，前端对 null 取 .length 或
// 契约校验（要求 jobs 是数组）都会失败——要么白屏要么误报"格式不正确"。
func TestListJobs_EmptyJobsIsArray(t *testing.T) {
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/jobs", nil)
	w := httptest.NewRecorder()
	handler.ListJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d，body: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}

	jobs, ok := resp["jobs"].([]interface{})
	if !ok {
		t.Fatalf("jobs 不是数组（可能是 null），类型为 %T", resp["jobs"])
	}
	if len(jobs) != 0 {
		t.Errorf("期望 0 条 job，得到 %d 条", len(jobs))
	}
	if total, ok := resp["total"].(float64); !ok || total != 0 {
		t.Errorf("total = %v，期望 0", resp["total"])
	}
}

// newFullProfileRecord 构造各参数互不相同的 source profile 记录。
//
// 引入动机：只有每个参数取值都不同，才能暴露合并时"某个字段取错来源"的缺陷；
// 若各字段用相同默认值，继承与覆盖将无法区分。
func newFullProfileRecord(id, name string, version int, status string) *profile.ProfileRecord {
	return &profile.ProfileRecord{
		ID:                        id,
		Name:                      name,
		Version:                   version,
		Status:                    status,
		EmbeddingProvider:         "openai-compatible",
		EmbeddingModel:            "qwen3-embedding",
		EmbeddingDimensions:       1024,
		EmbeddingQueryInstruction: "query-instruction",
		EmbeddingDocInstruction:   "document-instruction",
		ChunkTargetSize:           512,
		ChunkOverlap:              64,
		TitleBoost:                2.5,
		HeadingBoost:              1.5,
		PathBoost:                 1.25,
		TagsBoost:                 0.75,
		BodyBoost:                 1.0,
		Analyzer:                  "standard",
		LexicalTopK:               50,
		VectorTopK:                40,
		RRFK:                      60,
		RerankerProvider:          "openai-compatible",
		RerankerModel:             "qwen3-reranker",
		RerankerCandidateCount:    20,
		RerankerFinalCount:        10,
		MaxChunksPerDocument:      3,
		MergeAdjacentChunks:       true,
		ESIndexName:               "knowledge_v3",
		MaxP95LatencyMs:           2000,
		MaxRerankerCostPerQuery:   0.01,
		CreatedBy:                 "original-creator",
		CreatedAt:                 "2026-03-01T00:00:00Z",
	}
}

// assertProfileParamsEqual 逐字段比较 profile 的全部可调参数。
//
// 引入动机：继承语义要求"全部参数"一致，逐字段断言才能在失败时指出具体漂移的字段，
// 而不是笼统地断言两个结构体不相等。
func assertProfileParamsEqual(t *testing.T, want, got *profile.ProfileRecord) {
	t.Helper()
	if got.Name != want.Name {
		t.Errorf("name = %q，期望继承 %q", got.Name, want.Name)
	}
	if got.EmbeddingProvider != want.EmbeddingProvider {
		t.Errorf("embedding_provider = %q，期望 %q", got.EmbeddingProvider, want.EmbeddingProvider)
	}
	if got.EmbeddingModel != want.EmbeddingModel {
		t.Errorf("embedding_model = %q，期望 %q", got.EmbeddingModel, want.EmbeddingModel)
	}
	if got.EmbeddingDimensions != want.EmbeddingDimensions {
		t.Errorf("embedding_dimensions = %d，期望 %d", got.EmbeddingDimensions, want.EmbeddingDimensions)
	}
	if got.EmbeddingQueryInstruction != want.EmbeddingQueryInstruction {
		t.Errorf("embedding_query_instruction = %q，期望 %q", got.EmbeddingQueryInstruction, want.EmbeddingQueryInstruction)
	}
	if got.EmbeddingDocInstruction != want.EmbeddingDocInstruction {
		t.Errorf("embedding_document_instruction = %q，期望 %q", got.EmbeddingDocInstruction, want.EmbeddingDocInstruction)
	}
	if got.ChunkTargetSize != want.ChunkTargetSize {
		t.Errorf("chunk_target_size = %d，期望 %d", got.ChunkTargetSize, want.ChunkTargetSize)
	}
	if got.ChunkOverlap != want.ChunkOverlap {
		t.Errorf("chunk_overlap = %d，期望 %d", got.ChunkOverlap, want.ChunkOverlap)
	}
	if got.TitleBoost != want.TitleBoost {
		t.Errorf("title_boost = %v，期望 %v", got.TitleBoost, want.TitleBoost)
	}
	if got.HeadingBoost != want.HeadingBoost {
		t.Errorf("heading_boost = %v，期望 %v", got.HeadingBoost, want.HeadingBoost)
	}
	if got.PathBoost != want.PathBoost {
		t.Errorf("path_boost = %v，期望 %v", got.PathBoost, want.PathBoost)
	}
	if got.TagsBoost != want.TagsBoost {
		t.Errorf("tags_boost = %v，期望 %v", got.TagsBoost, want.TagsBoost)
	}
	if got.BodyBoost != want.BodyBoost {
		t.Errorf("body_boost = %v，期望 %v", got.BodyBoost, want.BodyBoost)
	}
	if got.Analyzer != want.Analyzer {
		t.Errorf("analyzer = %q，期望 %q", got.Analyzer, want.Analyzer)
	}
	if got.LexicalTopK != want.LexicalTopK {
		t.Errorf("lexical_top_k = %d，期望 %d", got.LexicalTopK, want.LexicalTopK)
	}
	if got.VectorTopK != want.VectorTopK {
		t.Errorf("vector_top_k = %d，期望 %d", got.VectorTopK, want.VectorTopK)
	}
	if got.RRFK != want.RRFK {
		t.Errorf("rrf_k = %d，期望 %d", got.RRFK, want.RRFK)
	}
	if got.RerankerProvider != want.RerankerProvider {
		t.Errorf("reranker_provider = %q，期望 %q", got.RerankerProvider, want.RerankerProvider)
	}
	if got.RerankerModel != want.RerankerModel {
		t.Errorf("reranker_model = %q，期望 %q", got.RerankerModel, want.RerankerModel)
	}
	if got.RerankerCandidateCount != want.RerankerCandidateCount {
		t.Errorf("reranker_candidate_count = %d，期望 %d", got.RerankerCandidateCount, want.RerankerCandidateCount)
	}
	if got.RerankerFinalCount != want.RerankerFinalCount {
		t.Errorf("reranker_final_count = %d，期望 %d", got.RerankerFinalCount, want.RerankerFinalCount)
	}
	if got.MaxChunksPerDocument != want.MaxChunksPerDocument {
		t.Errorf("max_chunks_per_document = %d，期望 %d", got.MaxChunksPerDocument, want.MaxChunksPerDocument)
	}
	if got.MergeAdjacentChunks != want.MergeAdjacentChunks {
		t.Errorf("merge_adjacent_chunks = %v，期望 %v", got.MergeAdjacentChunks, want.MergeAdjacentChunks)
	}
	if got.MaxP95LatencyMs != want.MaxP95LatencyMs {
		t.Errorf("max_p95_latency_ms = %d，期望 %d", got.MaxP95LatencyMs, want.MaxP95LatencyMs)
	}
	if got.MaxRerankerCostPerQuery != want.MaxRerankerCostPerQuery {
		t.Errorf("max_reranker_cost_per_query = %v，期望 %v", got.MaxRerankerCostPerQuery, want.MaxRerankerCostPerQuery)
	}
}

// callCreateProfileVersion 驱动真实 handler 并返回响应记录器与使用的仓储/mock。
func callCreateProfileVersion(t *testing.T, repo *mockProfileRepo, jobRepo *mockJobRepo, profileID, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(repo, jobRepo, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/search-profiles/"+profileID+"/versions", strings.NewReader(body))
	req.SetPathValue("id", profileID)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"}))
	w := httptest.NewRecorder()
	handler.CreateProfileVersion(w, req)
	return w
}

// TestCreateProfileVersion_InheritsAllParams 验证不提供任何覆盖字段时完整继承源 profile。
//
// 引入动机：新建版本的核心语义是"以源 profile 为基底复制"，必须逐参数验证；
// 同时必须验证服务端自有字段（version/es_index_name/status/created_by）由服务端产生，
// 并且没有发生任何激活副作用。
func TestCreateProfileVersion_InheritsAllParams(t *testing.T) {
	src := newFullProfileRecord("profile-1", "default", 3, "inactive")
	repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{"profile-1": src}}
	jobRepo := &mockJobRepo{}
	auditRepo := &mockAuditRepo{}
	handler := NewHandler(repo, jobRepo, auditRepo, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/search-profiles/profile-1/versions", strings.NewReader(`{}`))
	req.SetPathValue("id", "profile-1")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"}))

	w := httptest.NewRecorder()
	handler.CreateProfileVersion(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("期望 201，得到 %d，body: %s", w.Code, w.Body.String())
	}

	var got profile.ProfileRecord
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v，body: %s", err, w.Body.String())
	}

	// 全部可调参数逐字段继承
	assertProfileParamsEqual(t, src, &got)

	// 服务端语义：version 递增、index 名由服务端按版本号生成、status 为 draft
	if got.Version != src.Version+1 {
		t.Errorf("version = %d，期望 %d", got.Version, src.Version+1)
	}
	if want := es.GenerateIndexName(src.Version + 1); got.ESIndexName != want {
		t.Errorf("es_index_name = %q，期望服务端生成 %q", got.ESIndexName, want)
	}
	if got.Status != "draft" {
		t.Errorf("status = %q，期望 draft", got.Status)
	}
	if got.ActivatedAt != "" {
		t.Errorf("新建版本不应有 activated_at，实际 %q", got.ActivatedAt)
	}
	// created_by 是当前操作者，而不是源 profile 的创建者
	if got.CreatedBy != "admin-1" {
		t.Errorf("created_by = %q，期望 admin-1（当前认证用户）", got.CreatedBy)
	}

	// 绝不自动激活：无激活调用、无 rebuild job 入队、源 profile 状态未被改动
	if len(repo.activateCalls) != 0 {
		t.Errorf("新建版本不应调用 ActivateProfile，实际调用 %v", repo.activateCalls)
	}
	if len(jobRepo.enqueuedJobs) != 0 {
		t.Errorf("新建版本不应入队任何 job，实际入队 %d 个", len(jobRepo.enqueuedJobs))
	}
	if src.Status != "inactive" {
		t.Errorf("源 profile 状态不应被改动，实际 %q", src.Status)
	}

	// 审计动作与 resource 类型
	if len(auditRepo.records) != 1 {
		t.Fatalf("期望记录 1 条审计，实际 %d 条", len(auditRepo.records))
	}
	if auditRepo.records[0].action != "admin.profile.create_version" {
		t.Errorf("审计动作 = %q，期望 admin.profile.create_version", auditRepo.records[0].action)
	}
	if auditRepo.records[0].resourceType != "search_profile" {
		t.Errorf("审计 resource 类型 = %q，期望 search_profile", auditRepo.records[0].resourceType)
	}
	if auditRepo.records[0].resourceID != got.ID {
		t.Errorf("审计 resource id = %q，期望 %q", auditRepo.records[0].resourceID, got.ID)
	}
}

// TestCreateProfileVersion_PartialOverride 验证只覆盖显式提供的字段，其余仍继承源 profile。
//
// 引入动机：请求体字段用指针表达，必须验证"显式零值覆盖"（title_boost=0、
// merge_adjacent_chunks=false）真的生效，而不是被当作"未提供"而继承。
func TestCreateProfileVersion_PartialOverride(t *testing.T) {
	src := newFullProfileRecord("profile-1", "default", 3, "active")
	repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{"profile-1": src}}

	body := `{"embedding_model":"bge-m3","lexical_top_k":80,"title_boost":0,"merge_adjacent_chunks":false}`
	w := callCreateProfileVersion(t, repo, &mockJobRepo{}, "profile-1", body)

	if w.Code != http.StatusCreated {
		t.Fatalf("期望 201，得到 %d，body: %s", w.Code, w.Body.String())
	}

	var got profile.ProfileRecord
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v，body: %s", err, w.Body.String())
	}

	// 覆盖生效（含显式零值）
	if got.EmbeddingModel != "bge-m3" {
		t.Errorf("embedding_model = %q，期望覆盖为 bge-m3", got.EmbeddingModel)
	}
	if got.LexicalTopK != 80 {
		t.Errorf("lexical_top_k = %d，期望覆盖为 80", got.LexicalTopK)
	}
	if got.TitleBoost != 0 {
		t.Errorf("title_boost = %v，期望显式覆盖为 0", got.TitleBoost)
	}
	if got.MergeAdjacentChunks {
		t.Error("merge_adjacent_chunks 期望显式覆盖为 false")
	}

	// 其余字段继承：把期望记录改成覆盖后的取值，再逐字段比较
	want := *src
	want.EmbeddingModel = "bge-m3"
	want.LexicalTopK = 80
	want.TitleBoost = 0
	want.MergeAdjacentChunks = false
	assertProfileParamsEqual(t, &want, &got)

	if got.Name != "default" {
		t.Errorf("name = %q，期望继承 default", got.Name)
	}
	if got.Version != src.Version+1 {
		t.Errorf("version = %d，期望 %d", got.Version, src.Version+1)
	}
	if got.Status != "draft" {
		t.Errorf("status = %q，期望 draft", got.Status)
	}
}

// TestCreateProfileVersion_NameOverrideStartsNewVersionFamily 验证覆盖 name 后按新名称族计算版本号。
//
// 引入动机：CreateProfile 按 name 计算 maxVersion+1（UNIQUE (name, version)），
// 因此改名建版本会从新名称族的 v1 开始。这是实际契约，必须显式固定下来，
// 避免后续误以为"改名后仍沿用源版本号"。
func TestCreateProfileVersion_NameOverrideStartsNewVersionFamily(t *testing.T) {
	src := newFullProfileRecord("profile-1", "default", 3, "inactive")
	repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{"profile-1": src}}

	w := callCreateProfileVersion(t, repo, &mockJobRepo{}, "profile-1", `{"name":"renamed"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("期望 201，得到 %d，body: %s", w.Code, w.Body.String())
	}

	var got profile.ProfileRecord
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败: %v，body: %s", err, w.Body.String())
	}
	if got.Name != "renamed" {
		t.Errorf("name = %q，期望 renamed", got.Name)
	}
	if got.Version != 1 {
		t.Errorf("version = %d，期望 1（新名称族首版）", got.Version)
	}
	if want := es.GenerateIndexName(1); got.ESIndexName != want {
		t.Errorf("es_index_name = %q，期望 %q", got.ESIndexName, want)
	}
}

// TestCreateProfileVersion_SourceNotFound 验证源 profile 不存在时返回 404。
func TestCreateProfileVersion_SourceNotFound(t *testing.T) {
	repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{}}
	w := callCreateProfileVersion(t, repo, &mockJobRepo{}, "nonexistent", `{}`)

	if w.Code != http.StatusNotFound {
		t.Errorf("期望 404，得到 %d，body: %s", w.Code, w.Body.String())
	}
	if len(repo.createCalls) != 0 {
		t.Errorf("源不存在时不应创建 profile，实际创建 %d 次", len(repo.createCalls))
	}
}

// TestCreateProfileVersion_InvalidOverride 验证覆盖后的非法参数返回 400，且不触达创建。
//
// 引入动机：覆盖值是绕过前端直接调 API 时最危险的输入面，必须与 CreateProfile 使用
// 同一套校验（同一错误文案），且校验失败时绝不落库。
func TestCreateProfileVersion_InvalidOverride(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"维度非正", `{"embedding_dimensions":0}`, "embedding_dimensions 必须为正整数"},
		{"lexical_top_k 非正", `{"lexical_top_k":0}`, "top_k 和 rrf_k 必须为正整数"},
		{"vector_top_k 非正", `{"vector_top_k":-1}`, "top_k 和 rrf_k 必须为正整数"},
		{"rrf_k 非正", `{"rrf_k":0}`, "top_k 和 rrf_k 必须为正整数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newFullProfileRecord("profile-1", "default", 3, "inactive")
			repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{"profile-1": src}}

			w := callCreateProfileVersion(t, repo, &mockJobRepo{}, "profile-1", tc.body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，得到 %d，body: %s", w.Code, w.Body.String())
			}
			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("解析错误响应失败: %v", err)
			}
			if resp["error"] != tc.want {
				t.Errorf("错误消息 = %q，期望与 CreateProfile 一致为 %q", resp["error"], tc.want)
			}
			if len(repo.createCalls) != 0 {
				t.Errorf("校验失败时不应调用 CreateProfile，实际调用 %d 次", len(repo.createCalls))
			}
		})
	}
}

// TestCreateProfileVersion_RejectsServerOwnedFields 验证客户端无法指定服务端自有字段。
//
// 引入动机：version/status/es_index_name/created_by/created_at/activated_at 必须由服务端决定，
// 请求体出现这些字段应被严格解码拒绝（400），而不是被静默忽略后仍成功返回。
func TestCreateProfileVersion_RejectsServerOwnedFields(t *testing.T) {
	bodies := []string{
		`{"version":99}`,
		`{"status":"active"}`,
		`{"es_index_name":"knowledge_v9"}`,
		`{"created_by":"attacker"}`,
		`{"created_at":"2020-01-01T00:00:00Z"}`,
		`{"activated_at":"2020-01-01T00:00:00Z"}`,
		`{"unknown_field":1}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			src := newFullProfileRecord("profile-1", "default", 3, "inactive")
			repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{"profile-1": src}}

			w := callCreateProfileVersion(t, repo, &mockJobRepo{}, "profile-1", body)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，得到 %d，body: %s", w.Code, w.Body.String())
			}
			if len(repo.createCalls) != 0 {
				t.Errorf("严格解码失败时不应创建 profile，实际创建 %d 次", len(repo.createCalls))
			}
		})
	}
}

// --- 归档 ---

// callArchiveProfile 驱动真实 ArchiveProfile handler。
func callArchiveProfile(t *testing.T, repo *mockProfileRepo, profileID string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(repo, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/search-profiles/"+profileID+"/archive", nil)
	req.SetPathValue("id", profileID)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"}))
	w := httptest.NewRecorder()
	handler.ArchiveProfile(w, req)
	return w
}

// TestArchiveProfile_SucceedsForNonActiveStatuses 验证 draft/inactive/archived 均可归档。
//
// 引入动机：归档必须可逆且幂等友好——已归档的 profile 再次归档不应报错，
// 这样重试/重复点击不会产生假失败。
func TestArchiveProfile_SucceedsForNonActiveStatuses(t *testing.T) {
	for _, status := range []string{"draft", "inactive", "archived"} {
		t.Run(status, func(t *testing.T) {
			src := newFullProfileRecord("profile-1", "default", 3, status)
			repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{"profile-1": src}}
			auditRepo := &mockAuditRepo{}
			handler := NewHandler(repo, &mockJobRepo{}, auditRepo, &mockEvalRepo{}, &mockEvalResultRepo{})

			req := httptest.NewRequest(http.MethodPost, "/api/admin/search-profiles/profile-1/archive", nil)
			req.SetPathValue("id", "profile-1")
			req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"}))
			w := httptest.NewRecorder()
			handler.ArchiveProfile(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("期望 200，得到 %d，body: %s", w.Code, w.Body.String())
			}
			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("解析响应失败: %v", err)
			}
			if resp["status"] != "archived" {
				t.Errorf("status = %q，期望 archived", resp["status"])
			}
			if src.Status != "archived" {
				t.Errorf("记录状态 = %q，期望 archived", src.Status)
			}
			// 归档是状态迁移而非删除：记录仍可被查询
			if _, err := repo.GetProfileByID(context.Background(), "profile-1"); err != nil {
				t.Errorf("归档后记录应保留，实际查询失败: %v", err)
			}
			if len(repo.archiveCalls) != 1 {
				t.Errorf("期望调用仓储层归档 1 次，实际 %d 次", len(repo.archiveCalls))
			}
			if len(auditRepo.records) != 1 || auditRepo.records[0].action != "admin.profile.archive" {
				t.Errorf("期望审计动作 admin.profile.archive，实际 %v", auditRepo.actions())
			}
			if len(auditRepo.records) == 1 && auditRepo.records[0].resourceType != "search_profile" {
				t.Errorf("审计 resource 类型 = %q，期望 search_profile", auditRepo.records[0].resourceType)
			}
		})
	}
}

// TestArchiveProfile_ActiveConflict 验证 active profile 返回 409 且状态不变。
//
// 引入动机：归档 active profile 会让系统失去当前生效配置；handler 必须在触达仓储层
// 之前就拒绝，避免出现"守卫拦截后的部分变更"。
func TestArchiveProfile_ActiveConflict(t *testing.T) {
	src := newFullProfileRecord("profile-1", "default", 3, "active")
	repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{"profile-1": src}}
	auditRepo := &mockAuditRepo{}
	handler := NewHandler(repo, &mockJobRepo{}, auditRepo, &mockEvalRepo{}, &mockEvalResultRepo{})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/search-profiles/profile-1/archive", nil)
	req.SetPathValue("id", "profile-1")
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserID: "admin-1"}))
	w := httptest.NewRecorder()
	handler.ArchiveProfile(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("期望 409，得到 %d，body: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if !strings.Contains(resp["error"], "active") {
		t.Errorf("错误消息 %q 应说明 active profile 不能归档", resp["error"])
	}
	// 状态未被改动，且未触达仓储层归档
	if src.Status != "active" {
		t.Errorf("active profile 状态不应变更，实际 %q", src.Status)
	}
	if len(repo.archiveCalls) != 0 {
		t.Errorf("active profile 不应调用仓储层归档，实际调用 %v", repo.archiveCalls)
	}
	if len(auditRepo.records) != 0 {
		t.Errorf("拒绝归档不应写审计，实际 %v", auditRepo.actions())
	}
}

// TestArchiveProfile_NotFound 验证 id 不存在时返回 404。
func TestArchiveProfile_NotFound(t *testing.T) {
	repo := &mockProfileRepo{profiles: map[string]*profile.ProfileRecord{}}
	w := callArchiveProfile(t, repo, "nonexistent")

	if w.Code != http.StatusNotFound {
		t.Errorf("期望 404，得到 %d，body: %s", w.Code, w.Body.String())
	}
	if len(repo.archiveCalls) != 0 {
		t.Errorf("目标不存在时不应调用仓储层归档，实际 %v", repo.archiveCalls)
	}
}

// TestRegisterRoutes_RegistersNewProfileRoutes 验证新增路由能与既有路由共存注册。
//
// 引入动机：Go 1.22 的 ServeMux 在注册互相冲突的 pattern 时直接 panic，
// 失败的时机是服务启动（而非编译期）。新增 .../{id}/versions 与 .../{id}/archive 后，
// 必须在测试期就驱动真实 RegisterRoutes 完成注册，否则路由冲突只能等到线上启动才暴露。
//
// 关于 nil 依赖：authRepo 只在 middleware 闭包的请求处理阶段被访问，
// 注册阶段仅构建闭包链，因此传 nil 足以覆盖"注册不 panic"这一目标。
func TestRegisterRoutes_RegistersNewProfileRoutes(t *testing.T) {
	mux := http.NewServeMux()
	handler := NewHandler(&mockProfileRepo{}, &mockJobRepo{}, &mockAuditRepo{}, &mockEvalRepo{}, &mockEvalResultRepo{})

	RegisterRoutes(mux, handler, nil, auth.AuthConfig{})

	// 注册成功后应能匹配到新增的两个端点（这里只验证路由表存在，不做鉴权流程）。
	req := httptest.NewRequest(http.MethodPost, "/api/admin/search-profiles/some-id/versions", strings.NewReader(`{}`))
	if _, pattern := mux.Handler(req); pattern != "POST /api/admin/search-profiles/{id}/versions" {
		t.Errorf("versions 路由未注册，匹配到 pattern %q", pattern)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/admin/search-profiles/some-id/archive", nil)
	if _, pattern := mux.Handler(req); pattern != "POST /api/admin/search-profiles/{id}/archive" {
		t.Errorf("archive 路由未注册，匹配到 pattern %q", pattern)
	}
}

// 确保 io 包被使用（用于 httptest）
var _ = io.EOF
