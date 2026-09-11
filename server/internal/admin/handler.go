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
	"database/sql"
	"encoding/json"
	"errors"
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

// validateProfileParams 校验 profile 可调参数的核心约束。
//
// 引入动机：CreateProfile 与 CreateProfileVersion 必须对同一组参数施加同一套约束口径，
// 否则会出现"创建时合法、建新版本时非法"（或反之）的漂移。抽出单一实现，两个 handler 共用。
//
// 返回值即 HTTP 400 的响应体，调用方直接透出；文案与既有 CreateProfile 校验保持一致，
// 避免前端按错误文案分支时出现两套口径。
func validateProfileParams(name string, embeddingDimensions, lexicalTopK, vectorTopK, rrfK int) error {
	if name == "" {
		return errors.New("name 不能为空")
	}
	if embeddingDimensions <= 0 {
		return errors.New("embedding_dimensions 必须为正整数")
	}
	if lexicalTopK <= 0 || vectorTopK <= 0 || rrfK <= 0 {
		return errors.New("top_k 和 rrf_k 必须为正整数")
	}
	return nil
}

// applyOverride 在请求提供了覆盖值时改写目标字段；override 为 nil 表示继承，不做修改。
//
// 引入动机：新建版本允许"部分覆盖"，必须能区分"未提供（继承源 profile）"与"显式传零值"
// （例如把 title_boost 覆盖为 0、把 merge_adjacent_chunks 覆盖为 false），
// 因此请求体字段用指针表达并在此统一合并。
func applyOverride[T any](dst *T, override *T) {
	if override != nil {
		*dst = *override
	}
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

	if err := validateProfileParams(req.Name, req.EmbeddingDimensions, req.LexicalTopK, req.VectorTopK, req.RRFK); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
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

// createProfileVersionRequest 是"新建 profile 版本"的请求体，所有可调参数均可选。
//
// 引入动机：需求要求以指定 profile 为基底复制全部可调参数，并允许局部覆盖。
// 因此除 name 外的字段全部使用指针：nil 表示"未提供，继承源 profile"，非 nil 表示"显式覆盖"。
//
// 设计约束：
//   - name 为普通 string：为空时继承源 profile 的名称。源 profile 的 name 在数据库中非空，
//     因此最终 name 永远不会为空，不会绕过 validateProfileParams 的"name 不能为空"约束。
//   - version/status/es_index_name/created_by/created_at/activated_at 不接受客户端指定：
//     它们不在本结构体中；decodeJSONStrict 开启 DisallowUnknownFields，
//     请求体出现这些字段会直接返回 400，而不会静默忽略。
type createProfileVersionRequest struct {
	Name                      string   `json:"name"`
	EmbeddingProvider         *string  `json:"embedding_provider"`
	EmbeddingModel            *string  `json:"embedding_model"`
	EmbeddingDimensions       *int     `json:"embedding_dimensions"`
	EmbeddingQueryInstruction *string  `json:"embedding_query_instruction"`
	EmbeddingDocInstruction   *string  `json:"embedding_document_instruction"`
	ChunkTargetSize           *int     `json:"chunk_target_size"`
	ChunkOverlap              *int     `json:"chunk_overlap"`
	TitleBoost                *float32 `json:"title_boost"`
	HeadingBoost              *float32 `json:"heading_boost"`
	PathBoost                 *float32 `json:"path_boost"`
	TagsBoost                 *float32 `json:"tags_boost"`
	BodyBoost                 *float32 `json:"body_boost"`
	Analyzer                  *string  `json:"analyzer"`
	LexicalTopK               *int     `json:"lexical_top_k"`
	VectorTopK                *int     `json:"vector_top_k"`
	RRFK                      *int     `json:"rrf_k"`
	RerankerProvider          *string  `json:"reranker_provider"`
	RerankerModel             *string  `json:"reranker_model"`
	RerankerCandidateCount    *int     `json:"reranker_candidate_count"`
	RerankerFinalCount        *int     `json:"reranker_final_count"`
	MaxChunksPerDocument      *int     `json:"max_chunks_per_document"`
	MergeAdjacentChunks       *bool    `json:"merge_adjacent_chunks"`
	MaxP95LatencyMs           *int     `json:"max_p95_latency_ms"`
	MaxRerankerCostPerQuery   *float32 `json:"max_reranker_cost_per_query"`
}

// CreateProfileVersion 处理 POST /api/admin/search-profiles/{id}/versions。
//
// 引入动机：Search Profile 是版本化的（UNIQUE (name, version)），调参必须在不影响当前
// active profile 的前提下产出新版本草稿，因此需要"以源 profile 为基底复制参数 + 允许局部覆盖"，
// 并复用 CreateProfile 的服务端语义（version = 同名最大版本 + 1、es_index_name 自动生成、
// status 固定为 draft）。
//
// 设计约束：
//   - 绝不自动激活：本 handler 不调用 ActivateProfile，也不入队 rebuild_index job；
//     新版本要生效必须由管理员显式调用 activate 端点，避免一次调参请求意外切换线上配置。
//   - created_by 记录当前认证用户，而不是源 profile 的创建者：新版本的责任人是本次操作者。
func (h *Handler) CreateProfileVersion(w http.ResponseWriter, r *http.Request) {
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

	var req createProfileVersionRequest
	if err := decodeJSONStrict(r, &req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("请求体解析失败: %v", err))
		return
	}

	// 源 profile 必须存在：不存在就无从继承参数，返回 404。
	src, err := h.profileRepo.GetProfileByID(r.Context(), profileID)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "profile 不存在")
		return
	}

	// 先完整继承源 profile 的全部可调参数。
	input := &profile.CreateProfileInput{
		Name:                      src.Name,
		EmbeddingProvider:         src.EmbeddingProvider,
		EmbeddingModel:            src.EmbeddingModel,
		EmbeddingDimensions:       src.EmbeddingDimensions,
		EmbeddingQueryInstruction: src.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   src.EmbeddingDocInstruction,
		ChunkTargetSize:           src.ChunkTargetSize,
		ChunkOverlap:              src.ChunkOverlap,
		TitleBoost:                src.TitleBoost,
		HeadingBoost:              src.HeadingBoost,
		PathBoost:                 src.PathBoost,
		TagsBoost:                 src.TagsBoost,
		BodyBoost:                 src.BodyBoost,
		Analyzer:                  src.Analyzer,
		LexicalTopK:               src.LexicalTopK,
		VectorTopK:                src.VectorTopK,
		RRFK:                      src.RRFK,
		RerankerProvider:          src.RerankerProvider,
		RerankerModel:             src.RerankerModel,
		RerankerCandidateCount:    src.RerankerCandidateCount,
		RerankerFinalCount:        src.RerankerFinalCount,
		MaxChunksPerDocument:      src.MaxChunksPerDocument,
		MergeAdjacentChunks:       src.MergeAdjacentChunks,
		MaxP95LatencyMs:           src.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   src.MaxRerankerCostPerQuery,
		CreatedBy:                 id.UserID,
	}

	// 再按请求逐字段覆盖：nil = 继承。
	if req.Name != "" {
		input.Name = req.Name
	}
	applyOverride(&input.EmbeddingProvider, req.EmbeddingProvider)
	applyOverride(&input.EmbeddingModel, req.EmbeddingModel)
	applyOverride(&input.EmbeddingDimensions, req.EmbeddingDimensions)
	applyOverride(&input.EmbeddingQueryInstruction, req.EmbeddingQueryInstruction)
	applyOverride(&input.EmbeddingDocInstruction, req.EmbeddingDocInstruction)
	applyOverride(&input.ChunkTargetSize, req.ChunkTargetSize)
	applyOverride(&input.ChunkOverlap, req.ChunkOverlap)
	applyOverride(&input.TitleBoost, req.TitleBoost)
	applyOverride(&input.HeadingBoost, req.HeadingBoost)
	applyOverride(&input.PathBoost, req.PathBoost)
	applyOverride(&input.TagsBoost, req.TagsBoost)
	applyOverride(&input.BodyBoost, req.BodyBoost)
	applyOverride(&input.Analyzer, req.Analyzer)
	applyOverride(&input.LexicalTopK, req.LexicalTopK)
	applyOverride(&input.VectorTopK, req.VectorTopK)
	applyOverride(&input.RRFK, req.RRFK)
	applyOverride(&input.RerankerProvider, req.RerankerProvider)
	applyOverride(&input.RerankerModel, req.RerankerModel)
	applyOverride(&input.RerankerCandidateCount, req.RerankerCandidateCount)
	applyOverride(&input.RerankerFinalCount, req.RerankerFinalCount)
	applyOverride(&input.MaxChunksPerDocument, req.MaxChunksPerDocument)
	applyOverride(&input.MergeAdjacentChunks, req.MergeAdjacentChunks)
	applyOverride(&input.MaxP95LatencyMs, req.MaxP95LatencyMs)
	applyOverride(&input.MaxRerankerCostPerQuery, req.MaxRerankerCostPerQuery)

	// 与 CreateProfile 复用同一套校验，保证"覆盖后的参数"不会绕过既有约束。
	if err := validateProfileParams(input.Name, input.EmbeddingDimensions, input.LexicalTopK, input.VectorTopK, input.RRFK); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 复用 CreateProfile：version、es_index_name 由仓储层计算，status 固定为 draft。
	p, err := h.profileRepo.CreateProfile(r.Context(), input)
	if err != nil {
		slog.Error("创建 profile 新版本失败", "error", err, "source_profile_id", profileID)
		writeAdminError(w, http.StatusInternalServerError, "创建失败")
		return
	}

	slog.Info("search profile 新版本已创建",
		"source_profile_id", profileID, "id", p.ID, "name", p.Name, "version", p.Version, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.profile.create_version", "search_profile", p.ID,
		map[string]string{"source_profile_id": profileID, "name": p.Name})
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

// ArchiveProfile 处理 POST /api/admin/search-profiles/{id}/archive。
//
// 引入动机：Search Profile 需要可逆的下线操作——归档保留记录但不再参与搜索，
// 与既有"回滚 = 激活 archived profile"自洽，因此归档不是删除。
//
// 设计约束：status='active' 的 profile 不允许归档，归档 active 会让系统失去当前生效配置。
// 该约束在两处实现，职责不同：
//   - handler 先读取 profile 给出清晰的 409 语义（不依赖竞态）；
//   - 仓储层的带守卫 UPDATE 抵御并发（读取后状态被其它事务改为 active）。
//
// 本接口不接收请求体，与 activate/rollback 一致，id 取自路径参数。
func (h *Handler) ArchiveProfile(w http.ResponseWriter, r *http.Request) {
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

	p, err := h.profileRepo.GetProfileByID(r.Context(), profileID)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "profile 不存在")
		return
	}

	if p.Status == "active" {
		writeAdminError(w, http.StatusConflict, "active profile 不能归档，请先激活其它 profile")
		return
	}

	if err := h.profileRepo.ArchiveProfile(r.Context(), profileID); err != nil {
		// 仓储层守卫在并发下可能拒绝归档（状态已被其它事务改为 active，或记录已被删除）。
		// 这类拒绝必须如实返回 409，而不是伪装成 500 内部错误。
		if errors.Is(err, profile.ErrProfileNotArchivable) {
			writeAdminError(w, http.StatusConflict, "active profile 不能归档，请先激活其它 profile")
			return
		}
		slog.Error("归档 profile 失败", "error", err, "profile_id", profileID)
		writeAdminError(w, http.StatusInternalServerError, "归档失败")
		return
	}

	slog.Info("search profile 已归档", "profile_id", profileID, "name", p.Name, "user_id", id.UserID)
	h.recordAudit(r, id.UserID, "admin.profile.archive", "search_profile", profileID, map[string]string{"name": p.Name})
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "archived"})
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

	// 软删语义校验：数据集不存在返回 404，archived 数据集不再接受新条目返回 409。
	// 引入动机：数据集删除已改为归档（status='archived'），归档数据集的历史条目与评测结果
	// 被保留用于审计，若仍允许写入新条目会混淆"已归档"语义并让历史评测与新增条目混杂。
	ds, err := h.evalRepo.GetDataset(r.Context(), datasetID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeAdminError(w, http.StatusNotFound, "数据集不存在")
			return
		}
		slog.Error("查询评测数据集失败", "error", err, "dataset_id", datasetID)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if ds.Status == evaluation.DatasetStatusArchived {
		writeAdminError(w, http.StatusConflict, "数据集已归档，不能新增条目")
		return
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

// ListItems 处理 GET /api/admin/evaluation/datasets/{id}/items。
// 引入动机：前端审计发现管理端缺少"查看数据集条目"端点，无法核对条目内容。
// 返回 items + total + limit + offset，与既有列表端点契约一致。
func (h *Handler) ListItems(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := parsePagination(r)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}

	datasetID := r.PathValue("id")
	if datasetID == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 dataset ID")
		return
	}

	result, err := h.evalRepo.ListItems(r.Context(), datasetID, limit, offset)
	if err != nil {
		slog.Error("查询评测条目列表失败", "error", err, "dataset_id", datasetID)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}

	writeAdminJSON(w, http.StatusOK, map[string]interface{}{
		"items":  result.Items,
		"total":  result.Total,
		"limit":  limit,
		"offset": offset,
	})
}

// DeleteItem 处理 DELETE /api/admin/evaluation/datasets/{id}/items/{itemId}。
// 引入动机：前端审计发现管理端缺少条目删除能力。
// 仓储层 WHERE 同时限定 itemId 与 datasetID，防止仅凭条目 ID 越权删除其它数据集的条目。
func (h *Handler) DeleteItem(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeAdminError(w, http.StatusUnauthorized, "未认证")
		return
	}

	datasetID := r.PathValue("id")
	itemID := r.PathValue("itemId")
	if datasetID == "" || itemID == "" {
		writeAdminError(w, http.StatusBadRequest, "缺少 dataset ID 或 item ID")
		return
	}

	if err := h.evalRepo.DeleteItem(r.Context(), datasetID, itemID); err != nil {
		if err == sql.ErrNoRows {
			writeAdminError(w, http.StatusNotFound, "条目不存在")
			return
		}
		slog.Error("删除评测条目失败", "error", err, "dataset_id", datasetID, "item_id", itemID)
		writeAdminError(w, http.StatusInternalServerError, "删除失败")
		return
	}

	h.recordAudit(r, id.UserID, "admin.eval.item.delete", "evaluation_item", itemID, map[string]string{"dataset_id": datasetID})
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// DeleteDataset 处理 DELETE /api/admin/evaluation/datasets/{id}。
// 引入动机：前端审计发现管理端缺少数据集删除能力。
//
// 软删语义：DELETE 端点执行归档而非物理删除——evaluation_items.dataset_id 与
// evaluation_results.dataset_id 均为 ON DELETE CASCADE，物理 DELETE 会连带、
// 不可恢复地删除历史评测结果。归档仅将 status 置为 'archived'，
// 条目与评测历史保留用于审计；归档后数据集拒绝新增条目与运行评测（见 AddItem/RunEvaluation）。
// 归档是 API 层终态：不提供恢复端点，需恢复时只能由 DBA 直接 UPDATE status。
func (h *Handler) DeleteDataset(w http.ResponseWriter, r *http.Request) {
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

	if err := h.evalRepo.ArchiveDataset(r.Context(), datasetID); err != nil {
		if err == sql.ErrNoRows {
			writeAdminError(w, http.StatusNotFound, "数据集不存在")
			return
		}
		slog.Error("归档评测数据集失败", "error", err, "dataset_id", datasetID)
		writeAdminError(w, http.StatusInternalServerError, "归档失败")
		return
	}

	h.recordAudit(r, id.UserID, "admin.eval.dataset.archive", "evaluation_dataset", datasetID, nil)
	writeAdminJSON(w, http.StatusOK, map[string]string{"status": "archived"})
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
	ds, err := h.evalRepo.GetDataset(r.Context(), req.DatasetID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeAdminError(w, http.StatusNotFound, "数据集不存在")
			return
		}
		slog.Error("查询评测数据集失败", "error", err, "dataset_id", req.DatasetID)
		writeAdminError(w, http.StatusInternalServerError, "查询失败")
		return
	}
	// archived 数据集不再参与评测：归档是 API 层终态，历史评测结果只读保留，
	// 继续对归档数据集跑评测会产生与"已归档"语义矛盾的新结果。
	if ds.Status == evaluation.DatasetStatusArchived {
		writeAdminError(w, http.StatusConflict, "数据集已归档，不能运行评测")
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
//   - POST /api/admin/search-profiles/{id}/versions — system_admin，以指定 Profile 为基底新建版本
//   - POST /api/admin/search-profiles/{id}/activate — system_admin，激活 Profile
//   - POST /api/admin/search-profiles/{id}/archive — system_admin，归档 Profile（active 不可归档）
//   - POST /api/admin/search-profiles/{id}/rollback — system_admin，回滚 Profile
//   - GET  /api/admin/search-profiles/candidates — system_admin，候选列表
//   - POST /api/admin/search-profiles/candidates/{id}/confirm — system_admin，确认候选
//   - GET  /api/admin/evaluation/datasets — system_admin，评测数据集列表
//   - POST /api/admin/evaluation/datasets — system_admin，创建数据集
//   - DELETE /api/admin/evaluation/datasets/{id} — system_admin，归档数据集（软删，保留条目与评测历史）
//   - GET  /api/admin/evaluation/datasets/{id}/items — system_admin，评测条目列表
//   - POST /api/admin/evaluation/datasets/{id}/items — system_admin，添加评测条目
//   - DELETE /api/admin/evaluation/datasets/{id}/items/{itemId} — system_admin，删除评测条目
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

	// POST /api/admin/search-profiles/{id}/versions — system_admin + CSRF
	mux.Handle("POST /api/admin/search-profiles/{id}/versions",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.CreateProfileVersion),
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

	// POST /api/admin/search-profiles/{id}/archive — system_admin + CSRF
	mux.Handle("POST /api/admin/search-profiles/{id}/archive",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.ArchiveProfile),
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

	// GET /api/admin/evaluation/datasets/{id}/items — system_admin
	mux.Handle("GET /api/admin/evaluation/datasets/{id}/items",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				workspace.RequireSystemAdmin(
					http.HandlerFunc(handler.ListItems),
				),
			),
		),
	)

	// DELETE /api/admin/evaluation/datasets/{id}/items/{itemId} — system_admin + CSRF
	mux.Handle("DELETE /api/admin/evaluation/datasets/{id}/items/{itemId}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.DeleteItem),
					),
				),
			),
		),
	)

	// DELETE /api/admin/evaluation/datasets/{id} — system_admin + CSRF
	mux.Handle("DELETE /api/admin/evaluation/datasets/{id}",
		auth.AuthMiddleware(authRepo, authCfg)(
			auth.RequireAuth(
				auth.RequireCSRF(authRepo, authCfg)(
					workspace.RequireSystemAdmin(
						http.HandlerFunc(handler.DeleteDataset),
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
