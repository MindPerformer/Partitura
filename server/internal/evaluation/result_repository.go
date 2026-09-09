// result_repository.go 实现评测结果的持久化和查询。
//
// 引入动机：design/01-SEARCH.md §Evaluation 要求评测指标可查询；
// design/04-WEB-API.md §Admin 要求 evaluation 管理 API；
// design/05-OPERATIONS.md §Background Jobs 要求 evaluate_profile job 实际执行并持久化结果。
//
// M004 创建 evaluation_results 表，存储每次评测运行的指标结果（NDCG/Recall/MRR 等）。
package evaluation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// EvaluationRunResult 是一次评测运行的完整结果记录。
// 引入动机：RunEvaluation API 和 evaluate_profile job 需要将计算出的指标持久化，
// 使管理员可查询历史评测结果和趋势。
type EvaluationRunResult struct {
	ID         string          `json:"id"`
	DatasetID  string          `json:"dataset_id"`
	ProfileID  string          `json:"profile_id"`
	JobID      string          `json:"job_id,omitempty"`
	Metrics    json.RawMessage `json:"metrics"`
	ItemCount  int             `json:"item_count"`
	CreatedAt  string          `json:"created_at"`
}

// ListResultsResult 是评测结果列表查询结果。
type ListResultsResult struct {
	Results []EvaluationRunResult `json:"results"`
	Total   int                   `json:"total"`
}

// ResultRepository 定义评测结果的持久化接口。
// 引入动机：admin handler 和 job handler 需要写入和查询评测结果，
// 接口化便于测试用 fake 替换 PG 实现。
type ResultRepository interface {
	// SaveResult 持久化一次评测运行的结果。
	SaveResult(ctx context.Context, datasetID, profileID, jobID string, metrics json.RawMessage, itemCount int) (*EvaluationRunResult, error)

	// ListResults 查询指定 dataset 的评测结果列表（分页）。
	ListResults(ctx context.Context, datasetID string, limit, offset int) (*ListResultsResult, error)

	// GetLatestResult 查询指定 dataset+profile 的最新评测结果。
	GetLatestResult(ctx context.Context, datasetID, profileID string) (*EvaluationRunResult, error)
}

// PGResultRepository 是 ResultRepository 的 PostgreSQL 实现。
type PGResultRepository struct {
	db *sql.DB
}

// NewPGResultRepository 创建 PGResultRepository。
func NewPGResultRepository(db *sql.DB) *PGResultRepository {
	return &PGResultRepository{db: db}
}

// SaveResult 持久化一次评测运行的结果。
func (r *PGResultRepository) SaveResult(ctx context.Context, datasetID, profileID, jobID string, metrics json.RawMessage, itemCount int) (*EvaluationRunResult, error) {
	var result EvaluationRunResult
	var jobIDVal interface{}
	if jobID != "" {
		jobIDVal = jobID
	}

	err := r.db.QueryRowContext(ctx,
		`INSERT INTO evaluation_results (dataset_id, profile_id, job_id, metrics, item_count)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, dataset_id, profile_id, COALESCE(job_id::text, ''), metrics, item_count,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		datasetID, profileID, jobIDVal, []byte(metrics), itemCount,
	).Scan(&result.ID, &result.DatasetID, &result.ProfileID, &result.JobID, &result.Metrics, &result.ItemCount, &result.CreatedAt)
	if err != nil {
		return nil, mapResultDBError(err, "保存评测结果")
	}
	return &result, nil
}

// ListResults 查询指定 dataset 的评测结果列表（分页）。
func (r *PGResultRepository) ListResults(ctx context.Context, datasetID string, limit, offset int) (*ListResultsResult, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_results WHERE dataset_id = $1`,
		datasetID,
	).Scan(&total)
	if err != nil {
		return nil, mapResultDBError(err, "查询评测结果总数")
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, dataset_id, profile_id, COALESCE(job_id::text, ''), metrics, item_count,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM evaluation_results WHERE dataset_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		datasetID, limit, offset,
	)
	if err != nil {
		return nil, mapResultDBError(err, "查询评测结果列表")
	}
	defer rows.Close()

	var results []EvaluationRunResult
	for rows.Next() {
		var r EvaluationRunResult
		if err := rows.Scan(&r.ID, &r.DatasetID, &r.ProfileID, &r.JobID, &r.Metrics, &r.ItemCount, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描评测结果行: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历评测结果集: %w", err)
	}

	return &ListResultsResult{Results: results, Total: total}, nil
}

// GetLatestResult 查询指定 dataset+profile 的最新评测结果。
func (r *PGResultRepository) GetLatestResult(ctx context.Context, datasetID, profileID string) (*EvaluationRunResult, error) {
	var result EvaluationRunResult
	err := r.db.QueryRowContext(ctx,
		`SELECT id, dataset_id, profile_id, COALESCE(job_id::text, ''), metrics, item_count,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM evaluation_results WHERE dataset_id = $1 AND profile_id = $2
		 ORDER BY created_at DESC LIMIT 1`,
		datasetID, profileID,
	).Scan(&result.ID, &result.DatasetID, &result.ProfileID, &result.JobID, &result.Metrics, &result.ItemCount, &result.CreatedAt)
	if err != nil {
		return nil, mapResultDBError(err, "查询最新评测结果")
	}
	return &result, nil
}

// mapResultDBError 将 database/sql 错误映射为带上下文的错误信息。
func mapResultDBError(err error, context string) error {
	if err == sql.ErrNoRows {
		return err
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return fmt.Errorf("%s: PostgreSQL 错误 %s: %w", context, pgErr.Code, err)
	}
	return fmt.Errorf("%s: %w", context, err)
}
