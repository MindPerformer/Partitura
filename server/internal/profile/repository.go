// Package profile 实现 Search Profile 版本管理：CRUD、激活、回滚、alias 原子切换。
//
// 引入动机：design/01-SEARCH.md §Search Profile 要求版本化，
// §Index Version 要求 alias 原子切换，禁止原地破坏 active index。
// §Auto Tuning 要求 Level 1 自动激活且经 gate，Level 2 需管理员确认，Level 3 人工触发。
//
// 设计原则：
//   - Profile 状态变更记录 audit
//   - 新 profile 必须可回滚
//   - 重大 profile 变更创建新 index + 全量 rebuild + alias 切换
//   - 不出现 XxxService/XxxManager/XxxController
package profile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5/pgconn"

	"partitura/server/internal/es"
	types "partitura/server/internal/search/types"
)

// Repository 定义 Search Profile 的数据访问接口。
// 引入动机：profile handler 和 job worker 依赖此接口而非具体 PG 实现。
type Repository interface {
	// CreateProfile 创建新的 Search Profile。
	CreateProfile(ctx context.Context, p *CreateProfileInput) (*ProfileRecord, error)

	// GetProfileByID 根据 ID 查询 Profile。
	GetProfileByID(ctx context.Context, id string) (*ProfileRecord, error)

	// ListProfiles 查询 Profile 列表（分页），可按 status 过滤。
	ListProfiles(ctx context.Context, statusFilter string, limit, offset int) (*ListProfilesResult, error)

	// GetActiveProfile 查询当前 active 的 Profile。
	GetActiveProfile(ctx context.Context) (*ProfileRecord, error)

	// ActivateProfile 将指定 Profile 设为 active，同时将当前 active Profile 设为 inactive。
	// 引入动机：design/01-SEARCH.md §Index Version 要求 alias 原子切换。
	ActivateProfile(ctx context.Context, id string) error

	// DeactivateProfile 将指定 Profile 设为 inactive。
	DeactivateProfile(ctx context.Context, id string) error

	// UpdateProfileESIndex 更新 Profile 的 ES 索引名称。
	// 引入动机：rebuild job 创建新 index 后需要更新 profile 的 es_index_name。
	UpdateProfileESIndex(ctx context.Context, id, indexName string) error

	// CreateCandidate 创建调优候选。
	CreateCandidate(ctx context.Context, c *CandidateRecord) error

	// GetCandidateByID 根据 ID 查询候选。
	GetCandidateByID(ctx context.Context, id string) (*CandidateRecord, error)

	// ListCandidates 查询候选列表（分页），可按 status 过滤。
	ListCandidates(ctx context.Context, statusFilter string, limit, offset int) (*ListCandidatesResult, error)

	// UpdateCandidateStatus 更新候选状态。
	UpdateCandidateStatus(ctx context.Context, id, status string, gatePassed bool, gateDetails json.RawMessage, evaluationResult json.RawMessage) error

	// ConfirmCandidate 管理员确认候选（Level 2/3 需要）。
	ConfirmCandidate(ctx context.Context, id, adminUserID string) error
}

// ProfileRecord 是从数据库读取的 Search Profile 记录。
// JSON 标签必须与 web/types/api.ts 的 SearchProfile 契约精确一致（snake_case），
// 否则前端读取 embedding_model/lexical_top_k 等字段会得到 undefined。
// 引入动机：Phase6 WP1 修复 — 缺省 PascalCase 序列化导致前端字段全部缺失。
type ProfileRecord struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Version         int    `json:"version"`
	Status          string `json:"status"`
	EmbeddingProvider string `json:"embedding_provider"`
	EmbeddingModel    string `json:"embedding_model"`
	EmbeddingDimensions int  `json:"embedding_dimensions"`
	EmbeddingQueryInstruction string `json:"embedding_query_instruction"`
	EmbeddingDocInstruction  string `json:"embedding_document_instruction"`
	ChunkAlgorithmVersion string `json:"chunk_algorithm_version"`
	ChunkTargetSize   int `json:"chunk_target_size"`
	ChunkOverlap      int `json:"chunk_overlap"`
	ChunkParentSectionBehavior string `json:"chunk_parent_section_behavior"`
	TitleBoost   float32 `json:"title_boost"`
	HeadingBoost float32 `json:"heading_boost"`
	PathBoost    float32 `json:"path_boost"`
	TagsBoost    float32 `json:"tags_boost"`
	BodyBoost    float32 `json:"body_boost"`
	Analyzer     string  `json:"analyzer"`
	LexicalTopK  int     `json:"lexical_top_k"`
	VectorTopK   int     `json:"vector_top_k"`
	RRFK         int     `json:"rrf_k"`
	RerankerProvider      string `json:"reranker_provider"`
	RerankerModel         string `json:"reranker_model"`
	RerankerCandidateCount int    `json:"reranker_candidate_count"`
	RerankerFinalCount     int    `json:"reranker_final_count"`
	MaxChunksPerDocument int  `json:"max_chunks_per_document"`
	MergeAdjacentChunks  bool `json:"merge_adjacent_chunks"`
	ESIndexName string `json:"es_index_name"`
	MaxP95LatencyMs      int     `json:"max_p95_latency_ms"`
	MaxRerankerCostPerQuery float32 `json:"max_reranker_cost_per_query"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	// activated_at 允许为空（未激活 profile 无激活时间）；
	// 使用 omitempty 与 web SearchProfile.activated_at? 的可选契约一致。
	ActivatedAt string `json:"activated_at,omitempty"`
}

// ToConfig 将 ProfileRecord 转换为 SearchProfileConfig。
// 引入动机：搜索管线需要 SearchProfileConfig 来执行搜索。
func (r *ProfileRecord) ToConfig() types.SearchProfileConfig {
	return types.SearchProfileConfig{
		ID:                       r.ID,
		Name:                     r.Name,
		Version:                  r.Version,
		Status:                   r.Status,
		EmbeddingProvider:        r.EmbeddingProvider,
		EmbeddingModel:           r.EmbeddingModel,
		EmbeddingDimensions:      r.EmbeddingDimensions,
		EmbeddingQueryInstruction: r.EmbeddingQueryInstruction,
		EmbeddingDocInstruction:   r.EmbeddingDocInstruction,
		ChunkAlgorithmVersion:    r.ChunkAlgorithmVersion,
		ChunkTargetSize:          r.ChunkTargetSize,
		ChunkOverlap:             r.ChunkOverlap,
		TitleBoost:               r.TitleBoost,
		HeadingBoost:             r.HeadingBoost,
		PathBoost:                r.PathBoost,
		TagsBoost:                r.TagsBoost,
		BodyBoost:                r.BodyBoost,
		Analyzer:                 r.Analyzer,
		LexicalTopK:              r.LexicalTopK,
		VectorTopK:               r.VectorTopK,
		RRFK:                     r.RRFK,
		RerankerProvider:         r.RerankerProvider,
		RerankerModel:            r.RerankerModel,
		RerankerCandidateCount:   r.RerankerCandidateCount,
		RerankerFinalCount:       r.RerankerFinalCount,
		MaxChunksPerDocument:     r.MaxChunksPerDocument,
		MergeAdjacentChunks:      r.MergeAdjacentChunks,
		ESIndexName:              r.ESIndexName,
		MaxP95LatencyMs:          r.MaxP95LatencyMs,
		MaxRerankerCostPerQuery:  r.MaxRerankerCostPerQuery,
	}
}

// CreateProfileInput 是创建 Profile 的输入参数。
type CreateProfileInput struct {
	Name            string
	EmbeddingProvider string
	EmbeddingModel    string
	EmbeddingDimensions int
	EmbeddingQueryInstruction string
	EmbeddingDocInstruction  string
	ChunkTargetSize   int
	ChunkOverlap      int
	TitleBoost   float32
	HeadingBoost float32
	PathBoost    float32
	TagsBoost    float32
	BodyBoost    float32
	Analyzer     string
	LexicalTopK  int
	VectorTopK   int
	RRFK         int
	RerankerProvider      string
	RerankerModel         string
	RerankerCandidateCount int
	RerankerFinalCount     int
	MaxChunksPerDocument int
	MergeAdjacentChunks  bool
	MaxP95LatencyMs      int
	MaxRerankerCostPerQuery float32
	CreatedBy   string
}

// CandidateRecord 是调优候选记录。
type CandidateRecord struct {
	ID                 string
	BaseProfileID      string
	CandidateProfileID string
	TuningLevel        int
	ParameterChanges   json.RawMessage
	EvaluationResult   json.RawMessage
	GatePassed         bool
	GateDetails        json.RawMessage
	Status             string
	AdminConfirmed     bool
	AdminConfirmedBy   string
	AdminConfirmedAt   string
	CreatedAt          string
}

// ListProfilesResult 是 Profile 列表查询结果。
// 使用 snake_case JSON 标签保证与 REST 契约一致。
type ListProfilesResult struct {
	Profiles []ProfileRecord `json:"profiles"`
	Total    int             `json:"total"`
}

// ListCandidatesResult 是候选列表查询结果。
type ListCandidatesResult struct {
	Candidates []CandidateRecord `json:"candidates"`
	Total      int               `json:"total"`
}

// PGRepository 是 Repository 接口的 PostgreSQL 实现。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// CreateProfile 创建新的 Search Profile。
func (r *PGRepository) CreateProfile(ctx context.Context, input *CreateProfileInput) (*ProfileRecord, error) {
	// 计算版本号：同名 profile 的最大 version + 1
	var maxVersion int
	err := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM search_profiles WHERE name = $1`,
		input.Name,
	).Scan(&maxVersion)
	if err != nil {
		return nil, mapDBError(err, "查询 profile 最大版本")
	}
	newVersion := maxVersion + 1

	// 生成 ES 索引名称
	indexName := es.GenerateIndexName(newVersion)

	var p ProfileRecord
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO search_profiles (
			name, version, status,
			embedding_provider, embedding_model, embedding_dimensions,
			embedding_query_instruction, embedding_document_instruction,
			chunk_target_size, chunk_overlap,
			lexical_title_boost, lexical_heading_boost, lexical_path_boost, lexical_tags_boost, lexical_body_boost, lexical_analyzer,
			retrieval_lexical_top_k, retrieval_vector_top_k, retrieval_rrf_k,
			reranker_provider, reranker_model, reranker_candidate_count, reranker_final_count,
			diversification_max_chunks_per_document, diversification_merge_adjacent_chunks,
			es_index_name, max_p95_latency_ms, max_reranker_cost_per_query,
			created_by
		) VALUES (
			$1, $2, 'draft',
			$3, $4, $5,
			$6, $7,
			$8, $9,
			$10, $11, $12, $13, $14, $15,
			$16, $17, $18,
			$19, $20, $21, $22,
			$23, $24,
			$25, $26, $27,
			$28
		)
		RETURNING id, name, version, status,
			embedding_provider, embedding_model, embedding_dimensions,
			embedding_query_instruction, embedding_document_instruction,
			chunk_algorithm_version, chunk_target_size, chunk_overlap, chunk_parent_section_behavior,
			lexical_title_boost, lexical_heading_boost, lexical_path_boost, lexical_tags_boost, lexical_body_boost, lexical_analyzer,
			retrieval_lexical_top_k, retrieval_vector_top_k, retrieval_rrf_k,
			reranker_provider, reranker_model, reranker_candidate_count, reranker_final_count,
			diversification_max_chunks_per_document, diversification_merge_adjacent_chunks,
			es_index_name, max_p95_latency_ms, max_reranker_cost_per_query,
			created_by,
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(activated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')`,
		input.Name, newVersion,
		input.EmbeddingProvider, input.EmbeddingModel, input.EmbeddingDimensions,
		input.EmbeddingQueryInstruction, input.EmbeddingDocInstruction,
		input.ChunkTargetSize, input.ChunkOverlap,
		input.TitleBoost, input.HeadingBoost, input.PathBoost, input.TagsBoost, input.BodyBoost, input.Analyzer,
		input.LexicalTopK, input.VectorTopK, input.RRFK,
		input.RerankerProvider, input.RerankerModel, input.RerankerCandidateCount, input.RerankerFinalCount,
		input.MaxChunksPerDocument, input.MergeAdjacentChunks,
		indexName, input.MaxP95LatencyMs, input.MaxRerankerCostPerQuery,
		input.CreatedBy,
	).Scan(
		&p.ID, &p.Name, &p.Version, &p.Status,
		&p.EmbeddingProvider, &p.EmbeddingModel, &p.EmbeddingDimensions,
		&p.EmbeddingQueryInstruction, &p.EmbeddingDocInstruction,
		&p.ChunkAlgorithmVersion, &p.ChunkTargetSize, &p.ChunkOverlap, &p.ChunkParentSectionBehavior,
		&p.TitleBoost, &p.HeadingBoost, &p.PathBoost, &p.TagsBoost, &p.BodyBoost, &p.Analyzer,
		&p.LexicalTopK, &p.VectorTopK, &p.RRFK,
		&p.RerankerProvider, &p.RerankerModel, &p.RerankerCandidateCount, &p.RerankerFinalCount,
		&p.MaxChunksPerDocument, &p.MergeAdjacentChunks,
		&p.ESIndexName, &p.MaxP95LatencyMs, &p.MaxRerankerCostPerQuery,
		&p.CreatedBy, &p.CreatedAt, &p.ActivatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "创建 search profile")
	}

	return &p, nil
}

// GetProfileByID 根据 ID 查询 Profile。
func (r *PGRepository) GetProfileByID(ctx context.Context, id string) (*ProfileRecord, error) {
	var p ProfileRecord
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, version, status,
			embedding_provider, embedding_model, embedding_dimensions,
			embedding_query_instruction, embedding_document_instruction,
			chunk_algorithm_version, chunk_target_size, chunk_overlap, chunk_parent_section_behavior,
			lexical_title_boost, lexical_heading_boost, lexical_path_boost, lexical_tags_boost, lexical_body_boost, lexical_analyzer,
			retrieval_lexical_top_k, retrieval_vector_top_k, retrieval_rrf_k,
			reranker_provider, reranker_model, reranker_candidate_count, reranker_final_count,
			diversification_max_chunks_per_document, diversification_merge_adjacent_chunks,
			es_index_name, max_p95_latency_ms, max_reranker_cost_per_query,
			created_by,
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(activated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		 FROM search_profiles WHERE id = $1`,
		id,
	).Scan(
		&p.ID, &p.Name, &p.Version, &p.Status,
		&p.EmbeddingProvider, &p.EmbeddingModel, &p.EmbeddingDimensions,
		&p.EmbeddingQueryInstruction, &p.EmbeddingDocInstruction,
		&p.ChunkAlgorithmVersion, &p.ChunkTargetSize, &p.ChunkOverlap, &p.ChunkParentSectionBehavior,
		&p.TitleBoost, &p.HeadingBoost, &p.PathBoost, &p.TagsBoost, &p.BodyBoost, &p.Analyzer,
		&p.LexicalTopK, &p.VectorTopK, &p.RRFK,
		&p.RerankerProvider, &p.RerankerModel, &p.RerankerCandidateCount, &p.RerankerFinalCount,
		&p.MaxChunksPerDocument, &p.MergeAdjacentChunks,
		&p.ESIndexName, &p.MaxP95LatencyMs, &p.MaxRerankerCostPerQuery,
		&p.CreatedBy, &p.CreatedAt, &p.ActivatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询 search profile")
	}
	return &p, nil
}

// ListProfiles 查询 Profile 列表（分页），可按 status 过滤。
func (r *PGRepository) ListProfiles(ctx context.Context, statusFilter string, limit, offset int) (*ListProfilesResult, error) {
	conditions := []string{}
	args := []interface{}{}
	argIdx := 1

	if statusFilter != "" {
		conditions = append(conditions, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, statusFilter)
		argIdx++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + joinStrings(conditions, " AND ")
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM search_profiles" + whereClause
	err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询 profile 总数")
	}

	listQuery := fmt.Sprintf(
		`SELECT id, name, version, status,
			embedding_provider, embedding_model, embedding_dimensions,
			embedding_query_instruction, embedding_document_instruction,
			chunk_algorithm_version, chunk_target_size, chunk_overlap, chunk_parent_section_behavior,
			lexical_title_boost, lexical_heading_boost, lexical_path_boost, lexical_tags_boost, lexical_body_boost, lexical_analyzer,
			retrieval_lexical_top_k, retrieval_vector_top_k, retrieval_rrf_k,
			reranker_provider, reranker_model, reranker_candidate_count, reranker_final_count,
			diversification_max_chunks_per_document, diversification_merge_adjacent_chunks,
			es_index_name, max_p95_latency_ms, max_reranker_cost_per_query,
			created_by,
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(activated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		 FROM search_profiles%s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询 profile 列表")
	}
	defer rows.Close()

	// 初始化为空 slice 而非 nil：保证 0 行时 JSON 序列化为 [] 而非 null，
	// 防止 web 端 profiles.length 抛 TypeError。
	profiles := []ProfileRecord{}
	for rows.Next() {
		var p ProfileRecord
		if err := scanProfile(&p, rows); err != nil {
			return nil, fmt.Errorf("扫描 profile 行: %w", err)
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 profile 结果集: %w", err)
	}

	return &ListProfilesResult{Profiles: profiles, Total: total}, nil
}

// GetActiveProfile 查询当前 active 的 Profile。
func (r *PGRepository) GetActiveProfile(ctx context.Context) (*ProfileRecord, error) {
	var p ProfileRecord
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, version, status,
			embedding_provider, embedding_model, embedding_dimensions,
			embedding_query_instruction, embedding_document_instruction,
			chunk_algorithm_version, chunk_target_size, chunk_overlap, chunk_parent_section_behavior,
			lexical_title_boost, lexical_heading_boost, lexical_path_boost, lexical_tags_boost, lexical_body_boost, lexical_analyzer,
			retrieval_lexical_top_k, retrieval_vector_top_k, retrieval_rrf_k,
			reranker_provider, reranker_model, reranker_candidate_count, reranker_final_count,
			diversification_max_chunks_per_document, diversification_merge_adjacent_chunks,
			es_index_name, max_p95_latency_ms, max_reranker_cost_per_query,
			created_by,
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(activated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		 FROM search_profiles WHERE status = 'active' ORDER BY activated_at DESC LIMIT 1`,
	).Scan(
		&p.ID, &p.Name, &p.Version, &p.Status,
		&p.EmbeddingProvider, &p.EmbeddingModel, &p.EmbeddingDimensions,
		&p.EmbeddingQueryInstruction, &p.EmbeddingDocInstruction,
		&p.ChunkAlgorithmVersion, &p.ChunkTargetSize, &p.ChunkOverlap, &p.ChunkParentSectionBehavior,
		&p.TitleBoost, &p.HeadingBoost, &p.PathBoost, &p.TagsBoost, &p.BodyBoost, &p.Analyzer,
		&p.LexicalTopK, &p.VectorTopK, &p.RRFK,
		&p.RerankerProvider, &p.RerankerModel, &p.RerankerCandidateCount, &p.RerankerFinalCount,
		&p.MaxChunksPerDocument, &p.MergeAdjacentChunks,
		&p.ESIndexName, &p.MaxP95LatencyMs, &p.MaxRerankerCostPerQuery,
		&p.CreatedBy, &p.CreatedAt, &p.ActivatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询 active profile")
	}
	return &p, nil
}

// ActivateProfile 将指定 Profile 设为 active，同时将当前 active Profile 设为 inactive。
// 引入动机：design/01-SEARCH.md §Index Version 要求 alias 原子切换。
//
// 注意：search_profiles 表（M003）没有 updated_at 列，只有 created_at 和 activated_at。
// 此前 SQL 引用 updated_at 导致 PostgreSQL 42703 错误，激活失败。
// 修复后仅更新 status 和 activated_at，与 schema 契约一致。
func (r *PGRepository) ActivateProfile(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启激活 profile 事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// 将当前 active profile 设为 inactive
	// search_profiles 表无 updated_at 列，仅更新 status
	_, err = tx.ExecContext(ctx,
		`UPDATE search_profiles SET status = 'inactive' WHERE status = 'active'`,
	)
	if err != nil {
		return mapDBError(err, "取消当前 active profile")
	}

	// 将目标 profile 设为 active
	// activated_at 记录激活时间，search_profiles 表无 updated_at 列
	_, err = tx.ExecContext(ctx,
		`UPDATE search_profiles SET status = 'active', activated_at = now() WHERE id = $1`,
		id,
	)
	if err != nil {
		return mapDBError(err, "激活 profile")
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("提交激活 profile 事务: %w", err)
	}

	slog.Info("search profile 已激活", "profile_id", id)
	return nil
}

// DeactivateProfile 将指定 Profile 设为 inactive。
// 注意：search_profiles 表（M003）没有 updated_at 列，仅更新 status。
func (r *PGRepository) DeactivateProfile(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE search_profiles SET status = 'inactive' WHERE id = $1`,
		id,
	)
	if err != nil {
		return mapDBError(err, "取消激活 profile")
	}
	return nil
}

// UpdateProfileESIndex 更新 Profile 的 ES 索引名称。
// 引入动机：rebuild job 创建新 index 后需要更新 profile 的 es_index_name。
// 注意：search_profiles 表（M003）没有 updated_at 列，仅更新 es_index_name。
func (r *PGRepository) UpdateProfileESIndex(ctx context.Context, id, indexName string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE search_profiles SET es_index_name = $1 WHERE id = $2`,
		indexName, id,
	)
	if err != nil {
		return mapDBError(err, "更新 profile ES 索引名")
	}
	return nil
}

// CreateCandidate 创建调优候选。
func (r *PGRepository) CreateCandidate(ctx context.Context, c *CandidateRecord) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO search_profile_candidates (
			base_profile_id, candidate_profile_id, tuning_level,
			parameter_changes, status
		) VALUES ($1, $2, $3, $4, $5)`,
		c.BaseProfileID, c.CandidateProfileID, c.TuningLevel,
		[]byte(c.ParameterChanges), c.Status,
	)
	if err != nil {
		return mapDBError(err, "创建调优候选")
	}
	return nil
}

// GetCandidateByID 根据 ID 查询候选。
func (r *PGRepository) GetCandidateByID(ctx context.Context, id string) (*CandidateRecord, error) {
	var c CandidateRecord
	var paramChanges, evalResult, gateDetails []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT id, base_profile_id, candidate_profile_id, tuning_level,
			parameter_changes, COALESCE(evaluation_result::text, ''),
			gate_passed, COALESCE(gate_details::text, ''),
			status, admin_confirmed,
			COALESCE(admin_confirmed_by::text, ''),
			COALESCE(to_char(admin_confirmed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM search_profile_candidates WHERE id = $1`,
		id,
	).Scan(
		&c.ID, &c.BaseProfileID, &c.CandidateProfileID, &c.TuningLevel,
		&paramChanges, &evalResult,
		&c.GatePassed, &gateDetails,
		&c.Status, &c.AdminConfirmed,
		&c.AdminConfirmedBy, &c.AdminConfirmedAt,
		&c.CreatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询调优候选")
	}
	c.ParameterChanges = json.RawMessage(paramChanges)
	if len(evalResult) > 0 && string(evalResult) != "" {
		c.EvaluationResult = json.RawMessage(evalResult)
	}
	if len(gateDetails) > 0 && string(gateDetails) != "" {
		c.GateDetails = json.RawMessage(gateDetails)
	}
	return &c, nil
}

// ListCandidates 查询候选列表（分页），可按 status 过滤。
func (r *PGRepository) ListCandidates(ctx context.Context, statusFilter string, limit, offset int) (*ListCandidatesResult, error) {
	conditions := []string{}
	args := []interface{}{}
	argIdx := 1

	if statusFilter != "" {
		conditions = append(conditions, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, statusFilter)
		argIdx++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + joinStrings(conditions, " AND ")
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM search_profile_candidates" + whereClause
	err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询候选总数")
	}

	listQuery := fmt.Sprintf(
		`SELECT id, base_profile_id, candidate_profile_id, tuning_level,
			parameter_changes, COALESCE(evaluation_result::text, ''),
			gate_passed, COALESCE(gate_details::text, ''),
			status, admin_confirmed,
			COALESCE(admin_confirmed_by::text, ''),
			COALESCE(to_char(admin_confirmed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM search_profile_candidates%s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询候选列表")
	}
	defer rows.Close()

	// 与 ListProfiles 一致：空结果序列化为 [] 而非 null。
	candidates := []CandidateRecord{}
	for rows.Next() {
		var c CandidateRecord
		var paramChanges, evalResult, gateDetails []byte
		if err := rows.Scan(
			&c.ID, &c.BaseProfileID, &c.CandidateProfileID, &c.TuningLevel,
			&paramChanges, &evalResult,
			&c.GatePassed, &gateDetails,
			&c.Status, &c.AdminConfirmed,
			&c.AdminConfirmedBy, &c.AdminConfirmedAt,
			&c.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描候选行: %w", err)
		}
		c.ParameterChanges = json.RawMessage(paramChanges)
		if len(evalResult) > 0 && string(evalResult) != "" {
			c.EvaluationResult = json.RawMessage(evalResult)
		}
		if len(gateDetails) > 0 && string(gateDetails) != "" {
			c.GateDetails = json.RawMessage(gateDetails)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历候选结果集: %w", err)
	}

	return &ListCandidatesResult{Candidates: candidates, Total: total}, nil
}

// UpdateCandidateStatus 更新候选状态。
func (r *PGRepository) UpdateCandidateStatus(ctx context.Context, id, status string, gatePassed bool, gateDetails json.RawMessage, evaluationResult json.RawMessage) error {
	var gateDetailsVal, evalResultVal interface{}
	if len(gateDetails) > 0 {
		gateDetailsVal = []byte(gateDetails)
	}
	if len(evaluationResult) > 0 {
		evalResultVal = []byte(evaluationResult)
	}

	_, err := r.db.ExecContext(ctx,
		`UPDATE search_profile_candidates
		 SET status = $1, gate_passed = $2, gate_details = $3, evaluation_result = $4, updated_at = now()
		 WHERE id = $5`,
		status, gatePassed, gateDetailsVal, evalResultVal, id,
	)
	if err != nil {
		return mapDBError(err, "更新候选状态")
	}
	return nil
}

// ConfirmCandidate 管理员确认候选。
func (r *PGRepository) ConfirmCandidate(ctx context.Context, id, adminUserID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE search_profile_candidates
		 SET admin_confirmed = TRUE, admin_confirmed_by = $1, admin_confirmed_at = now(), updated_at = now()
		 WHERE id = $2`,
		adminUserID, id,
	)
	if err != nil {
		return mapDBError(err, "确认候选")
	}
	return nil
}

// scanProfile 从 rows 扫描 ProfileRecord。
func scanProfile(p *ProfileRecord, rows interface{ Scan(dest ...interface{}) error }) error {
	return rows.Scan(
		&p.ID, &p.Name, &p.Version, &p.Status,
		&p.EmbeddingProvider, &p.EmbeddingModel, &p.EmbeddingDimensions,
		&p.EmbeddingQueryInstruction, &p.EmbeddingDocInstruction,
		&p.ChunkAlgorithmVersion, &p.ChunkTargetSize, &p.ChunkOverlap, &p.ChunkParentSectionBehavior,
		&p.TitleBoost, &p.HeadingBoost, &p.PathBoost, &p.TagsBoost, &p.BodyBoost, &p.Analyzer,
		&p.LexicalTopK, &p.VectorTopK, &p.RRFK,
		&p.RerankerProvider, &p.RerankerModel, &p.RerankerCandidateCount, &p.RerankerFinalCount,
		&p.MaxChunksPerDocument, &p.MergeAdjacentChunks,
		&p.ESIndexName, &p.MaxP95LatencyMs, &p.MaxRerankerCostPerQuery,
		&p.CreatedBy, &p.CreatedAt, &p.ActivatedAt,
	)
}

// joinStrings 用 sep 连接字符串切片。
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += sep + parts[i]
	}
	return result
}

// mapDBError 将 database/sql 错误映射为带上下文的错误信息。
func mapDBError(err error, context string) error {
	if err == sql.ErrNoRows {
		return err
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return fmt.Errorf("%s: PostgreSQL 错误 %s: %w", context, pgErr.Code, err)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// EnsureDefaultProfile 确保存在一个默认的 active Search Profile。
// 引入动机：系统首次启动时需要有一个默认 profile 供搜索使用。
// 如果已存在 active profile 则不做任何操作。
func EnsureDefaultProfile(ctx context.Context, repo Repository, createdBy string) error {
	_, err := repo.GetActiveProfile(ctx)
	if err == nil {
		return nil // 已存在 active profile
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("检查 active profile: %w", err)
	}

	// 创建默认 profile
	_, err = repo.CreateProfile(ctx, &CreateProfileInput{
		Name:                     "default",
		EmbeddingProvider:        "openai-compatible",
		EmbeddingModel:           "qwen3-embedding",
		EmbeddingDimensions:      1024,
		EmbeddingQueryInstruction: "",
		EmbeddingDocInstruction:   "",
		ChunkTargetSize:          512,
		ChunkOverlap:             64,
		TitleBoost:               2.0,
		HeadingBoost:             1.5,
		PathBoost:                1.0,
		TagsBoost:                0.5,
		BodyBoost:                1.0,
		Analyzer:                 "standard",
		LexicalTopK:              50,
		VectorTopK:               50,
		RRFK:                     60,
		RerankerProvider:         "openai-compatible",
		RerankerModel:            "qwen3-reranker",
		RerankerCandidateCount:   20,
		RerankerFinalCount:       10,
		MaxChunksPerDocument:     3,
		MergeAdjacentChunks:      true,
		MaxP95LatencyMs:          2000,
		MaxRerankerCostPerQuery:  0.01,
		CreatedBy:                createdBy,
	})
	if err != nil {
		return fmt.Errorf("创建默认 profile: %w", err)
	}

	// 激活默认 profile
	profiles, err := repo.ListProfiles(ctx, "", 1, 0)
	if err != nil || len(profiles.Profiles) == 0 {
		return fmt.Errorf("查询刚创建的默认 profile: %w", err)
	}

	return repo.ActivateProfile(ctx, profiles.Profiles[0].ID)
}

// float32ToBits 用于避免 unused import。
var _ = math.Float32bits
