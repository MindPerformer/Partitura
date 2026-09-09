// repository.go 实现 document 模块的 PostgreSQL 数据访问层。
//
// 引入动机：文档 CRUD、revision 存储、source 管理、乐观并发控制需要访问
// documents、revisions、sources 三张表。将数据访问集中到 repository，
// 使领域逻辑和 HTTP handler 不直接依赖 SQL。
//
// 设计原则：
//   - 接口定义与实现分离，领域逻辑依赖接口而非具体 PG 实现
//   - 所有 workspace-scoped 查询 WHERE 子句必须包含 workspace_id
//   - 乐观并发通过 WHERE revision_number = $expected AND content_hash = $expected 实现
//   - 每次变更在同一事务中写入 revision snapshot
//   - 错误包装具体上下文，不吞错
package document

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Repository 定义 document 模块所需的数据访问接口。
// 引入动机：领域逻辑和 HTTP handler 依赖此接口而非具体 PG 实现，
// 便于测试时注入 mock 或使用替代存储。
type Repository interface {
	// CreateDocument 在指定 workspace 中创建文档，并在同一事务中写入初始 revision。
	// 引入动机：design/03-DOCUMENTS.md §Revision 要求每次 create 产生 revision。
	// 保证文档和初始 revision 原子创建。
	//
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中 enqueue index_document job，
	// 保证文档写入和 job 入队原子性——事务提交后 job 可见，回滚则两者都不存在。
	//
	// 参数：
	//   - ctx：请求 context
	//   - workspaceID：workspace UUID
	//   - path：文档路径（已通过 ValidatePath 校验）
	//   - title：文档标题
	//   - docType：文档类型（可为空）
	//   - contentMarkdown：Markdown 正文
	//   - contentHash：content 的 SHA-256 摘要
	//   - isSpecial：是否为特殊文件
	//   - createdBy：创建者用户 UUID
	//   - jobEnq：事务内 job enqueuer（可为 nil，表示不 enqueue index job）
	//
	// 返回创建的 Document 记录。
	CreateDocument(ctx context.Context, workspaceID, path, title, docType, contentMarkdown, contentHash string, isSpecial bool, createdBy string, jobEnq TxJobEnqueuer) (*Document, error)

	// GetDocumentByPath 根据 workspace_id + path 查询当前文档。
	// 引入动机：read/outline/section/lines/update/move/archive 等 API 需要按路径定位文档。
	// 不存在返回 sql.ErrNoRows。
	GetDocumentByPath(ctx context.Context, workspaceID, path string) (*Document, error)

	// GetDocumentByID 根据 workspace_id + id 查询文档。
	// 引入动机：source attach 等 API 需要按 ID 定位文档。
	// 不存在返回 sql.ErrNoRows。
	GetDocumentByID(ctx context.Context, workspaceID, documentID string) (*Document, error)

	// ListDocuments 查询指定 workspace 的文档列表（分页），可按 status/type 过滤。
	// 引入动机：design/04-WEB-API.md §Document 要求 list endpoint，默认隐藏 archived。
	//
	// 参数：
	//   - workspaceID：workspace UUID
	//   - statusFilter：状态过滤（可为空表示不过滤）
	//   - typeFilter：类型过滤（可为空表示不过滤）
	//   - includeArchived：是否包含已归档文档
	//   - limit/offset：分页参数
	ListDocuments(ctx context.Context, workspaceID, statusFilter, typeFilter string, includeArchived bool, limit, offset int) (*ListDocumentsResult, error)

	// ReplaceDocumentFull 在单一 PG 事务中原子性地替换文档的全文和元数据。
	// 引入动机：design/04-WEB-API.md §Concurrency 要求 replace 操作必须携带
	// expected_revision + expected_hash，且整个替换（content + title + type）必须在
	// 同一事务中完成，写入恰好一条完整 snapshot revision。
	// 之前的实现将内容更新和元数据更新拆成两次 repository 调用，导致两条 revision、
	// 两事务，中间可被并发修改产生半成品。此方法消除该风险。
	//
	// 行为：
	//   - 开启事务，SELECT ... FOR UPDATE 锁定文档行
	//   - 校验 expected_revision 和 expected_hash，不匹配返回 ErrRevisionConflict
	//   - 拒绝对 archived 文档的替换（返回 sql.ErrNoRows）
	//   - 一次性更新 content_markdown、content_hash、title、type、revision_number、updated_by、updated_at
	//   - 写入恰好一条包含新 content/title/type 的完整 revision snapshot
	//   - 任何检查或更新失败均回滚整个事务
	//
	// 参数：
	//   - ctx：请求 context
	//   - workspaceID：workspace UUID
	//   - path：文档路径
	//   - newContent：新 Markdown 正文
	//   - newHash：新 content 的 SHA-256 摘要
	//   - newTitle：新标题
	//   - newType：新类型（可为空表示清除 type）
	//   - expectedRevision：客户端持有的期望 revision 号
	//   - expectedHash：客户端持有的期望 content hash
	//   - updatedBy：执行更新的用户 UUID
	//
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中 enqueue index_document job。
	//
	// 返回更新后的 Document。失配返回 ErrRevisionConflict，文档不存在返回 sql.ErrNoRows。
	ReplaceDocumentFull(ctx context.Context, workspaceID, path, newContent, newHash, newTitle, newType string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error)

	// UpdateDocumentContent 更新文档正文（replace 或 patch 结果），使用乐观并发控制。
	// 引入动机：design/04-WEB-API.md §Concurrency 要求 expected_revision + expected_hash 匹配才更新。
	// 在同一事务中写入新 revision snapshot。
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中 enqueue index_document job。
	// 不匹配返回 ErrRevisionConflict。
	UpdateDocumentContent(ctx context.Context, workspaceID, path, newContent, newHash string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error)

	// UpdateDocumentMetadata 更新文档的 title 和 type（不改变 content）。
	// 引入动机：replace 操作可能同时更新 title/type。
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中 enqueue index_document job。
	// 在同一事务中写入新 revision snapshot。
	UpdateDocumentMetadata(ctx context.Context, workspaceID, path, newTitle, newType string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error)

	// MoveDocument 移动文档路径，使用乐观并发控制。
	// 引入动机：design/04-WEB-API.md §Document 要求 move endpoint。
	// 在同一事务中写入新 revision snapshot（记录新 path）。
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中 enqueue index_document job。
	// 新路径重复返回唯一约束冲突错误。
	MoveDocument(ctx context.Context, workspaceID, oldPath, newPath string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error)

	// ArchiveDocument 将文档状态设置为 archived，使用乐观并发控制。
	// 引入动机：design/03-DOCUMENTS.md §Archive 要求默认删除操作是 archive。
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中 enqueue index_document job。
	// 在同一事务中写入新 revision snapshot。
	ArchiveDocument(ctx context.Context, workspaceID, path string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error)

	// RestoreDocument 将文档状态从 archived 恢复为 active，使用乐观并发控制。
	// 引入动机：design/03-DOCUMENTS.md §Archive 要求 archived document 可恢复。
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中 enqueue index_document job。
	// 在同一事务中写入新 revision snapshot。
	RestoreDocument(ctx context.Context, workspaceID, path string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error)

	// PurgeDocument 永久删除文档及其全部 revision 和 source。
	// 引入动机：design/03-DOCUMENTS.md §Archive 要求 purge 仅 owner 可执行。
	// 级联删除由数据库 ON DELETE CASCADE 保证。
	PurgeDocument(ctx context.Context, workspaceID, documentID string) error

	// ListRevisions 查询指定文档的版本历史（分页）。
	// 引入动机：design/04-WEB-API.md §Document 要求 history endpoint。
	ListRevisions(ctx context.Context, workspaceID, documentID string, limit, offset int) (*ListRevisionsResult, error)

	// GetRevision 查询指定文档的指定版本。
	// 引入动机：design/04-WEB-API.md §Document 要求 revision endpoint。
	// 不存在返回 sql.ErrNoRows。
	GetRevision(ctx context.Context, workspaceID, documentID string, revisionNumber int) (*Revision, error)

	// AddSource 向指定文档添加来源。
	// 引入动机：design/03-DOCUMENTS.md §Source 要求 Document-level provenance。
	AddSource(ctx context.Context, workspaceID, documentID, sourceType, value, title, retrievedAt, contentHash string, refreshIntervalDays int, sourceDocumentID, createdBy string) (*Source, error)

	// ListSources 查询指定文档的来源列表（分页）。
	// 引入动机：design/04-WEB-API.md §Document 要求 sources endpoint。
	ListSources(ctx context.Context, workspaceID, documentID string, limit, offset int) (*ListSourcesResult, error)

	// DeleteSource 删除指定来源。
	// 引入动机：design/04-WEB-API.md §Document 要求 source delete endpoint。
	DeleteSource(ctx context.Context, workspaceID, sourceID string) error

	// GetWorkspaceMaxDocumentSize 查询 workspace 的 max_document_size_bytes。
	// 引入动机：创建/更新文档时需要验证 content 大小限制。
	GetWorkspaceMaxDocumentSize(ctx context.Context, workspaceID string) (int, error)

	// GetWorkspaceRetentionConfig 查询 workspace 的 revision 保留配置。
	// 引入动机：retention 清理需要知道 retention_days 和 max_count。
	GetWorkspaceRetentionConfig(ctx context.Context, workspaceID string) (retentionDays int, maxCount int, err error)

	// CleanupRevisions 清理指定文档的过期 revision，保留当前版本。
	// 引入动机：design/03-DOCUMENTS.md §Revision 要求 retention 清理，当前版本永不删除。
	// 历史版本满足 retention_days 或 max_count 任一上限后允许清理。
	CleanupRevisions(ctx context.Context, workspaceID, documentID string, retentionDays, maxCount int) (int, error)

	// CreateSpecialDocuments 在指定 workspace 中创建 PROJECT.md 和 AGENTS.md。
	// 引入动机：design/00-MASTER.md §特殊文件 要求 workspace 创建时自动创建特殊文件。
	// 在调用者提供的事务中执行，保证与 workspace 创建原子性。
	// C1 事务一致性：如果 jobEnq 非空，在同一事务中为每个特殊文档 enqueue index_document job。
	CreateSpecialDocuments(ctx context.Context, tx *sql.Tx, workspaceID, createdBy string, jobEnq TxJobEnqueuer) error
}

// ErrRevisionConflict 表示乐观并发冲突。
// 引入动机：design/04-WEB-API.md §Concurrency 要求不匹配返回 HTTP 409。
var ErrRevisionConflict = fmt.Errorf("revision conflict: expected revision/hash does not match current")

// PGRepository 是 Repository 接口的 PostgreSQL 实现。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// CreateDocument 在指定 workspace 中创建文档，并在同一事务中写入初始 revision。
// C1：如果 jobEnq 非空，在同一事务中 enqueue index_document job。
func (r *PGRepository) CreateDocument(ctx context.Context, workspaceID, path, title, docType, contentMarkdown, contentHash string, isSpecial bool, createdBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启创建文档事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// 处理可空的 type 字段
	var docTypeVal interface{}
	if docType != "" {
		docTypeVal = docType
	}

	var doc Document
	err = tx.QueryRowContext(ctx,
		`INSERT INTO documents (workspace_id, path, title, type, content_markdown, content_hash, revision_number, is_special, created_by, updated_by)
		 VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $8, $8)
		 RETURNING id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		workspaceID, path, title, docTypeVal, contentMarkdown, contentHash, isSpecial, createdBy,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "创建文档")
	}

	// 写入初始 revision
	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, 1, $3, $4, $5, $6, $7, $8)`,
		doc.ID, workspaceID, path, title, contentMarkdown, contentHash, doc.Status, createdBy,
	)
	if err != nil {
		return nil, mapDBError(err, "写入初始 revision")
	}

	// C1：在同一事务中 enqueue index_document job
	if jobEnq != nil {
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(doc.ID)); err != nil {
			return nil, fmt.Errorf("事务内 enqueue index job: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交创建文档事务: %w", err)
	}

	return &doc, nil
}

// GetDocumentByPath 根据 workspace_id + path 查询当前文档。
func (r *PGRepository) GetDocumentByPath(ctx context.Context, workspaceID, path string) (*Document, error) {
	var doc Document
	err := r.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE workspace_id = $1 AND path = $2`,
		workspaceID, path,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "根据路径查询文档")
	}
	return &doc, nil
}

// GetDocumentByID 根据 workspace_id + id 查询文档。
func (r *PGRepository) GetDocumentByID(ctx context.Context, workspaceID, documentID string) (*Document, error) {
	var doc Document
	err := r.db.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE workspace_id = $1 AND id = $2`,
		workspaceID, documentID,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "根据 ID 查询文档")
	}
	return &doc, nil
}

// ListDocuments 查询指定 workspace 的文档列表（分页），可按 status/type 过滤。
func (r *PGRepository) ListDocuments(ctx context.Context, workspaceID, statusFilter, typeFilter string, includeArchived bool, limit, offset int) (*ListDocumentsResult, error) {
	// 构建动态 WHERE 条件
	conditions := []string{"workspace_id = $1"}
	args := []interface{}{workspaceID}
	argIdx := 2

	if !includeArchived {
		conditions = append(conditions, "status != 'archived'")
	}
	if statusFilter != "" {
		conditions = append(conditions, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, statusFilter)
		argIdx++
	}
	if typeFilter != "" {
		conditions = append(conditions, fmt.Sprintf("type = $%d", argIdx))
		args = append(args, typeFilter)
		argIdx++
	}

	whereClause := strings.Join(conditions, " AND ")

	// 查询总数
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM documents WHERE %s", whereClause)
	var total int
	err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询文档总数")
	}

	// 查询分页数据
	// 引入动机：list API 不返回全文，减少响应体积和数据库 I/O。
	// 不 SELECT content_markdown 列，避免加载大字段。
	listQuery := fmt.Sprintf(
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE %s ORDER BY path ASC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询文档列表")
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var doc Document
		if err := rows.Scan(
			&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
			&doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
			&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描文档行: %w", err)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历文档结果集: %w", err)
	}

	return &ListDocumentsResult{Documents: docs, Total: total}, nil
}

// ReplaceDocumentFull 在单一 PG 事务中原子性地替换文档的全文和元数据。
// 引入动机：handler ReplaceDocument 之前依次调用 UpdateDocumentContent 和
// UpdateDocumentMetadata，导致两条 revision、两事务，中间可被并发修改产生半成品。
// 此方法在单一事务中锁定行、校验 expected_revision/expected_hash、一次性更新
// content+hash+title+type+revision_number+updated_by/updated_at，并写入恰好一条
// 完整 snapshot revision。任何失败均回滚。
func (r *PGRepository) ReplaceDocumentFull(ctx context.Context, workspaceID, path, newContent, newHash, newTitle, newType string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启替换文档事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// 锁定文档行并校验并发版本
	var doc Document
	err = tx.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE workspace_id = $1 AND path = $2 FOR UPDATE`,
		workspaceID, path,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询待替换文档")
	}

	// 拒绝对 archived 文档的替换
	if doc.Status == StatusArchived {
		err = sql.ErrNoRows
		return nil, err
	}

	// 乐观并发校验
	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		err = ErrRevisionConflict
		return nil, err
	}

	newRevision := doc.RevisionNumber + 1

	// 处理可空的 type 字段
	var docTypeVal interface{}
	if newType != "" {
		docTypeVal = newType
	}

	err = tx.QueryRowContext(ctx,
		`UPDATE documents
		 SET content_markdown = $1, content_hash = $2, title = $3, type = $4,
		     revision_number = $5, updated_by = $6, updated_at = now()
		 WHERE workspace_id = $7 AND path = $8 AND revision_number = $9 AND content_hash = $10
		 RETURNING id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		newContent, newHash, newTitle, docTypeVal, newRevision, updatedBy,
		workspaceID, path, expectedRevision, expectedHash,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrRevisionConflict
		}
		return nil, mapDBError(err, "替换文档全文和元数据")
	}

	// 写入恰好一条完整 revision snapshot
	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		doc.ID, workspaceID, newRevision, doc.Path, doc.Title, newContent, newHash, doc.Status, updatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "写入替换 revision")
	}

	// C1：在同一事务中 enqueue index_document job
	if jobEnq != nil {
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(doc.ID)); err != nil {
			return nil, fmt.Errorf("事务内 enqueue index job: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交替换文档事务: %w", err)
	}

	return &doc, nil
}

// UpdateDocumentContent 更新文档正文，使用乐观并发控制。
func (r *PGRepository) UpdateDocumentContent(ctx context.Context, workspaceID, path, newContent, newHash string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启更新文档事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// 乐观并发：先查询当前文档，验证 revision + hash
	var doc Document
	err = tx.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE workspace_id = $1 AND path = $2 FOR UPDATE`,
		workspaceID, path,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询待更新文档")
	}

	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		err = ErrRevisionConflict
		return nil, err
	}

	newRevision := doc.RevisionNumber + 1

	err = tx.QueryRowContext(ctx,
		`UPDATE documents SET content_markdown = $1, content_hash = $2, revision_number = $3, updated_by = $4, updated_at = now()
		 WHERE workspace_id = $5 AND path = $6 AND revision_number = $7 AND content_hash = $8
		 RETURNING id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		newContent, newHash, newRevision, updatedBy, workspaceID, path, expectedRevision, expectedHash,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrRevisionConflict
		}
		return nil, mapDBError(err, "更新文档内容")
	}

	// 写入新 revision
	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		doc.ID, workspaceID, newRevision, doc.Path, doc.Title, newContent, newHash, doc.Status, updatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "写入更新 revision")
	}

	// C1：在同一事务中 enqueue index_document job
	if jobEnq != nil {
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(doc.ID)); err != nil {
			return nil, fmt.Errorf("事务内 enqueue index job: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交更新文档事务: %w", err)
	}

	return &doc, nil
}

// UpdateDocumentMetadata 更新文档的 title 和 type（不改变 content）。
func (r *PGRepository) UpdateDocumentMetadata(ctx context.Context, workspaceID, path, newTitle, newType string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启更新文档元数据事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var doc Document
	err = tx.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE workspace_id = $1 AND path = $2 FOR UPDATE`,
		workspaceID, path,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询待更新文档")
	}

	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		err = ErrRevisionConflict
		return nil, err
	}

	newRevision := doc.RevisionNumber + 1

	var docTypeVal interface{}
	if newType != "" {
		docTypeVal = newType
	}

	err = tx.QueryRowContext(ctx,
		`UPDATE documents SET title = $1, type = $2, revision_number = $3, updated_by = $4, updated_at = now()
		 WHERE workspace_id = $5 AND path = $6 AND revision_number = $7 AND content_hash = $8
		 RETURNING id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		newTitle, docTypeVal, newRevision, updatedBy, workspaceID, path, expectedRevision, expectedHash,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrRevisionConflict
		}
		return nil, mapDBError(err, "更新文档元数据")
	}

	// 写入新 revision（内容不变，但 title/type 可能变了）
	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		doc.ID, workspaceID, newRevision, doc.Path, doc.Title, doc.ContentMarkdown, doc.ContentHash, doc.Status, updatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "写入元数据更新 revision")
	}

	// C1：在同一事务中 enqueue index_document job
	if jobEnq != nil {
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(doc.ID)); err != nil {
			return nil, fmt.Errorf("事务内 enqueue index job: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交更新文档元数据事务: %w", err)
	}

	return &doc, nil
}

// MoveDocument 移动文档路径，使用乐观并发控制。
func (r *PGRepository) MoveDocument(ctx context.Context, workspaceID, oldPath, newPath string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启移动文档事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var doc Document
	err = tx.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE workspace_id = $1 AND path = $2 FOR UPDATE`,
		workspaceID, oldPath,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询待移动文档")
	}

	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		err = ErrRevisionConflict
		return nil, err
	}

	newRevision := doc.RevisionNumber + 1

	err = tx.QueryRowContext(ctx,
		`UPDATE documents SET path = $1, revision_number = $2, updated_by = $3, updated_at = now()
		 WHERE workspace_id = $4 AND path = $5 AND revision_number = $6 AND content_hash = $7
		 RETURNING id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		newPath, newRevision, updatedBy, workspaceID, oldPath, expectedRevision, expectedHash,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrRevisionConflict
		}
		return nil, mapDBError(err, "移动文档")
	}

	// 写入新 revision（记录新 path）
	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		doc.ID, workspaceID, newRevision, newPath, doc.Title, doc.ContentMarkdown, doc.ContentHash, doc.Status, updatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "写入移动 revision")
	}

	// C1：在同一事务中 enqueue index_document job
	if jobEnq != nil {
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(doc.ID)); err != nil {
			return nil, fmt.Errorf("事务内 enqueue index job: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交移动文档事务: %w", err)
	}

	return &doc, nil
}

// ArchiveDocument 将文档状态设置为 archived，使用乐观并发控制。
func (r *PGRepository) ArchiveDocument(ctx context.Context, workspaceID, path string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	return r.changeDocumentStatus(ctx, workspaceID, path, StatusArchived, expectedRevision, expectedHash, updatedBy, jobEnq)
}

// RestoreDocument 将文档状态从 archived 恢复为 active，使用乐观并发控制。
func (r *PGRepository) RestoreDocument(ctx context.Context, workspaceID, path string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	return r.changeDocumentStatus(ctx, workspaceID, path, StatusActive, expectedRevision, expectedHash, updatedBy, jobEnq)
}

// changeDocumentStatus 通用状态变更方法，使用乐观并发控制并写入 revision。
// 引入动机：archive 和 restore 共享相同的事务+并发控制+revision 逻辑。
//
// 幂等行为：如果文档已经处于目标状态，不新增 revision，直接返回当前文档。
// 引入动机：design/03-DOCUMENTS.md §Archive 要求 archived document 可恢复，
// 对已处于目标状态的文档再次执行 archive/restore 不应产生多余 revision，
// 保持受控幂等响应。
func (r *PGRepository) changeDocumentStatus(ctx context.Context, workspaceID, path, newStatus string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启文档状态变更事务: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var doc Document
	err = tx.QueryRowContext(ctx,
		`SELECT id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM documents WHERE workspace_id = $1 AND path = $2 FOR UPDATE`,
		workspaceID, path,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询待变更状态文档")
	}

	// 幂等：文档已处于目标状态，不新增 revision，直接返回
	if doc.Status == newStatus {
		// 仍需校验 expected_revision/expected_hash 以保证调用者持有正确版本
		if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
			err = ErrRevisionConflict
			return nil, err
		}
		// 回滚空事务（未做任何变更）
		_ = tx.Rollback()
		// 将 err 置 nil 以跳过 defer 中的二次回滚
		err = nil
		return &doc, nil
	}

	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		err = ErrRevisionConflict
		return nil, err
	}

	newRevision := doc.RevisionNumber + 1

	err = tx.QueryRowContext(ctx,
		`UPDATE documents SET status = $1, revision_number = $2, updated_by = $3, updated_at = now()
		 WHERE workspace_id = $4 AND path = $5 AND revision_number = $6 AND content_hash = $7
		 RETURNING id, workspace_id, path, title, COALESCE(type, ''), status, content_markdown, content_hash, revision_number, is_special, created_by, updated_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		newStatus, newRevision, updatedBy, workspaceID, path, expectedRevision, expectedHash,
	).Scan(
		&doc.ID, &doc.WorkspaceID, &doc.Path, &doc.Title, &doc.Type, &doc.Status,
		&doc.ContentMarkdown, &doc.ContentHash, &doc.RevisionNumber, &doc.IsSpecial,
		&doc.CreatedBy, &doc.UpdatedBy, &doc.CreatedAt, &doc.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrRevisionConflict
		}
		return nil, mapDBError(err, "变更文档状态")
	}

	// 写入新 revision
	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		doc.ID, workspaceID, newRevision, doc.Path, doc.Title, doc.ContentMarkdown, doc.ContentHash, doc.Status, updatedBy,
	)
	if err != nil {
		return nil, mapDBError(err, "写入状态变更 revision")
	}

	// C1：在同一事务中 enqueue index_document job
	if jobEnq != nil {
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(doc.ID)); err != nil {
			return nil, fmt.Errorf("事务内 enqueue index job: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交文档状态变更事务: %w", err)
	}

	return &doc, nil
}

// PurgeDocument 永久删除文档及其全部 revision 和 source。
func (r *PGRepository) PurgeDocument(ctx context.Context, workspaceID, documentID string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM documents WHERE workspace_id = $1 AND id = $2`,
		workspaceID, documentID,
	)
	if err != nil {
		return mapDBError(err, "永久删除文档")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取永久删除文档影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListRevisions 查询指定文档的版本历史（分页）。
func (r *PGRepository) ListRevisions(ctx context.Context, workspaceID, documentID string, limit, offset int) (*ListRevisionsResult, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM revisions WHERE workspace_id = $1 AND document_id = $2`,
		workspaceID, documentID,
	).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询版本总数")
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM revisions WHERE workspace_id = $1 AND document_id = $2
		 ORDER BY revision_number DESC LIMIT $3 OFFSET $4`,
		workspaceID, documentID, limit, offset,
	)
	if err != nil {
		return nil, mapDBError(err, "查询版本列表")
	}
	defer rows.Close()

	var revs []Revision
	for rows.Next() {
		var rev Revision
		if err := rows.Scan(
			&rev.ID, &rev.DocumentID, &rev.WorkspaceID, &rev.RevisionNumber,
			&rev.Path, &rev.Title, &rev.ContentMarkdown, &rev.ContentHash,
			&rev.Status, &rev.CreatedBy, &rev.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描版本行: %w", err)
		}
		revs = append(revs, rev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历版本结果集: %w", err)
	}

	return &ListRevisionsResult{Revisions: revs, Total: total}, nil
}

// GetRevision 查询指定文档的指定版本。
func (r *PGRepository) GetRevision(ctx context.Context, workspaceID, documentID string, revisionNumber int) (*Revision, error) {
	var rev Revision
	err := r.db.QueryRowContext(ctx,
		`SELECT id, document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM revisions WHERE workspace_id = $1 AND document_id = $2 AND revision_number = $3`,
		workspaceID, documentID, revisionNumber,
	).Scan(
		&rev.ID, &rev.DocumentID, &rev.WorkspaceID, &rev.RevisionNumber,
		&rev.Path, &rev.Title, &rev.ContentMarkdown, &rev.ContentHash,
		&rev.Status, &rev.CreatedBy, &rev.CreatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询指定版本")
	}
	return &rev, nil
}

// AddSource 向指定文档添加来源。
func (r *PGRepository) AddSource(ctx context.Context, workspaceID, documentID, sourceType, value, title, retrievedAt, contentHash string, refreshIntervalDays int, sourceDocumentID, createdBy string) (*Source, error) {
	var src Source
	var retrievedAtVal interface{}
	if retrievedAt != "" {
		retrievedAtVal = retrievedAt
	}
	var contentHashVal interface{}
	if contentHash != "" {
		contentHashVal = contentHash
	}
	var refreshVal interface{}
	if refreshIntervalDays > 0 {
		refreshVal = refreshIntervalDays
	}
	var srcDocIDVal interface{}
	if sourceDocumentID != "" {
		srcDocIDVal = sourceDocumentID
	}

	err := r.db.QueryRowContext(ctx,
		`INSERT INTO sources (document_id, workspace_id, source_type, value, title, retrieved_at, content_hash, refresh_interval_days, source_document_id, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id, document_id, workspace_id, source_type, value, title,
		           COALESCE(to_char(retrieved_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
		           COALESCE(content_hash, ''),
		           COALESCE(refresh_interval_days, 0),
		           COALESCE(source_document_id::text, ''),
		           created_by,
		           to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
		documentID, workspaceID, sourceType, value, title, retrievedAtVal, contentHashVal, refreshVal, srcDocIDVal, createdBy,
	).Scan(
		&src.ID, &src.DocumentID, &src.WorkspaceID, &src.SourceType, &src.Value, &src.Title,
		&src.RetrievedAt, &src.ContentHash, &src.RefreshIntervalDays, &src.SourceDocumentID,
		&src.CreatedBy, &src.CreatedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "添加来源")
	}
	return &src, nil
}

// ListSources 查询指定文档的来源列表（分页）。
func (r *PGRepository) ListSources(ctx context.Context, workspaceID, documentID string, limit, offset int) (*ListSourcesResult, error) {
	var total int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sources WHERE workspace_id = $1 AND document_id = $2`,
		workspaceID, documentID,
	).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询来源总数")
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT id, document_id, workspace_id, source_type, value, title,
		        COALESCE(to_char(retrieved_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
		        COALESCE(content_hash, ''),
		        COALESCE(refresh_interval_days, 0),
		        COALESCE(source_document_id::text, ''),
		        created_by,
		        to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM sources WHERE workspace_id = $1 AND document_id = $2
		 ORDER BY created_at ASC LIMIT $3 OFFSET $4`,
		workspaceID, documentID, limit, offset,
	)
	if err != nil {
		return nil, mapDBError(err, "查询来源列表")
	}
	defer rows.Close()

	var sources []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(
			&src.ID, &src.DocumentID, &src.WorkspaceID, &src.SourceType, &src.Value, &src.Title,
			&src.RetrievedAt, &src.ContentHash, &src.RefreshIntervalDays, &src.SourceDocumentID,
			&src.CreatedBy, &src.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描来源行: %w", err)
		}
		sources = append(sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历来源结果集: %w", err)
	}

	return &ListSourcesResult{Sources: sources, Total: total}, nil
}

// DeleteSource 删除指定来源。
func (r *PGRepository) DeleteSource(ctx context.Context, workspaceID, sourceID string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM sources WHERE workspace_id = $1 AND id = $2`,
		workspaceID, sourceID,
	)
	if err != nil {
		return mapDBError(err, "删除来源")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取删除来源影响行数: %w", err)
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetWorkspaceMaxDocumentSize 查询 workspace 的 max_document_size_bytes。
func (r *PGRepository) GetWorkspaceMaxDocumentSize(ctx context.Context, workspaceID string) (int, error) {
	var maxSize int
	err := r.db.QueryRowContext(ctx,
		`SELECT max_document_size_bytes FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&maxSize)
	if err != nil {
		return 0, mapDBError(err, "查询 workspace 文档大小限制")
	}
	return maxSize, nil
}

// GetWorkspaceRetentionConfig 查询 workspace 的 revision 保留配置。
func (r *PGRepository) GetWorkspaceRetentionConfig(ctx context.Context, workspaceID string) (int, int, error) {
	var retentionDays, maxCount int
	err := r.db.QueryRowContext(ctx,
		`SELECT revision_retention_days, revision_max_count FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&retentionDays, &maxCount)
	if err != nil {
		return 0, 0, mapDBError(err, "查询 workspace retention 配置")
	}
	return retentionDays, maxCount, nil
}

// CleanupRevisions 清理指定文档的过期 revision，保留当前版本。
func (r *PGRepository) CleanupRevisions(ctx context.Context, workspaceID, documentID string, retentionDays, maxCount int) (int, error) {
	// 获取当前 revision_number
	var currentRevision int
	err := r.db.QueryRowContext(ctx,
		`SELECT revision_number FROM documents WHERE workspace_id = $1 AND id = $2`,
		workspaceID, documentID,
	).Scan(&currentRevision)
	if err != nil {
		return 0, mapDBError(err, "查询当前 revision 号")
	}

	// 删除满足任一条件的旧 revision：
	// 1. revision_number < currentRevision AND created_at < now() - interval 'N days'
	// 2. revision_number < currentRevision AND revision_number <= currentRevision - maxCount
	// 当前版本（revision_number = currentRevision）永不清除
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM revisions
		 WHERE workspace_id = $1 AND document_id = $2
		 AND revision_number < $3
		 AND (
		   created_at < now() - make_interval(days => $4)
		   OR revision_number <= $3 - $5
		 )`,
		workspaceID, documentID, currentRevision, retentionDays, maxCount,
	)
	if err != nil {
		return 0, mapDBError(err, "清理过期 revision")
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("获取清理 revision 影响行数: %w", err)
	}
	slog.Info("revision 清理完成", "workspace_id", workspaceID, "document_id", documentID, "cleaned", rows, "current_revision", currentRevision)
	return int(rows), nil
}

// CreateSpecialDocuments 在指定 workspace 中创建 PROJECT.md 和 AGENTS.md。
// 在调用者提供的事务中执行，保证与 workspace 创建原子性。
// C1：如果 jobEnq 非空，在同一事务中为每个特殊文档 enqueue index_document job。
func (r *PGRepository) CreateSpecialDocuments(ctx context.Context, tx *sql.Tx, workspaceID, createdBy string, jobEnq TxJobEnqueuer) error {
	// PROJECT.md 初始内容
	projectContent := "# Project\n\nProject description goes here.\n"
	projectHash := ComputeContentHash(projectContent)

	_, err := tx.ExecContext(ctx,
		`INSERT INTO documents (workspace_id, path, title, type, content_markdown, content_hash, revision_number, is_special, created_by, updated_by)
		 VALUES ($1, $2, $3, NULL, $4, $5, 1, TRUE, $6, $6)`,
		workspaceID, SpecialFileProject, "Project", projectContent, projectHash, createdBy,
	)
	if err != nil {
		return mapDBError(err, "创建 PROJECT.md")
	}

	// 写入 PROJECT.md 初始 revision
	var projectDocID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM documents WHERE workspace_id = $1 AND path = $2`,
		workspaceID, SpecialFileProject,
	).Scan(&projectDocID)
	if err != nil {
		return mapDBError(err, "查询 PROJECT.md ID")
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, 1, $3, $4, $5, $6, 'active', $7)`,
		projectDocID, workspaceID, SpecialFileProject, "Project", projectContent, projectHash, createdBy,
	)
	if err != nil {
		return mapDBError(err, "写入 PROJECT.md 初始 revision")
	}

	// AGENTS.md 初始内容
	agentsContent := "# Agents\n\nAgent guidelines go here.\n"
	agentsHash := ComputeContentHash(agentsContent)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO documents (workspace_id, path, title, type, content_markdown, content_hash, revision_number, is_special, created_by, updated_by)
		 VALUES ($1, $2, $3, NULL, $4, $5, 1, TRUE, $6, $6)`,
		workspaceID, SpecialFileAgents, "Agents", agentsContent, agentsHash, createdBy,
	)
	if err != nil {
		return mapDBError(err, "创建 AGENTS.md")
	}

	// 写入 AGENTS.md 初始 revision
	var agentsDocID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM documents WHERE workspace_id = $1 AND path = $2`,
		workspaceID, SpecialFileAgents,
	).Scan(&agentsDocID)
	if err != nil {
		return mapDBError(err, "查询 AGENTS.md ID")
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, 1, $3, $4, $5, $6, 'active', $7)`,
		agentsDocID, workspaceID, SpecialFileAgents, "Agents", agentsContent, agentsHash, createdBy,
	)
	if err != nil {
		return mapDBError(err, "写入 AGENTS.md 初始 revision")
	}

	// C1：在同一事务中为特殊文档 enqueue index_document job
	if jobEnq != nil {
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(projectDocID)); err != nil {
			return fmt.Errorf("事务内 enqueue PROJECT.md index job: %w", err)
		}
		if _, err = jobEnq.EnqueueInTx(ctx, tx, "index_document", enqueueIndexJobPayload(agentsDocID)); err != nil {
			return fmt.Errorf("事务内 enqueue AGENTS.md index job: %w", err)
		}
	}

	return nil
}
func mapDBError(err error, context string) error {
	if err == sql.ErrNoRows {
		return err
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return fmt.Errorf("%s: PostgreSQL 错误 %s: %w", context, pgErr.Code, err)
	}
	return fmt.Errorf("%s: %w", context, err)
}
