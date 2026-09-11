// repository.go 实现搜索评测数据集和条目的 PG 数据访问。
//
// 引入动机：design/01-SEARCH.md §Evaluation 要求建设 Search Evaluation Dataset，
// 每条包含 query, expected/relevant documents, relevance grade 0..3, query class。
// design/04-WEB-API.md §Admin 要求 evaluation 管理 API。
package evaluation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// 数据集状态常量。
// 引入动机：evaluation_datasets.status 受 CHECK (status IN ('active','archived'))
// 约束（migrations/M004），handler 与 mock 需要以常量引用合法取值，避免散落字面量。
const (
	// DatasetStatusActive 表示数据集可用于添加条目与运行评测。
	DatasetStatusActive = "active"
	// DatasetStatusArchived 表示数据集已归档：条目与历史评测结果保留，
	// 但不再允许新增条目或运行评测（归档即 API 层终态，不提供恢复端点）。
	DatasetStatusArchived = "archived"
)

// Dataset 是评测数据集记录。
type Dataset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// Item 是评测条目记录。
type Item struct {
	ID                string             `json:"id"`
	DatasetID         string             `json:"dataset_id"`
	Query             string             `json:"query"`
	ExpectedDocuments []ExpectedDocument `json:"expected_documents"`
	RelevanceGrade    int                `json:"relevance_grade"`
	QueryClass        string             `json:"query_class"`
	CreatedAt         string             `json:"created_at"`
}

// ListDatasetsResult 是数据集列表查询结果。
type ListDatasetsResult struct {
	Datasets []Dataset `json:"datasets"`
	Total    int       `json:"total"`
}

// ListItemsResult 是条目列表查询结果。
type ListItemsResult struct {
	Items []Item `json:"items"`
	Total int    `json:"total"`
}

// Repository 定义评测数据访问接口。
// 引入动机：admin handler 依赖此接口而非具体 PG 实现。
type Repository interface {
	CreateDataset(ctx context.Context, name, description, createdBy string) (*Dataset, error)
	ListDatasets(ctx context.Context, statusFilter string, limit, offset int) (*ListDatasetsResult, error)
	GetDataset(ctx context.Context, id string) (*Dataset, error)
	AddItem(ctx context.Context, datasetID, query string, expectedDocs []ExpectedDocument, relevanceGrade int, queryClass string) (*Item, error)
	ListItems(ctx context.Context, datasetID string, limit, offset int) (*ListItemsResult, error)
	// ListAllItems 查询指定数据集的全部条目（不分页）。
	// 引入动机：evaluate_profile job 需要读取全部评测条目执行评测，分页不适用。
	ListAllItems(ctx context.Context, datasetID string) ([]Item, error)

	// DeleteItem 删除指定数据集中的指定条目。
	// 引入动机：admin 需要管理评测条目，删除必须限定 dataset_id 防止
	// 仅凭条目 ID 就越权删除其它数据集的条目。
	// 目标行不存在（或 dataset_id 与条目不属于同一数据集）时返回 sql.ErrNoRows。
	DeleteItem(ctx context.Context, datasetID, itemID string) error

	// ArchiveDataset 将指定数据集软删为 archived。
	// 引入动机：evaluation_items.dataset_id 与 evaluation_results.dataset_id
	// 均为 ON DELETE CASCADE（见 migrations/M004），物理 DELETE 会连带、不可恢复地
	// 删除历史评测结果；归档只翻转 status 字段，条目与评测历史全部保留。
	// 数据集不存在时返回 sql.ErrNoRows。
	ArchiveDataset(ctx context.Context, datasetID string) error
}

// PGRepository 是 Repository 的 PostgreSQL 实现。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// CreateDataset 创建评测数据集。
func (r *PGRepository) CreateDataset(ctx context.Context, name, description, createdBy string) (*Dataset, error) {
	var ds Dataset
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO evaluation_datasets (name, description, created_by)
		 VALUES ($1, $2, $3)
		 RETURNING id, name, COALESCE(description, ''), status, created_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		           to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		name, description, createdBy,
	).Scan(&ds.ID, &ds.Name, &ds.Description, &ds.Status, &ds.CreatedBy, &ds.CreatedAt, &ds.UpdatedAt)
	if err != nil {
		return nil, mapEvalDBError(err, "创建评测数据集")
	}
	return &ds, nil
}

// ListDatasets 查询数据集列表（分页），可按 status 过滤。
func (r *PGRepository) ListDatasets(ctx context.Context, statusFilter string, limit, offset int) (*ListDatasetsResult, error) {
	conditions := []string{"1=1"}
	args := []interface{}{}
	argIdx := 1

	if statusFilter != "" {
		conditions = append(conditions, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, statusFilter)
		argIdx++
	}

	whereClause := joinConditions(conditions)

	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM evaluation_datasets WHERE %s", whereClause)
	err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, mapEvalDBError(err, "查询数据集总数")
	}

	listQuery := fmt.Sprintf(
		`SELECT id, name, COALESCE(description, ''), status, created_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		        to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM evaluation_datasets WHERE %s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapEvalDBError(err, "查询数据集列表")
	}
	defer rows.Close()

	datasets := make([]Dataset, 0)
	for rows.Next() {
		var ds Dataset
		if err := rows.Scan(&ds.ID, &ds.Name, &ds.Description, &ds.Status, &ds.CreatedBy, &ds.CreatedAt, &ds.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描数据集行: %w", err)
		}
		datasets = append(datasets, ds)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历数据集结果集: %w", err)
	}

	return &ListDatasetsResult{Datasets: datasets, Total: total}, nil
}

// GetDataset 根据 ID 查询数据集。
func (r *PGRepository) GetDataset(ctx context.Context, id string) (*Dataset, error) {
	var ds Dataset
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, COALESCE(description, ''), status, created_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		        to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM evaluation_datasets WHERE id = $1`,
		id,
	).Scan(&ds.ID, &ds.Name, &ds.Description, &ds.Status, &ds.CreatedBy, &ds.CreatedAt, &ds.UpdatedAt)
	if err != nil {
		return nil, mapEvalDBError(err, "查询数据集")
	}
	return &ds, nil
}

// AddItem 向数据集添加评测条目。
func (r *PGRepository) AddItem(ctx context.Context, datasetID, query string, expectedDocs []ExpectedDocument, relevanceGrade int, queryClass string) (*Item, error) {
	docsJSON, err := json.Marshal(expectedDocs)
	if err != nil {
		return nil, fmt.Errorf("序列化 expected_documents: %w", err)
	}

	var item Item
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO evaluation_items (dataset_id, query, expected_documents, relevance_grade, query_class)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, dataset_id, query, expected_documents, relevance_grade, query_class,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		datasetID, query, []byte(docsJSON), relevanceGrade, queryClass,
	).Scan(&item.ID, &item.DatasetID, &item.Query, &item.ExpectedDocuments, &item.RelevanceGrade, &item.QueryClass, &item.CreatedAt)
	if err != nil {
		return nil, mapEvalDBError(err, "添加评测条目")
	}
	return &item, nil
}

// ListItems 查询指定数据集的条目列表（分页）。
func (r *PGRepository) ListItems(ctx context.Context, datasetID string, limit, offset int) (*ListItemsResult, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_items WHERE dataset_id = $1`,
		datasetID,
	).Scan(&total)
	if err != nil {
		return nil, mapEvalDBError(err, "查询条目总数")
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, dataset_id, query, expected_documents, relevance_grade, query_class,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM evaluation_items WHERE dataset_id = $1
		 ORDER BY created_at ASC LIMIT $2 OFFSET $3`,
		datasetID, limit, offset,
	)
	if err != nil {
		return nil, mapEvalDBError(err, "查询条目列表")
	}
	defer rows.Close()

	items := make([]Item, 0)
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.DatasetID, &item.Query, &item.ExpectedDocuments, &item.RelevanceGrade, &item.QueryClass, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描条目行: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历条目结果集: %w", err)
	}

	return &ListItemsResult{Items: items, Total: total}, nil
}

// ListAllItems 查询指定数据集的全部条目（不分页）。
// 引入动机：evaluate_profile job 需要读取全部评测条目执行评测，分页不适用。
func (r *PGRepository) ListAllItems(ctx context.Context, datasetID string) ([]Item, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, dataset_id, query, expected_documents, relevance_grade, query_class,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM evaluation_items WHERE dataset_id = $1
		 ORDER BY created_at ASC`,
		datasetID,
	)
	if err != nil {
		return nil, mapEvalDBError(err, "查询全部条目")
	}
	defer rows.Close()

	items := make([]Item, 0)
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.DatasetID, &item.Query, &item.ExpectedDocuments, &item.RelevanceGrade, &item.QueryClass, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("扫描条目行: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历条目结果集: %w", err)
	}

	return items, nil
}

// DeleteItem 删除指定数据集中的指定条目。
// WHERE 同时限定 id 与 dataset_id：仅知道条目 ID 不足以删除，
// 防止跨数据集越权删除。影响 0 行时返回 sql.ErrNoRows，由 handler 映射为 404。
func (r *PGRepository) DeleteItem(ctx context.Context, datasetID, itemID string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM evaluation_items WHERE id = $1 AND dataset_id = $2`,
		itemID, datasetID,
	)
	if err != nil {
		return mapEvalDBError(err, "删除评测条目")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取删除评测条目影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ArchiveDataset 将指定数据集软删为 archived（保留 items 与 evaluation_results 历史）。
// 引入动机：物理 DELETE 会经 ON DELETE CASCADE 连带、不可恢复地删除历史评测结果；
// 软删仅 UPDATE status 字段，数据保留、可审计。
// 幂等：对已 archived 的数据集重复调用仍返回成功（影响 1 行）。
// 影响 0 行时返回 sql.ErrNoRows，由 handler 映射为 404。
func (r *PGRepository) ArchiveDataset(ctx context.Context, datasetID string) error {
	result, err := r.db.ExecContext(ctx,
		`UPDATE evaluation_datasets SET status = 'archived', updated_at = now() WHERE id = $1`,
		datasetID,
	)
	if err != nil {
		return mapEvalDBError(err, "归档评测数据集")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取归档评测数据集影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// mapEvalDBError 将 database/sql 错误映射为带上下文的错误信息。
func mapEvalDBError(err error, context string) error {
	if err == sql.ErrNoRows {
		return err
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return fmt.Errorf("%s: PostgreSQL 错误 %s: %w", context, pgErr.Code, err)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// joinConditions 用 AND 连接 WHERE 条件。
func joinConditions(conditions []string) string {
	result := ""
	for i, c := range conditions {
		if i > 0 {
			result += " AND "
		}
		result += c
	}
	return result
}
