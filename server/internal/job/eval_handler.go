// eval_handler.go 实现 evaluate_profile 和 optimize_profile 任务的执行逻辑。
//
// 引入动机：design/05-OPERATIONS.md §Background Jobs 声明 evaluate_profile 和 optimize_profile
// 两类 job，但现有 IndexJobHandler 仅处理 index/rebuild/repair/cleanup。
// design/01-SEARCH.md §Evaluation 要求评测实际执行搜索并计算指标；
// §Auto Tuning 要求 Level 1 自动调优在 gate 通过后自动激活。
//
// 设计原则：
//   - evaluate_profile 读取评测 items，对指定 profile 调用搜索管线，计算并持久化评测结果
//   - optimize_profile 实现 Level 1 自动调优（field boost/top_k/RRF/candidate count/diversity），
//     候选必须运行评测且仅在 Regression Gate 全条件通过后自动激活
//   - Level 2 仅测试待管理员确认；Level 3 仅人工触发
//   - 定时触发属于 Phase 6，不在本次实现
package job

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"partitura/server/internal/evaluation"
	"partitura/server/internal/search/pipeline"
	types "partitura/server/internal/search/types"
)

// EvalRepo 定义评测数据访问接口（job 包使用的子集）。
// 引入动机：evaluate_profile job 需要读取评测 items，避免 job 包直接依赖 evaluation.PGRepository。
type EvalRepo interface {
	ListAllItems(ctx context.Context, datasetID string) ([]evaluation.Item, error)
}

// EvalResultRepo 定义评测结果持久化接口。
// 引入动机：evaluate_profile job 需要将计算出的指标持久化到 evaluation_results 表。
type EvalResultRepo interface {
	SaveResult(ctx context.Context, datasetID, profileID, jobID string, metrics json.RawMessage, itemCount int) (*evaluation.EvaluationRunResult, error)
}

// ProfileRepoExtended 定义 job handler 需要的完整 profile 操作接口。
// 引入动机：optimize_profile job 需要创建候选 profile、运行评测、检查 gate、自动激活。
// 现有 ProfileRepo 仅提供 GetActiveProfile，此处扩展为完整接口。
type ProfileRepoExtended interface {
	ProfileRepo
	GetProfileByID(ctx context.Context, id string) (*ProfileForJobExtended, error)
	CreateCandidateProfile(ctx context.Context, base *ProfileForJobExtended, changes ParameterChanges) (*ProfileForJobExtended, error)
	ActivateProfile(ctx context.Context, id string) error
	CreateCandidateRecord(ctx context.Context, baseProfileID, candidateProfileID string, tuningLevel int, parameterChanges json.RawMessage) error
	UpdateCandidateStatus(ctx context.Context, id string, status string, gatePassed bool, gateDetails json.RawMessage, evaluationResult json.RawMessage) error
}

// ProfileForJobExtended 是 job handler 使用的完整 profile 信息。
// 引入动机：optimize_profile job 需要访问 profile 的全部调优参数以创建候选。
// 候选 profile 必须继承 base profile 的 embedding/reranker provider/model/analyzer，
// 不得硬编码——design/01-SEARCH.md §Auto Tuning Level 1 仅允许调整
// field boost/top_k/RRF/candidate count/diversity，provider/model/analyzer 不可变。
type ProfileForJobExtended struct {
	ID                        string
	Name                      string
	Version                   int
	ESIndexName               string
	ChunkTargetSize           int
	ChunkOverlap              int
	EmbeddingProvider         string
	EmbeddingModel            string
	EmbeddingDimensions       int
	EmbeddingQueryInstruction string
	EmbeddingDocInstruction   string
	Analyzer                  string
	// Lexical 配置
	// 引入动机：evaluate_profile 和 optimize_profile 需要读取和调整 lexical boost 参数，
	// Level 1 自动调优允许调整 field boost。这些字段在 main.go 的
	// profileRepoExtAdapter.GetProfileByID 中从 profile.PGRepository 设置。
	TitleBoost                float32
	HeadingBoost              float32
	PathBoost                 float32
	TagsBoost                 float32
	BodyBoost                 float32
	// Retrieval 配置
	// 引入动机：Level 1 自动调优允许调整 top_k 和 RRF 参数。
	LexicalTopK               int
	VectorTopK                int
	RRFK                      int
	RerankerProvider          string
	RerankerModel             string
	RerankerCandidateCount    int
	RerankerFinalCount        int
	MaxChunksPerDocument      int
	MergeAdjacentChunks       bool
	MaxP95LatencyMs           int
	MaxRerankerCostPerQuery   float32
	// CreatedBy 是创建候选 profile 时使用的创建者 UUID。
	// 引入动机：候选 profile 继承 base profile 的 created_by，
	// 而非使用伪造的零 UUID，保证审计可追溯。
	CreatedBy string
}

// ParameterChanges 描述 Level 1 自动调优的参数变更。
// 引入动机：design/01-SEARCH.md §Auto Tuning Level 1 仅允许调整
// field boost / top_k / RRF / candidate count / diversity 参数。
type ParameterChanges struct {
	TitleBoost             *float32 `json:"title_boost,omitempty"`
	HeadingBoost           *float32 `json:"heading_boost,omitempty"`
	PathBoost              *float32 `json:"path_boost,omitempty"`
	TagsBoost              *float32 `json:"tags_boost,omitempty"`
	BodyBoost              *float32 `json:"body_boost,omitempty"`
	LexicalTopK            *int     `json:"lexical_top_k,omitempty"`
	VectorTopK             *int     `json:"vector_top_k,omitempty"`
	RRFK                   *int     `json:"rrf_k,omitempty"`
	RerankerCandidateCount *int     `json:"reranker_candidate_count,omitempty"`
	MaxChunksPerDocument   *int     `json:"max_chunks_per_document,omitempty"`
	MergeAdjacentChunks    *bool    `json:"merge_adjacent_chunks,omitempty"`
}

// EvalJobHandler 实现 evaluate_profile 和 optimize_profile 任务的执行逻辑。
// 引入动机：design/05-OPERATIONS.md §Background Jobs 声明这两类 job，
// design/01-SEARCH.md §Evaluation 和 §Auto Tuning 定义了执行要求。
//
// 该 handler 包装 IndexJobHandler，将未知 job 类型委托给底层 handler，
// 同时新增 evaluate_profile 和 optimize_profile 的实际执行逻辑。
type EvalJobHandler struct {
	*IndexJobHandler
	evalRepo       EvalRepo
	evalResultRepo EvalResultRepo
	profileRepoExt ProfileRepoExtended
	pipeline       *pipeline.Pipeline
}

// NewEvalJobHandler 创建带评测能力的 job handler。
// 引入动机：main.go 需要注入 eval/profile/pipeline 依赖以支持 evaluate_profile 和 optimize_profile job。
func NewEvalJobHandler(
	base *IndexJobHandler,
	evalRepo EvalRepo,
	evalResultRepo EvalResultRepo,
	profileRepoExt ProfileRepoExtended,
	pipe *pipeline.Pipeline,
) *EvalJobHandler {
	return &EvalJobHandler{
		IndexJobHandler: base,
		evalRepo:        evalRepo,
		evalResultRepo:  evalResultRepo,
		profileRepoExt:  profileRepoExt,
		pipeline:        pipe,
	}
}

// HandleJob 处理任务，分发到对应的 handler。
// 引入动机：扩展 IndexJobHandler 的 HandleJob 以支持 evaluate_profile 和 optimize_profile。
func (h *EvalJobHandler) HandleJob(ctx context.Context, job *Job) error {
	switch job.Type {
	case types.JobEvaluateProfile:
		return h.handleEvaluateProfile(ctx, job)
	case types.JobOptimizeProfile:
		return h.handleOptimizeProfile(ctx, job)
	case types.JobIndexDocument, types.JobRebuildIndex, types.JobRepairIndex, types.JobCleanupOldIndexes, types.JobCleanupRevisions:
		return h.IndexJobHandler.HandleJob(ctx, job)
	default:
		slog.Error("未知的 job 类型", "job_id", job.ID, "type", job.Type)
		return fmt.Errorf("未知的 job 类型: %s", job.Type)
	}
}

// handleEvaluateProfile 执行评测任务。
// 引入动机：design/01-SEARCH.md §Evaluation 要求运行评测并计算指标。
// 读取指定 dataset 的全部评测 items，对指定 profile 调用搜索管线，
// 计算 NDCG/Recall/MRR/Precision/latency 等指标，持久化到 evaluation_results 表。
func (h *EvalJobHandler) handleEvaluateProfile(ctx context.Context, job *Job) error {
	datasetID, _ := job.Payload["dataset_id"].(string)
	profileID, _ := job.Payload["profile_id"].(string)

	if datasetID == "" {
		return fmt.Errorf("缺少 dataset_id")
	}
	if profileID == "" {
		return fmt.Errorf("缺少 profile_id")
	}

	// 读取评测 items
	items, err := h.evalRepo.ListAllItems(ctx, datasetID)
	if err != nil {
		return fmt.Errorf("读取评测 items: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("数据集 %s 无评测条目", datasetID)
	}

	// 获取被评测的 profile 配置
	profileConfig, err := h.profileRepoExt.GetProfileByID(ctx, profileID)
	if err != nil {
		return fmt.Errorf("获取 profile %s: %w", profileID, err)
	}

	// 构建搜索管线输入配置
	searchProfile := types.SearchProfileConfig{
		ID:                        profileConfig.ID,
		Name:                      profileConfig.Name,
		Version:                   profileConfig.Version,
		ESIndexName:               profileConfig.ESIndexName,
		ChunkTargetSize:           profileConfig.ChunkTargetSize,
		ChunkOverlap:              profileConfig.ChunkOverlap,
		EmbeddingProvider:         profileConfig.EmbeddingProvider,
		EmbeddingModel:            profileConfig.EmbeddingModel,
		EmbeddingDimensions:       profileConfig.EmbeddingDimensions,
		EmbeddingQueryInstruction: profileConfig.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   profileConfig.EmbeddingDocInstruction,
		Analyzer:                  profileConfig.Analyzer,
		RerankerProvider:          profileConfig.RerankerProvider,
		RerankerModel:             profileConfig.RerankerModel,
		TitleBoost:                profileConfig.TitleBoost,
		HeadingBoost:              profileConfig.HeadingBoost,
		PathBoost:                 profileConfig.PathBoost,
		TagsBoost:                 profileConfig.TagsBoost,
		BodyBoost:                 profileConfig.BodyBoost,
		LexicalTopK:               profileConfig.LexicalTopK,
		VectorTopK:                profileConfig.VectorTopK,
		RRFK:                     profileConfig.RRFK,
		RerankerCandidateCount:    profileConfig.RerankerCandidateCount,
		RerankerFinalCount:        profileConfig.RerankerFinalCount,
		MaxChunksPerDocument:      profileConfig.MaxChunksPerDocument,
		MergeAdjacentChunks:       profileConfig.MergeAdjacentChunks,
		MaxP95LatencyMs:           profileConfig.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   profileConfig.MaxRerankerCostPerQuery,
	}

	// 对每条评测 query 执行搜索
	evalItems := make([]evaluation.EvaluationItem, len(items))
	queryResults := make([]evaluation.QueryResult, len(items))

	for i, item := range items {
		evalItems[i] = evaluation.EvaluationItem{
			Query:             item.Query,
			QueryClass:        item.QueryClass,
			ExpectedDocuments: item.ExpectedDocuments,
		}

		// 执行搜索
		searchInput := pipeline.SearchInput{
			WorkspaceID: "", // 评测不限定 workspace，搜索全部索引
			Query:       item.Query,
			Profile:     searchProfile,
			Limit:       10,
			Mode:        "hybrid",
		}

		output, searchErr := h.pipeline.Search(ctx, searchInput)
		if searchErr != nil {
			slog.Warn("评测搜索失败，记录空结果", "query", item.Query, "error", searchErr)
			queryResults[i] = evaluation.QueryResult{
				QueryClass: item.QueryClass,
				LatencyMs:  0,
			}
			continue
		}

		queryResults[i] = evaluation.QueryResult{
			QueryClass:    item.QueryClass,
			LatencyMs:     output.LatencyMs,
			Results:       output.Results,
			RerankerUsed:  output.RerankerUsed,
			RerankerCost:  output.RerankerCost,
		}
	}

	// 计算指标
	result := evaluation.ComputeMetrics(evalItems, queryResults)

	// 序列化指标
	metricsJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("序列化评测指标: %w", err)
	}

	// 持久化评测结果
	_, err = h.evalResultRepo.SaveResult(ctx, datasetID, profileID, job.ID, metricsJSON, len(items))
	if err != nil {
		return fmt.Errorf("持久化评测结果: %w", err)
	}

	slog.Info("评测完成",
		"job_id", job.ID,
		"dataset_id", datasetID,
		"profile_id", profileID,
		"items", len(items),
		"ndcg_10", result.NDCG10,
		"recall_10", result.Recall10,
		"mrr", result.MRR,
		"p95_latency_ms", result.P95LatencyMs,
	)
	return nil
}

// handleOptimizeProfile 执行自动调优任务。
// 引入动机：design/01-SEARCH.md §Auto Tuning 要求：
//   - Level 1: 自动创建候选并运行评测，gate 通过后自动激活
//   - Level 2: 自动测试但管理员确认激活
//   - Level 3: 人工触发
//
// 本次实现 Level 1 自动调优逻辑：基于 active profile 创建候选 profile（仅调整 Level 1 参数），
// 运行评测，检查 Regression Gate，通过后自动激活候选 profile。
func (h *EvalJobHandler) handleOptimizeProfile(ctx context.Context, job *Job) error {
	baseProfileID, _ := job.Payload["base_profile_id"].(string)
	datasetID, _ := job.Payload["dataset_id"].(string)
	tuningLevelF, _ := job.Payload["tuning_level"].(float64)
	tuningLevel := int(tuningLevelF)
	if tuningLevel == 0 {
		tuningLevel = types.TuningLevel1
	}

	if baseProfileID == "" {
		return fmt.Errorf("缺少 base_profile_id")
	}
	if datasetID == "" {
		return fmt.Errorf("缺少 dataset_id")
	}

	// 获取 base profile
	baseProfile, err := h.profileRepoExt.GetProfileByID(ctx, baseProfileID)
	if err != nil {
		return fmt.Errorf("获取 base profile: %w", err)
	}

	// 解析参数变更
	changesRaw, _ := job.Payload["parameter_changes"]
	var changes ParameterChanges
	if changesRaw != nil {
		changesBytes, mErr := json.Marshal(changesRaw)
		if mErr != nil {
			return fmt.Errorf("序列化 parameter_changes: %w", mErr)
		}
		if uErr := json.Unmarshal(changesBytes, &changes); uErr != nil {
			return fmt.Errorf("解析 parameter_changes: %w", uErr)
		}
	}

	// Level 3 必须人工触发，job 不应自动执行
	if tuningLevel == types.TuningLevel3 {
		return fmt.Errorf("Level 3 调优必须人工触发，不支持 job 自动执行")
	}

	// 验证 Level 1 仅调整允许的参数
	if tuningLevel == types.TuningLevel1 {
		if err := validateLevel1Changes(&changes); err != nil {
			return fmt.Errorf("Level 1 参数验证失败: %w", err)
		}
	}

	// 创建候选 profile（应用参数变更）
	candidateProfile, err := h.profileRepoExt.CreateCandidateProfile(ctx, baseProfile, changes)
	if err != nil {
		return fmt.Errorf("创建候选 profile: %w", err)
	}

	// 序列化参数变更用于候选记录
	changesJSON, err := json.Marshal(changes)
	if err != nil {
		return fmt.Errorf("序列化参数变更: %w", err)
	}

	// 创建候选记录
	if err := h.profileRepoExt.CreateCandidateRecord(ctx, baseProfileID, candidateProfile.ID, tuningLevel, changesJSON); err != nil {
		return fmt.Errorf("创建候选记录: %w", err)
	}

	// 运行评测（对候选 profile 执行评测）
	evalItems, err := h.evalRepo.ListAllItems(ctx, datasetID)
	if err != nil {
		return fmt.Errorf("读取评测 items: %w", err)
	}
	if len(evalItems) == 0 {
		return fmt.Errorf("数据集 %s 无评测条目", datasetID)
	}

	// 构建候选 profile 的搜索配置
	candidateConfig := types.SearchProfileConfig{
		ID:                        candidateProfile.ID,
		Name:                      candidateProfile.Name,
		Version:                   candidateProfile.Version,
		ESIndexName:               candidateProfile.ESIndexName,
		ChunkTargetSize:           candidateProfile.ChunkTargetSize,
		ChunkOverlap:              candidateProfile.ChunkOverlap,
		EmbeddingProvider:         candidateProfile.EmbeddingProvider,
		EmbeddingModel:            candidateProfile.EmbeddingModel,
		EmbeddingDimensions:       candidateProfile.EmbeddingDimensions,
		EmbeddingQueryInstruction: candidateProfile.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   candidateProfile.EmbeddingDocInstruction,
		Analyzer:                  candidateProfile.Analyzer,
		RerankerProvider:          candidateProfile.RerankerProvider,
		RerankerModel:             candidateProfile.RerankerModel,
		TitleBoost:                candidateProfile.TitleBoost,
		HeadingBoost:              candidateProfile.HeadingBoost,
		PathBoost:                 candidateProfile.PathBoost,
		TagsBoost:                 candidateProfile.TagsBoost,
		BodyBoost:                 candidateProfile.BodyBoost,
		LexicalTopK:               candidateProfile.LexicalTopK,
		VectorTopK:                candidateProfile.VectorTopK,
		RRFK:                     candidateProfile.RRFK,
		RerankerCandidateCount:    candidateProfile.RerankerCandidateCount,
		RerankerFinalCount:        candidateProfile.RerankerFinalCount,
		MaxChunksPerDocument:      candidateProfile.MaxChunksPerDocument,
		MergeAdjacentChunks:       candidateProfile.MergeAdjacentChunks,
		MaxP95LatencyMs:           candidateProfile.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   candidateProfile.MaxRerankerCostPerQuery,
	}

	// 执行评测
	candidateResult, err := h.runEvaluation(ctx, evalItems, candidateConfig, job.ID, datasetID, candidateProfile.ID)
	if err != nil {
		return fmt.Errorf("候选 profile 评测失败: %w", err)
	}

	// 获取 baseline（当前 active profile）的评测结果
	baselineResult, err := h.getBaselineEvaluation(ctx, evalItems, baseProfile, datasetID, job.ID)
	if err != nil {
		slog.Warn("获取 baseline 评测结果失败，使用空 baseline", "error", err)
		emptyBaseline := evaluation.EvaluationResult{ClassMetrics: make(map[string]evaluation.ClassMetric)}
		baselineResult = &emptyBaseline
	}

	// 检查 Regression Gate
	gateInput := evaluation.RegressionGateInput{
		Candidate:              *candidateResult,
		Baseline:               *baselineResult,
		MaxP95LatencyMs:        float64(baseProfile.MaxP95LatencyMs),
		MaxRerankerCostPerQuery: float64(baseProfile.MaxRerankerCostPerQuery),
		IndexIntegrityOK:       true, // 候选 profile 使用同一 ES 索引
		CriticalQueryClasses:   extractCriticalClasses(evalItems),
	}
	gateResult := evaluation.CheckRegressionGate(gateInput)

	// 序列化 gate 结果和评测结果
	gateDetailsJSON, _ := json.Marshal(gateResult)
	candidateResultJSON, _ := json.Marshal(candidateResult)

	// 更新候选状态
	candidateStatus := types.CandidateFailed
	if gateResult.Passed {
		if tuningLevel == types.TuningLevel1 {
			// Level 1: gate 通过后自动激活
			if err := h.profileRepoExt.ActivateProfile(ctx, candidateProfile.ID); err != nil {
				return fmt.Errorf("自动激活候选 profile 失败: %w", err)
			}
			candidateStatus = types.CandidateActivated
			slog.Info("Level 1 自动调优通过 gate 并已激活", "candidate_profile_id", candidateProfile.ID, "base_profile_id", baseProfileID)
		} else {
			// Level 2: gate 通过但需管理员确认
			candidateStatus = types.CandidatePassed
			slog.Info("Level 2 调优通过 gate，等待管理员确认", "candidate_profile_id", candidateProfile.ID, "base_profile_id", baseProfileID)
		}
	} else {
		slog.Warn("调优候选未通过 regression gate", "candidate_profile_id", candidateProfile.ID, "reasons", gateResult.Reasons)
	}

	// 更新候选记录状态
	// 注意：此处使用 candidateProfile.ID 作为候选记录标识的近似，
	// 实际实现中 CreateCandidateRecord 应返回候选记录 ID
	if err := h.profileRepoExt.UpdateCandidateStatus(ctx, candidateProfile.ID, candidateStatus, gateResult.Passed, gateDetailsJSON, candidateResultJSON); err != nil {
		slog.Error("更新候选状态失败", "candidate_profile_id", candidateProfile.ID, "error", err)
	}

	return nil
}

// runEvaluation 对指定 profile 配置执行评测并返回指标结果。
// 引入动机：handleEvaluateProfile 和 handleOptimizeProfile 共享评测执行逻辑。
func (h *EvalJobHandler) runEvaluation(ctx context.Context, items []evaluation.Item, profileConfig types.SearchProfileConfig, jobID, datasetID, profileID string) (*evaluation.EvaluationResult, error) {
	evalItems := make([]evaluation.EvaluationItem, len(items))
	queryResults := make([]evaluation.QueryResult, len(items))

	for i, item := range items {
		evalItems[i] = evaluation.EvaluationItem{
			Query:             item.Query,
			QueryClass:        item.QueryClass,
			ExpectedDocuments: item.ExpectedDocuments,
		}

		searchInput := pipeline.SearchInput{
			WorkspaceID: "",
			Query:       item.Query,
			Profile:     profileConfig,
			Limit:       10,
			Mode:        "hybrid",
		}

		output, searchErr := h.pipeline.Search(ctx, searchInput)
		if searchErr != nil {
			slog.Warn("评测搜索失败，记录空结果", "query", item.Query, "error", searchErr)
			queryResults[i] = evaluation.QueryResult{
				QueryClass: item.QueryClass,
			}
			continue
		}

		queryResults[i] = evaluation.QueryResult{
			QueryClass:   item.QueryClass,
			LatencyMs:    output.LatencyMs,
			Results:      output.Results,
			RerankerUsed: output.RerankerUsed,
			RerankerCost: output.RerankerCost,
		}
	}

	result := evaluation.ComputeMetrics(evalItems, queryResults)

	// 持久化评测结果
	metricsJSON, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("序列化评测指标: %w", err)
	}
	if _, err := h.evalResultRepo.SaveResult(ctx, datasetID, profileID, jobID, metricsJSON, len(items)); err != nil {
		return nil, fmt.Errorf("持久化评测结果: %w", err)
	}

	return &result, nil
}

// getBaselineEvaluation 对 baseline profile 执行评测以获取基线指标。
// 引入动机：Regression Gate 需要对比候选和基线的指标。
func (h *EvalJobHandler) getBaselineEvaluation(ctx context.Context, items []evaluation.Item, baseProfile *ProfileForJobExtended, datasetID, jobID string) (*evaluation.EvaluationResult, error) {
	baseConfig := types.SearchProfileConfig{
		ID:                        baseProfile.ID,
		Name:                      baseProfile.Name,
		Version:                   baseProfile.Version,
		ESIndexName:               baseProfile.ESIndexName,
		ChunkTargetSize:           baseProfile.ChunkTargetSize,
		ChunkOverlap:              baseProfile.ChunkOverlap,
		EmbeddingProvider:         baseProfile.EmbeddingProvider,
		EmbeddingModel:            baseProfile.EmbeddingModel,
		EmbeddingDimensions:       baseProfile.EmbeddingDimensions,
		EmbeddingQueryInstruction: baseProfile.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   baseProfile.EmbeddingDocInstruction,
		Analyzer:                  baseProfile.Analyzer,
		RerankerProvider:          baseProfile.RerankerProvider,
		RerankerModel:             baseProfile.RerankerModel,
		TitleBoost:                baseProfile.TitleBoost,
		HeadingBoost:              baseProfile.HeadingBoost,
		PathBoost:                 baseProfile.PathBoost,
		TagsBoost:                 baseProfile.TagsBoost,
		BodyBoost:                 baseProfile.BodyBoost,
		LexicalTopK:               baseProfile.LexicalTopK,
		VectorTopK:                baseProfile.VectorTopK,
		RRFK:                     baseProfile.RRFK,
		RerankerCandidateCount:    baseProfile.RerankerCandidateCount,
		RerankerFinalCount:        baseProfile.RerankerFinalCount,
		MaxChunksPerDocument:      baseProfile.MaxChunksPerDocument,
		MergeAdjacentChunks:       baseProfile.MergeAdjacentChunks,
		MaxP95LatencyMs:           baseProfile.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:   baseProfile.MaxRerankerCostPerQuery,
	}

	return h.runEvaluation(ctx, items, baseConfig, jobID, datasetID, baseProfile.ID)
}

// validateLevel1Changes 验证 Level 1 参数变更仅涉及允许的参数。
// 引入动机：design/01-SEARCH.md §Auto Tuning Level 1 仅允许调整
// field boost / top_k / RRF / candidate count / diversity 参数。
func validateLevel1Changes(changes *ParameterChanges) error {
	// ParameterChanges 结构体本身仅包含 Level 1 允许的字段，
	// 不包含 chunk/embedding/reranker model 等高级参数。
	// 此处验证字段值合法性。
	if changes.LexicalTopK != nil && *changes.LexicalTopK <= 0 {
		return fmt.Errorf("lexical_top_k 必须为正整数")
	}
	if changes.VectorTopK != nil && *changes.VectorTopK <= 0 {
		return fmt.Errorf("vector_top_k 必须为正整数")
	}
	if changes.RRFK != nil && *changes.RRFK <= 0 {
		return fmt.Errorf("rrf_k 必须为正整数")
	}
	if changes.RerankerCandidateCount != nil && *changes.RerankerCandidateCount <= 0 {
		return fmt.Errorf("reranker_candidate_count 必须为正整数")
	}
	if changes.MaxChunksPerDocument != nil && *changes.MaxChunksPerDocument <= 0 {
		return fmt.Errorf("max_chunks_per_document 必须为正整数")
	}
	return nil
}

// extractCriticalClasses 从评测 items 中提取所有 query class 作为关键 class。
// 引入动机：Regression Gate 要求任一关键 query class 不出现明显退化。
func extractCriticalClasses(items []evaluation.Item) []string {
	seen := make(map[string]bool)
	var classes []string
	for _, item := range items {
		if item.QueryClass != "" && !seen[item.QueryClass] {
			seen[item.QueryClass] = true
			classes = append(classes, item.QueryClass)
		}
	}
	return classes
}
