// metrics.go 实现搜索指标和反馈的持久化。
//
// 引入动机：design/01-SEARCH.md §Feedback 要求记录 verified evaluation,
// explicit feedback, implicit search→read/patch 信号。
// design/05-OPERATIONS.md §Search Maintenance 要求持续收集 search metrics。
package job

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
)

// SearchMetricsRepo 定义搜索指标的数据访问接口。
type SearchMetricsRepo interface {
	// RecordMetrics 记录搜索指标。
	RecordMetrics(ctx context.Context, params RecordMetricsParams) error

	// RecordFeedback 记录搜索反馈。
	RecordFeedback(ctx context.Context, params RecordFeedbackParams) error
}

// RecordMetricsParams 是记录搜索指标的参数。
type RecordMetricsParams struct {
	WorkspaceID       string
	UserID            string
	SearchID          string
	ProfileID         string
	LatencyMs         int
	RerankerUsed      bool
	RerankerCost      float32
	ResultCount       int
	Degraded          bool
	DegradationReason string
}

// RecordFeedbackParams 是记录搜索反馈的参数。
type RecordFeedbackParams struct {
	WorkspaceID  string
	UserID       string
	FeedbackType string
	Query        string
	DocumentID   string
	Detail       json.RawMessage
	SearchID     string
}

// PGSearchMetricsRepo 是 SearchMetricsRepo 的 PostgreSQL 实现。
type PGSearchMetricsRepo struct {
	db *sql.DB
}

// NewPGSearchMetricsRepo 创建 PGSearchMetricsRepo。
func NewPGSearchMetricsRepo(db *sql.DB) *PGSearchMetricsRepo {
	return &PGSearchMetricsRepo{db: db}
}

// RecordMetrics 记录搜索指标。
func (r *PGSearchMetricsRepo) RecordMetrics(ctx context.Context, params RecordMetricsParams) error {
	var profileIDVal interface{}
	if params.ProfileID != "" {
		profileIDVal = params.ProfileID
	}
	var userIDVal interface{}
	if params.UserID != "" {
		userIDVal = params.UserID
	}
	var degrReasonVal interface{}
	if params.DegradationReason != "" {
		degrReasonVal = params.DegradationReason
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO search_metrics (
			workspace_id, user_id, search_id, profile_id,
			latency_ms, reranker_used, reranker_cost, result_count,
			degraded, degradation_reason
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		params.WorkspaceID, userIDVal, params.SearchID, profileIDVal,
		params.LatencyMs, params.RerankerUsed, params.RerankerCost, params.ResultCount,
		params.Degraded, degrReasonVal,
	)
	if err != nil {
		return mapDBError(err, "记录搜索指标")
	}
	return nil
}

// RecordFeedback 记录搜索反馈。
// 引入动机：design/01-SEARCH.md §Feedback 要求记录反馈，
// query 使用 hash 存储以保护隐私（不记录完整 query 如有隐私风险）。
func (r *PGSearchMetricsRepo) RecordFeedback(ctx context.Context, params RecordFeedbackParams) error {
	// 对 query 计算 hash，不存储原始 query 以保护隐私
	queryHash := hashQuery(params.Query)

	var docIDVal interface{}
	if params.DocumentID != "" {
		docIDVal = params.DocumentID
	}
	var detailVal interface{}
	if len(params.Detail) > 0 {
		detailVal = []byte(params.Detail)
	}
	var searchIDVal interface{}
	if params.SearchID != "" {
		searchIDVal = params.SearchID
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO search_feedback (
			workspace_id, user_id, feedback_type, query_hash,
			document_id, detail, search_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		params.WorkspaceID, params.UserID, params.FeedbackType, queryHash,
		docIDVal, detailVal, searchIDVal,
	)
	if err != nil {
		return mapDBError(err, "记录搜索反馈")
	}
	return nil
}

// hashQuery 对查询文本计算 SHA-256 hash。
// 引入动机：design 审计要求不记录可能有隐私风险的 query 全文。
func hashQuery(query string) string {
	if query == "" {
		return ""
	}
	h := sha256.Sum256([]byte(query))
	return hex.EncodeToString(h[:])
}
