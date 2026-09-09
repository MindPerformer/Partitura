// Package admin 实现搜索管理 HTTP handler：Search Profile 管理、Evaluation、Job 管理。
//
// 引入动机：design/04-WEB-API.md §Admin 要求：
//   - search profiles 管理
//   - evaluation 管理
//   - jobs 管理
//
// 所有 admin 端点仅 system_admin 可访问（RequireSystemAdmin 在路由层验证）。
//
// 设计原则：
//   - handler 只负责 HTTP 解析和响应
//   - 严格 JSON 解码（拒绝未知字段/畸形 JSON/多值）
//   - 分页和 limit 强校验
//   - 安全敏感操作记录 audit
//   - 不出现 XxxService/XxxManager/XxxController
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"partitura/server/internal/auth"
	"partitura/server/internal/evaluation"
	httpmw "partitura/server/internal/http"
	"partitura/server/internal/job"
	"partitura/server/internal/profile"
	"partitura/server/internal/queryutil"
	types "partitura/server/internal/search/types"
	"partitura/server/internal/workspace"
)

// Handler 是搜索管理 HTTP handler。
type Handler struct {
	profileRepo    profile.Repository
	jobRepo        job.Repository
	auditRepo      AuditRepository
	evalRepo       evaluation.Repository
	evalResultRepo evaluation.ResultRepository
}

// AuditRepository 定义审计记录接口。
// 引入动机：admin 操作需要记录审计日志。
type AuditRepository interface {
	Record(ctx context.Context, userID, workspaceID, action, resourceType, resourceID string, detail json.RawMessage, requestID string) error
}

// NewHandler 创建搜索管理 handler。
// 引入动机：H7 要求 admin handler 注入 auditRepo 并记录审计日志。
// C2 要求 evaluation API 完整实现，注入 evalRepo 和 evalResultRepo。
func NewHandler(profileRepo profile.Repository, jobRepo job.Repository, auditRepo AuditRepository, evalRepo evaluation.Repository, evalResultRepo evaluation.ResultRepository) *Handler {
	return &Handler{
		profileRepo:    profileRepo,
		jobRepo:        jobRepo,
		auditRepo:      auditRepo,
		evalRepo:       evalRepo,
		evalResultRepo: evalResultRepo,
	}
}

// recordAudit 记录审计日志。
// 引入动机：H7 要求 admin 操作记录审计日志。
func (h *Handler) recordAudit(r *http.Request, userID, action, resourceType, resourceID string, detail interface{}) {
	if h.auditRepo == nil {
		return
	}
	var detailJSON json.RawMessage
	if detail != nil {
		data, err := json.Marshal(detail)
		if err != nil {
			slog.Error("序列化审计 detail 失败", "error", err, "action", action)
			return
		}
		detailJSON = data
	}
	requestID := httpmw.RequestIDFromContext(r.Context())
	if err := h.auditRepo.Record(r.Context(), userID, "", action, resourceType, resourceID, detailJSON, requestID); err != nil {
		slog.Error("写入审计日志失败", "error", err, "action", action)
	}
}

// --- Search Profile ---

// createProfileRequest 是创建 profile 的请求体。
type createProfileRequest struct {
	Name                     string  `json:"name"`
	EmbeddingProvider        string  `json:"embedding_provider"`
	EmbeddingModel           string  `json:"embedding_model"`
	EmbeddingDimensions      int     `json:"embedding_dimensions"`
	EmbeddingQueryInstruction string  `json:"embedding_query_instruction"`
	EmbeddingDocInstruction  string  `json:"embedding_document_instruction"`
	ChunkTargetSize          int     `json:"chunk_target_size"`
	ChunkOverlap             int     `json:"chunk_overlap"`
	TitleBoost               float32 `json:"title_boost"`
	HeadingBoost             float32 `json:"heading_boost"`
	PathBoost                float32 `json:"path_boost"`
	TagsBoost                float32 `json:"tags_boost"`
	BodyBoost                float32 `json:"body_boost"`
	Analyzer                 string  `json:"analyzer"`
	LexicalTopK              int     `json:"lexical_top_k"`
	VectorTopK               int     `json:"vector_top_k"`
	RRFK                     int     `json:"rrf_k"`
	RerankerProvider         string  `json:"reranker_provider"`
	RerankerModel            string  `json:"reranker_model"`
	RerankerCandidateCount   int     `json:"reranker_candidate_count"`
	RerankerFinalCount       int     `json:"reranker_final_count"`
	MaxChunksPerDocument     int     `json:"max_chunks_per_document"`
	MergeAdjacentChunks      bool    `json:"merge_adjacent_chunks"`
	MaxP95LatencyMs          int     `json:"max_p95_latency_ms"`
	MaxRerankerCostPerQuery  float32 `json:"max_reranker_cost_per_query"`
}

// ListProfiles 处理 GET /api/admin/search-profiles。
func (h *Handler) ListProfiles(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	statusFilter := r.URL.Query().Get("status")
	result, err := h.profileRepo.ListProfiles(r.Context(), statusFilter, limit, offset)
	if err != nil {
		slog.Error("查询 profile 列表失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"profiles": result.Profiles,
		"total":    result.Total,
		"limit":    limit,
		"offset":   offset,
	})
}

// CreateProfile 处理 POST /api/admin/search-profiles。
func (h *Handler) CreateProfile(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req createProfileRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	if req.Name == "" {
		writeAdminError(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if req.EmbeddingDimensions <= 0 {
		writeAdminError(w, http.StatusBadRequest, "embedding_dimensions 必须为正整数")
		return
	}
	if req.LexicalTopK <= 0 || req.VectorTopK <= 0 || req.RRFK <= 0 {
		writeAdminError(w, http.StatusBadRequest, "top_k 和 rrf_k 必须为正整数")
		return
	}

	input := &profile.CreateProfileInput{
		Name:                     req.Name,
		EmbeddingProvider:        req.EmbeddingProvider,
		EmbeddingModel:           req.EmbeddingModel,
		EmbeddingDimensions:      req.EmbeddingDimensions,
		EmbeddingQueryInstruction: req.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   req.EmbeddingDocInstruction,
		ChunkTargetSize:          req.ChunkTargetSize,
		ChunkOverlap:             req.ChunkOverlap,
		TitleBoost:               req.TitleBoost,
		HeadingBoost:             req.HeadingBoost,
		PathBoost:                req.PathBoost,
		TagsBoost:                req.TagsBoost,
		BodyBoost:                req.BodyBoost,
		Analyzer:                 req.Analyzer,
		LexicalTopK:              req.LexicalTopK,
		VectorTopK:               req.VectorTopK,
		RRFK:                     req.RRFK,
		RerankerProvider:         req.RerankerProvider,
		RerankerModel:            req.RerankerModel,
		RerankerCandidateCount:   req.RerankerCandidateCount,
		RerankerFinalCount:       req.RerankerFinalCount,
		MaxChunksPerDocument:     req.MaxChunksPerDocument,
		MergeAdjacentChunks:      req.MergeAdjacentChunks,
		MaxP95LatencyMs:          req.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:  req.MaxRerankerCostPerQuery,
		CreatedBy:                id.UserID,
	}

	p, err := h.profileRepo.CreateProfile(r.Context(), input)
	if err != nil {
		slog.Error("创建 profile 失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "创建失败")
		return
	}

	slog.Info("search profile 已创建", "id", p.ID, "name", p.Name, "version", p.Version, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.profile.create", "search_profile", p.ID, map[string]string{"name": p.Name})
	writeAdminJSON(w, http.StatusCreated, p)
}

// ActivateProfile 处理 POST /api/admin/search-profiles/{id}/activate。
// 引入动机：C4 要求 Profile activate 触发 rebuild + alias 切换。
// 激活后 enqueue rebuild_index job，由 job worker 执行：
// 1. 创建新 versioned index
// 2. 全量 reindex
// 3. integrity validation
// 4. alias 原子切换
// 5. 保留旧 index
func (h *Handler) ActivateProfile(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	profileID := r.PathValue("id")
	if profileID == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 profile ID")
		return
	}

	// 获取 profile 信息用于审计和确定 index_name
	p, err := h.profileRepo.GetProfileByID(r.Context(), profileID)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "profile 不存在")
		return
	}

	if err := h.profileRepo.ActivateProfile(r.Context(), profileID); err != nil {
		slog.Error("激活 profile 失败", "error", err, "profile_id", profileID)
		writeAdminError(w, http.StatusInternalServerError, "激活失败")
		return
	}

	// C4：enqueue rebuild_index job，由 job worker 执行全量重建 + alias 切换
	indexName := p.ESIndexName
	if indexName == "" {
		slog.Warn("profile 无 ESIndexName，rebuild job 将使用默认 alias", "profile_id", profileID)
	}
	rebuildPayload := map[string]interface{}{
		"index_name":  indexName,
		"profile_id":  profileID,
	}
	if _, err := h.jobRepo.Enqueue(r.Context(), types.JobRebuildIndex, rebuildPayload); err != nil {
		slog.Error("enqueue rebuild job 失败", "profile_id", profileID, "error", err)
		// 不返回错误——profile 已激活，rebuild 可手动触发
	}

	slog.Info("search profile 已激活，rebuild job 已入队", "profile_id", profileID, "name", p.Name, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.profile.activate", "search_profile", profileID, map[string]string{"name": p.Name})
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "activated"})
}

// RollbackProfile 处理 POST /api/admin/search-profiles/{id}/rollback。
// 引入动机：design/01-SEARCH.md §Regression Gate 要求支持立即回滚到旧 Search Profile。
// C4：回滚 = 激活旧 profile + enqueue rebuild job 切换 alias 指向旧 index。
func (h *Handler) RollbackProfile(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	profileID := r.PathValue("id")
	if profileID == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 profile ID")
		return
	}

	// 回滚 = 激活旧 profile
	p, err := h.profileRepo.GetProfileByID(r.Context(), profileID)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "profile 不存在")
		return
	}

	if err := h.profileRepo.ActivateProfile(r.Context(), profileID); err != nil {
		slog.Error("回滚 profile 失败", "error", err, "profile_id", profileID)
		writeAdminError(w, http.StatusInternalServerError, "回滚失败")
		return
	}

	// C4：enqueue rebuild job 切换 alias 指向旧 profile 的 index
	indexName := p.ESIndexName
	if indexName != "" {
		rebuildPayload := map[string]interface{}{
			"index_name":  indexName,
			"profile_id":  profileID,
		}
		if _, err := h.jobRepo.Enqueue(r.Context(), types.JobRebuildIndex, rebuildPayload); err != nil {
			slog.Error("enqueue rebuild job 失败（回滚）", "profile_id", profileID, "error", err)
		}
	}

	slog.Info("search profile 已回滚，rebuild job 已入队", "profile_id", profileID, "name", p.Name, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.profile.rollback", "search_profile", profileID, map[string]string{"name": p.Name})
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "rolled_back"})
}

// ListCandidates 处理 GET /api/admin/search-profiles/candidates。
func (h *Handler) ListCandidates(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	statusFilter := r.URL.Query().Get("status")
	result, err := h.profileRepo.ListCandidates(r.Context(), statusFilter, limit, offset)
	if err != nil {
		slog.Error("查询候选列表失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"candidates": result.Candidates,
		"total":      result.Total,
		"limit":      limit,
		"offset":     offset,
	})
}

// ConfirmCandidate 处理 POST /api/admin/search-profiles/candidates/{id}/confirm。
// 引入动机：design/01-SEARCH.md §Auto Tuning Level 2/3 需要管理员确认。
func (h *Handler) ConfirmCandidate(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	candidateID := r.PathValue("id")
	if candidateID == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 candidate ID")
		return
	}

	if err := h.profileRepo.ConfirmCandidate(r.Context(), candidateID, id.UserID); err != nil {
		slog.Error("确认候选失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "确认失败")
		return
	}

	slog.Info("调优候选已确认", "candidate_id", candidateID, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.candidate.confirm", "tuning_candidate", candidateID, nil)
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "confirmed"})
}

// --- Evaluation ---

// ListJobs 处理 GET /api/admin/jobs。
func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	statusFilter := r.URL.Query().Get("status")
	result, err := h.jobRepo.List(r.Context(), statusFilter, limit, offset)
	if err != nil {
		slog.Error("查询 job 列表失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"jobs":   result.Jobs,
		"total":  result.Total,
		"limit":  limit,
		"offset": offset,
	})
}

// RetryJob 处理 POST /api/admin/jobs/{id}/retry。
func (h *Handler) RetryJob(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	jobID := r.PathValue("id")
	if jobID == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 job ID")
		return
	}

	if err := h.jobRepo.Retry(r.Context(), jobID); err != nil {
		slog.Error("重试 job 失败", "error", err, "job_id", jobID)
		writeAdminError(w, http.StatusInternalServerError, "重试失败")
		return
	}

	slog.Info("job 已重试", "job_id", jobID, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.job.retry", "job", jobID, nil)
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "retrying"})
}

// rebuildRequest 是触发全量 rebuild 的请求体。
type rebuildRequest struct {
	WorkspaceID string `json:"workspace_id"`
	IndexName   string `json:"index_name"`
}

// RebuildIndex 处理 POST /api/admin/jobs/rebuild。
func (h *Handler) RebuildIndex(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req rebuildRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	// 获取 active profile 以确定 index_name
	activeProfile, err := h.profileRepo.GetActiveProfile(r.Context())
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "无 active profile")
		return
	}

	indexName := activeProfile.ESIndexName
	if req.IndexName != "" {
		indexName = req.IndexName
	}

	// Enqueue rebuild job
	payload := map[string]interface{}{
		"index_name":   indexName,
		"workspace_id": req.WorkspaceID,
	}
	_, err = h.jobRepo.Enqueue(r.Context(), types.JobRebuildIndex, payload)
	if err != nil {
		slog.Error("入队 rebuild job 失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "触发 rebuild 失败")
		return
	}

	slog.Info("rebuild job 已入队", "index_name", indexName, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.job.rebuild", "job", "", map[string]string{"index_name": indexName})
	writeAdminJSON(w, http.StatusAccepted, map[string]string{"status": "enqueued"})
}

// --- Evaluation ---

// createDatasetRequest 是创建评测数据集的请求体。
type createDatasetRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// addItemRequest 是添加评测条目的请求体。
type addItemRequest struct {
	Query             string                          `json:"query"`
	ExpectedDocuments []evaluation.ExpectedDocument   `json:"expected_documents"`
	RelevanceGrade    int                             `json:"relevance_grade"`
	QueryClass        string                          `json:"query_class"`
}

// runEvaluationRequest 是运行评测的请求体。
type runEvaluationRequest struct {
	DatasetID string `json:"dataset_id"`
	ProfileID string `json:"profile_id"`
}

// ListDatasets 处理 GET /api/admin/evaluation/datasets。
func (h *Handler) ListDatasets(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	statusFilter := r.URL.Query().Get("status")
	result, err := h.evalRepo.ListDatasets(r.Context(), statusFilter, limit, offset)
	if err != nil {
		slog.Error("查询评测数据集列表失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"datasets": result.Datasets,
		"total":    result.Total,
		"limit":    limit,
		"offset":   offset,
	})
}

// CreateDataset 处理 POST /api/admin/evaluation/datasets。
func (h *Handler) CreateDataset(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req createDatasetRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	if req.Name == "" {
		writeAdminError(w, http.StatusBadRequest, "name 不能为空")
		return
	}

	ds, err := h.evalRepo.CreateDataset(r.Context(), req.Name, req.Description, id.UserID)
	if err != nil {
		slog.Error("创建评测数据集失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "创建失败")
		return
	}

	h.recordAudit(r, id.UserID, "admin.eval.dataset.create", "evaluation_dataset", ds.ID, map[string]string{"name": req.Name})
	writeAdminJSON(w, http.StatusCreated, ds)
}

// AddItem 处理 POST /api/admin/evaluation/datasets/{id}/items。
func (h *Handler) AddItem(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	datasetID := r.PathValue("id")
	if datasetID == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 dataset ID")
		return
	}

	var req addItemRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	if req.Query == "" {
		writeAdminError(w, http.StatusBadRequest, "query 不能为空")
		return
	}
	if req.RelevanceGrade < 0 || req.RelevanceGrade > 3 {
		writeAdminError(w, http.StatusBadRequest, "relevance_grade 必须在 0..3 范围内")
		return
	}
	if req.QueryClass == "" {
		req.QueryClass = "general"
	}

	item, err := h.evalRepo.AddItem(r.Context(), datasetID, req.Query, req.ExpectedDocuments, req.RelevanceGrade, req.QueryClass)
	if err != nil {
		slog.Error("添加评测条目失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "添加失败")
		return
	}

	h.recordAudit(r, id.UserID, "admin.eval.item.add", "evaluation_item", item.ID, map[string]string{"dataset_id": datasetID})
	writeAdminJSON(w, http.StatusCreated, item)
}

// RunEvaluation 处理 POST /api/admin/evaluation/run。
// 引入动机：design/01-SEARCH.md §Evaluation 要求运行评测并计算指标。
// 评测通过入队 evaluate_profile job 异步执行：job worker 读取评测 items、
// 对指定 profile 调用搜索管线、计算 NDCG/Recall/MRR 等指标并持久化到 evaluation_results 表。
// 此处严格校验 profile/dataset 存在性，入队真实 job，返回 job id 和状态。
func (h *Handler) RunEvaluation(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	var req runEvaluationRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	if req.DatasetID == "" {
		writeAdminError(w, http.StatusBadRequest, "dataset_id 不能为空")
		return
	}
	if req.ProfileID == "" {
		writeAdminError(w, http.StatusBadRequest, "profile_id 不能为空")
		return
	}

	// 验证数据集存在
	_, err := h.evalRepo.GetDataset(r.Context(), req.DatasetID)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "数据集不存在")
		return
	}

	// 验证 profile 存在
	profileRecord, err := h.profileRepo.GetProfileByID(r.Context(), req.ProfileID)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "profile 不存在")
		return
	}

	// 入队 evaluate_profile job
	payload := map[string]interface{}{
		"dataset_id": req.DatasetID,
		"profile_id": req.ProfileID,
	}
	jobID, err := h.jobRepo.Enqueue(r.Context(), types.JobEvaluateProfile, payload)
	if err != nil {
		slog.Error("入队 evaluate_profile job 失败", "error", err, "dataset_id", req.DatasetID, "profile_id", req.ProfileID)
		writeAdminError(w, http.StatusInternalServerError, "触发评测失败")
		return
	}

	h.recordAudit(r, id.UserID, "admin.eval.run", "evaluation_dataset", req.DatasetID, map[string]string{"profile_id": req.ProfileID, "job_id": jobID})
	slog.Info("evaluation run job 已入队", "job_id", jobID, "dataset_id", req.DatasetID, "profile_id", req.ProfileID, "profile_name", profileRecord.Name, "user_id", id.UserID)
	writeAdminJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":     "enqueued",
		"job_id":     jobID,
		"dataset_id": req.DatasetID,
		"profile_id": req.ProfileID,
	})
}

// ListEvaluationResults 处理 GET /api/admin/evaluation/results。
// 引入动机：design/04-WEB-API.md §Admin 要求 evaluation 管理 API，
// 评测结果需要可查询以供管理员查看历史评测趋势。
func (h *Handler) ListEvaluationResults(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	datasetID := r.URL.Query().Get("dataset_id")
	if datasetID == "" {
		writeAdminError(w, http.StatusBadRequest, "dataset_id 查询参数不能为空")
		return
	}

	result, err := h.evalResultRepo.ListResults(r.Context(), datasetID, limit, offset)
	if err != nil {
		slog.Error("查询评测结果列表失败", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"results": result.Results,
		"total":   result.Total,
		"limit":   limit,
		"offset":  offset,
	})
}

// RegisterRoutes 注册搜索管理模块的 HTTP 路由。
//
// 引入动机：main.go 调用此函数完成 admin 路由注册。
// 所有端点仅 system_admin 可访问。
//
// 路由清单：
//   - GET  /api/admin/search-profiles — system_admin，Profile 列表
//   - POST /api/admin/search-profiles — system_admin，创建 Profile
//   - POST /api/admin/search-profiles/{id}/activate — system_admin，激活 Profile
//   - POST /api/admin/search-profiles/{id}/rollback — system_admin，回滚 Profile
//   - GET  /api/admin/search-profiles/candidates — system_admin，候选列表
//   - POST /api/admin/search-profiles/candidates/{id}/confirm — system_admin，确认候选
//   - GET  /api/admin/evaluation/datasets — system_admin，评测数据集列表
//   - POST /api/admin/evaluation/datasets — system_admin，创建数据集
//   - POST /api/admin/evaluation/datasets/{id}/items — system_admin，添加评测条目
//   - POST /api/admin/evaluation/run — system_admin，运行评测
//   - GET  /api/admin/jobs — system_admin，Job 列表
//   - POST /api/admin/jobs/{id}/retry — system_admin，重试 Job
//   - POST /api/admin/jobs/rebuild — system_admin，触发全量 rebuild
func RegisterRoutes(
	mux *http.ServeMux,
	handler *Handler,
	authRepo auth.Repository,
	authCfg auth.AuthConfig,
) {
	// GET /api/admin/search-profiles — system_admin
	mux.Handle("GET /api/admin/search-profiles",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListProfiles),
				),
			),
		),
	)

	// POST /api/admin/search-profiles — system_admin + CSRF
	mux.Handle("POST /api/admin/search-profiles",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.CreateProfile),
					),
				),
			),
		),
	)

	// POST /api/admin/search-profiles/{id}/activate — system_admin + CSRF
	mux.Handle("POST /api/admin/search-profiles/{id}/activate",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.ActivateProfile),
					),
				),
			),
		),
	)

	// POST /api/admin/search-profiles/{id}/rollback — system_admin + CSRF
	mux.Handle("POST /api/admin/search-profiles/{id}/rollback",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.RollbackProfile),
					),
				),
			),
		),
	)

	// GET /api/admin/search-profiles/candidates — system_admin
	mux.Handle("GET /api/admin/search-profiles/candidates",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListCandidates),
				),
			),
		),
	)

	// POST /api/admin/search-profiles/candidates/{id}/confirm — system_admin + CSRF
	mux.Handle("POST /api/admin/search-profiles/candidates/{id}/confirm",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.ConfirmCandidate),
					),
				),
			),
		),
	)

	// --- Evaluation ---

	// GET /api/admin/evaluation/datasets — system_admin
	mux.Handle("GET /api/admin/evaluation/datasets",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListDatasets),
				),
			),
		),
	)

	// POST /api/admin/evaluation/datasets — system_admin + CSRF
	mux.Handle("POST /api/admin/evaluation/datasets",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.CreateDataset),
					),
				),
			),
		),
	)

	// POST /api/admin/evaluation/datasets/{id}/items — system_admin + CSRF
	mux.Handle("POST /api/admin/evaluation/datasets/{id}/items",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.AddItem),
					),
				),
			),
		),
	)

	// POST /api/admin/evaluation/run — system_admin + CSRF
	mux.Handle("POST /api/admin/evaluation/run",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.RunEvaluation),
					),
				),
			),
		),
	)

	// GET /api/admin/evaluation/results — system_admin
	mux.Handle("GET /api/admin/evaluation/results",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListEvaluationResults),
				),
			),
		),
	)

	// GET /api/admin/jobs — system_admin
	mux.Handle("GET /api/admin/jobs",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListJobs),
				),
			),
		),
	)

	// POST /api/admin/jobs/{id}/retry — system_admin + CSRF
	mux.Handle("POST /api/admin/jobs/{id}/retry",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.RetryJob),
					),
				),
			),
		),
	)

	// POST /api/admin/jobs/rebuild — system_admin + CSRF
	mux.Handle("POST /api/admin/jobs/rebuild",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.RebuildIndex),
					),
				),
			),
		),
	)
}

// writeAdminError 写入统一 JSON 错误响应。
func writeAdminError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		slog.Error("写入错误响应失败", "error", err)
	}
}

// writeAdminJSON 写入 JSON 响应。
func writeAdminJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(body)
	if err != nil {
		slog.Error("序列化 JSON 响应失败", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"内部错误"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// decodeJSONStrict 严格解码 JSON 请求体。
func decodeJSONStrict(r *http.Request, dst interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体包含多个 JSON 值")
	}
	return nil
}

// parsePagination 从 URL 查询参数解析分页参数。
// 委托至 queryutil.ParsePagination，采用 Fail Fast 策略：
// 畸形 RawQuery（含 '?'）直接返回错误并记录日志，不做静默清洁。
func parsePagination(r *http.Request) (int, int, error) {
	return queryutil.ParsePagination(r)
}

// 确保 httpmw 包被使用
var _ = httpmw.RequestIDFromContext
