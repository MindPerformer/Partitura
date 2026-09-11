// mock_repo_test.go 提供 document.Repository 接口的内存 mock 实现，用于 handler 测试。
//
// 引入动机：HTTP handler 和 middleware 测试需要在不依赖 PostgreSQL 的情况下
// 验证 document RBAC、并发控制、patch 唯一匹配等逻辑。
// mock repository 在内存中模拟数据库行为，包括乐观并发控制和 revision 计数。
package document

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

// MockRepository 是 Repository 接口的内存 mock 实现。
// 引入动机：测试 handler/middleware 时注入，避免依赖真实数据库。
type MockRepository struct {
	mu sync.Mutex

	// documents 按 "workspaceID:path" 索引
	docs map[string]*Document
	// revisions 按 "workspaceID:docID" 索引，按 revision_number 排序
	revisions map[string][]Revision
	// sources 按 "workspaceID:docID" 索引
	sources map[string][]Source
	// workspaceMaxSize 按 workspaceID 索引
	workspaceMaxSize map[string]int
	// workspaceRetention 按 workspaceID 索引 [retentionDays, maxCount]
	workspaceRetention map[string][2]int

	// 自增计数器
	docIDCounter    int
	revisionCounter int
	sourceIDCounter int
}

// NewMockRepository 创建空 mock repository。
func NewMockRepository() *MockRepository {
	return &MockRepository{
		docs:               make(map[string]*Document),
		revisions:          make(map[string][]Revision),
		sources:            make(map[string][]Source),
		workspaceMaxSize:   make(map[string]int),
		workspaceRetention: make(map[string][2]int),
	}
}

// SetWorkspaceMaxSize 设置 mock workspace 的文档大小限制。
// 引入动机：测试需要可配置的 workspace 大小限制。
func (m *MockRepository) SetWorkspaceMaxSize(workspaceID string, maxSize int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workspaceMaxSize[workspaceID] = maxSize
}

// SetWorkspaceRetention 设置 mock workspace 的 revision 保留配置。
func (m *MockRepository) SetWorkspaceRetention(workspaceID string, retentionDays, maxCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workspaceRetention[workspaceID] = [2]int{retentionDays, maxCount}
}

// GetRevisionCount 返回指定文档的 revision 数量（测试辅助函数）。
func (m *MockRepository) GetRevisionCount(workspaceID, docID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.revisions[workspaceID+":"+docID])
}

func docKey(workspaceID, path string) string {
	return workspaceID + ":" + path
}

func revKey(workspaceID, docID string) string {
	return workspaceID + ":" + docID
}

func srcKey(workspaceID, docID string) string {
	return workspaceID + ":" + docID
}

// CreateDocument 实现 Repository 接口。
func (m *MockRepository) CreateDocument(ctx context.Context, workspaceID, path, title, docType, contentMarkdown, contentHash string, isSpecial bool, createdBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := docKey(workspaceID, path)
	if _, exists := m.docs[k]; exists {
		return nil, &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	}

	m.docIDCounter++
	docID := fmt.Sprintf("doc-%d", m.docIDCounter)
	doc := &Document{
		ID:              docID,
		WorkspaceID:     workspaceID,
		Path:            path,
		Title:           title,
		Type:            docType,
		Status:          StatusActive,
		ContentMarkdown: contentMarkdown,
		ContentHash:     contentHash,
		RevisionNumber:  1,
		IsSpecial:       isSpecial,
		CreatedBy:       createdBy,
		UpdatedBy:       createdBy,
		// mock 语义：测试用户 ID 即 username，使 handler 断言新字段非空且等于操作者。
		CreatedByUsername: createdBy,
		UpdatedByUsername: createdBy,
		CreatedAt:       "2025-01-01T00:00:00+00:00",
		UpdatedAt:       "2025-01-01T00:00:00+00:00",
	}
	m.docs[k] = doc

	// 写入初始 revision
	m.revisionCounter++
	rev := Revision{
		ID:              fmt.Sprintf("rev-%d", m.revisionCounter),
		DocumentID:      docID,
		WorkspaceID:     workspaceID,
		RevisionNumber:  1,
		Path:            path,
		Title:           title,
		ContentMarkdown: contentMarkdown,
		ContentHash:     contentHash,
		Status:          StatusActive,
		CreatedBy:       createdBy,
		CreatedByUsername: createdBy,
		CreatedAt:       "2025-01-01T00:00:00+00:00",
	}
	m.revisions[revKey(workspaceID, docID)] = append(m.revisions[revKey(workspaceID, docID)], rev)

	copied := *doc
	return &copied, nil
}

// GetDocumentByPath 实现 Repository 接口。
func (m *MockRepository) GetDocumentByPath(ctx context.Context, workspaceID, path string) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.docs[docKey(workspaceID, path)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copied := *doc
	return &copied, nil
}

// GetDocumentByID 实现 Repository 接口。
func (m *MockRepository) GetDocumentByID(ctx context.Context, workspaceID, documentID string) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, doc := range m.docs {
		if doc.WorkspaceID == workspaceID && doc.ID == documentID {
			copied := *doc
			return &copied, nil
		}
	}
	return nil, sql.ErrNoRows
}

// ListDocuments 实现 Repository 接口。
func (m *MockRepository) ListDocuments(ctx context.Context, workspaceID, statusFilter, typeFilter string, includeArchived bool, limit, offset int) (*ListDocumentsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var docs []Document
	for _, doc := range m.docs {
		if doc.WorkspaceID != workspaceID {
			continue
		}
		if !includeArchived && doc.Status == StatusArchived {
			continue
		}
		if statusFilter != "" && doc.Status != statusFilter {
			continue
		}
		if typeFilter != "" && doc.Type != typeFilter {
			continue
		}
		// 列表不返回 content_markdown
		copied := *doc
		copied.ContentMarkdown = ""
		docs = append(docs, copied)
	}

	total := len(docs)
	if offset >= total {
		return &ListDocumentsResult{Documents: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return &ListDocumentsResult{Documents: docs[offset:end], Total: total}, nil
}

// ReplaceDocumentFull 实现 Repository 接口。
// 引入动机：原子性替换文档全文和元数据，在 mock 中模拟单事务行为。
func (m *MockRepository) ReplaceDocumentFull(ctx context.Context, workspaceID, path, newContent, newHash, newTitle, newType string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	doc, ok := m.docs[docKey(workspaceID, path)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if doc.Status == StatusArchived {
		return nil, sql.ErrNoRows
	}
	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		return nil, ErrRevisionConflict
	}

	newRevision := doc.RevisionNumber + 1
	doc.ContentMarkdown = newContent
	doc.ContentHash = newHash
	doc.Title = newTitle
	doc.Type = newType
	doc.RevisionNumber = newRevision
	doc.UpdatedBy = updatedBy
	doc.UpdatedByUsername = updatedBy

	// 写入恰好一条 revision
	m.revisionCounter++
	rev := Revision{
		ID:              fmt.Sprintf("rev-%d", m.revisionCounter),
		DocumentID:      doc.ID,
		WorkspaceID:     workspaceID,
		RevisionNumber:  newRevision,
		Path:            doc.Path,
		Title:           newTitle,
		ContentMarkdown: newContent,
		ContentHash:     newHash,
		Status:          doc.Status,
		CreatedBy:       updatedBy,
		CreatedByUsername: updatedBy,
		CreatedAt:       "2025-01-01T00:00:00+00:00",
	}
	m.revisions[revKey(workspaceID, doc.ID)] = append(m.revisions[revKey(workspaceID, doc.ID)], rev)

	copied := *doc
	return &copied, nil
}

// UpdateDocumentContent 实现 Repository 接口。
func (m *MockRepository) UpdateDocumentContent(ctx context.Context, workspaceID, path, newContent, newHash string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	doc, ok := m.docs[docKey(workspaceID, path)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		return nil, ErrRevisionConflict
	}

	newRevision := doc.RevisionNumber + 1
	doc.ContentMarkdown = newContent
	doc.ContentHash = newHash
	doc.RevisionNumber = newRevision
	doc.UpdatedBy = updatedBy
	doc.UpdatedByUsername = updatedBy

	m.revisionCounter++
	rev := Revision{
		ID:              fmt.Sprintf("rev-%d", m.revisionCounter),
		DocumentID:      doc.ID,
		WorkspaceID:     workspaceID,
		RevisionNumber:  newRevision,
		Path:            doc.Path,
		Title:           doc.Title,
		ContentMarkdown: newContent,
		ContentHash:     newHash,
		Status:          doc.Status,
		CreatedBy:       updatedBy,
		CreatedByUsername: updatedBy,
		CreatedAt:       "2025-01-01T00:00:00+00:00",
	}
	m.revisions[revKey(workspaceID, doc.ID)] = append(m.revisions[revKey(workspaceID, doc.ID)], rev)

	copied := *doc
	return &copied, nil
}

// UpdateDocumentMetadata 实现 Repository 接口。
func (m *MockRepository) UpdateDocumentMetadata(ctx context.Context, workspaceID, path, newTitle, newType string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	doc, ok := m.docs[docKey(workspaceID, path)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		return nil, ErrRevisionConflict
	}

	newRevision := doc.RevisionNumber + 1
	doc.Title = newTitle
	doc.Type = newType
	doc.RevisionNumber = newRevision
	doc.UpdatedBy = updatedBy
	doc.UpdatedByUsername = updatedBy

	m.revisionCounter++
	rev := Revision{
		ID:              fmt.Sprintf("rev-%d", m.revisionCounter),
		DocumentID:      doc.ID,
		WorkspaceID:     workspaceID,
		RevisionNumber:  newRevision,
		Path:            doc.Path,
		Title:           newTitle,
		ContentMarkdown: doc.ContentMarkdown,
		ContentHash:     doc.ContentHash,
		Status:          doc.Status,
		CreatedBy:       updatedBy,
		CreatedByUsername: updatedBy,
		CreatedAt:       "2025-01-01T00:00:00+00:00",
	}
	m.revisions[revKey(workspaceID, doc.ID)] = append(m.revisions[revKey(workspaceID, doc.ID)], rev)

	copied := *doc
	return &copied, nil
}

// MoveDocument 实现 Repository 接口。
func (m *MockRepository) MoveDocument(ctx context.Context, workspaceID, oldPath, newPath string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldK := docKey(workspaceID, oldPath)
	doc, ok := m.docs[oldK]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		return nil, ErrRevisionConflict
	}

	newK := docKey(workspaceID, newPath)
	if _, exists := m.docs[newK]; exists {
		return nil, &pgconn.PgError{Code: "23505", Message: "duplicate key"}
	}

	newRevision := doc.RevisionNumber + 1
	delete(m.docs, oldK)
	doc.Path = newPath
	doc.RevisionNumber = newRevision
	doc.UpdatedBy = updatedBy
	doc.UpdatedByUsername = updatedBy
	m.docs[newK] = doc

	m.revisionCounter++
	rev := Revision{
		ID:              fmt.Sprintf("rev-%d", m.revisionCounter),
		DocumentID:      doc.ID,
		WorkspaceID:     workspaceID,
		RevisionNumber:  newRevision,
		Path:            newPath,
		Title:           doc.Title,
		ContentMarkdown: doc.ContentMarkdown,
		ContentHash:     doc.ContentHash,
		Status:          doc.Status,
		CreatedBy:       updatedBy,
		CreatedByUsername: updatedBy,
		CreatedAt:       "2025-01-01T00:00:00+00:00",
	}
	m.revisions[revKey(workspaceID, doc.ID)] = append(m.revisions[revKey(workspaceID, doc.ID)], rev)

	copied := *doc
	return &copied, nil
}

// ArchiveDocument 实现 Repository 接口。
func (m *MockRepository) ArchiveDocument(ctx context.Context, workspaceID, path string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	return m.mockChangeStatus(ctx, workspaceID, path, StatusArchived, expectedRevision, expectedHash, updatedBy, jobEnq)
}

// RestoreDocument 实现 Repository 接口。
func (m *MockRepository) RestoreDocument(ctx context.Context, workspaceID, path string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	return m.mockChangeStatus(ctx, workspaceID, path, StatusActive, expectedRevision, expectedHash, updatedBy, jobEnq)
}

// mockChangeStatus 模拟状态变更，含幂等行为。
// 引入动机：与 PGRepository.changeDocumentStatus 保持一致——已处于目标状态时不新增 revision。
func (m *MockRepository) mockChangeStatus(ctx context.Context, workspaceID, path, newStatus string, expectedRevision int, expectedHash, updatedBy string, jobEnq TxJobEnqueuer) (*Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	doc, ok := m.docs[docKey(workspaceID, path)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if doc.RevisionNumber != expectedRevision || doc.ContentHash != expectedHash {
		return nil, ErrRevisionConflict
	}

	// 幂等：已处于目标状态，不新增 revision
	if doc.Status == newStatus {
		copied := *doc
		return &copied, nil
	}

	newRevision := doc.RevisionNumber + 1
	doc.Status = newStatus
	doc.RevisionNumber = newRevision
	doc.UpdatedBy = updatedBy
	doc.UpdatedByUsername = updatedBy

	m.revisionCounter++
	rev := Revision{
		ID:              fmt.Sprintf("rev-%d", m.revisionCounter),
		DocumentID:      doc.ID,
		WorkspaceID:     workspaceID,
		RevisionNumber:  newRevision,
		Path:            doc.Path,
		Title:           doc.Title,
		ContentMarkdown: doc.ContentMarkdown,
		ContentHash:     doc.ContentHash,
		Status:          newStatus,
		CreatedBy:       updatedBy,
		CreatedByUsername: updatedBy,
		CreatedAt:       "2025-01-01T00:00:00+00:00",
	}
	m.revisions[revKey(workspaceID, doc.ID)] = append(m.revisions[revKey(workspaceID, doc.ID)], rev)

	copied := *doc
	return &copied, nil
}

// PurgeDocument 实现 Repository 接口。
func (m *MockRepository) PurgeDocument(ctx context.Context, workspaceID, documentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for k, doc := range m.docs {
		if doc.WorkspaceID == workspaceID && doc.ID == documentID {
			delete(m.docs, k)
			delete(m.revisions, revKey(workspaceID, documentID))
			delete(m.sources, srcKey(workspaceID, documentID))
			return nil
		}
	}
	return sql.ErrNoRows
}

// ListRevisions 实现 Repository 接口。
func (m *MockRepository) ListRevisions(ctx context.Context, workspaceID, documentID string, limit, offset int) (*ListRevisionsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	revs := m.revisions[revKey(workspaceID, documentID)]
	total := len(revs)
	if offset >= total {
		return &ListRevisionsResult{Revisions: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	// 按降序返回
	result := make([]Revision, 0, end-offset)
	for i := total - 1 - offset; i >= total-end; i-- {
		result = append(result, revs[i])
	}
	return &ListRevisionsResult{Revisions: result, Total: total}, nil
}

// GetRevision 实现 Repository 接口。
func (m *MockRepository) GetRevision(ctx context.Context, workspaceID, documentID string, revisionNumber int) (*Revision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	revs := m.revisions[revKey(workspaceID, documentID)]
	for i := range revs {
		if revs[i].RevisionNumber == revisionNumber {
			return &revs[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

// AddSource 实现 Repository 接口。
func (m *MockRepository) AddSource(ctx context.Context, workspaceID, documentID, sourceType, value, title, retrievedAt, contentHash string, refreshIntervalDays int, sourceDocumentID, createdBy string) (*Source, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sourceIDCounter++
	srcID := fmt.Sprintf("src-%d", m.sourceIDCounter)
	src := Source{
		ID:                  srcID,
		DocumentID:          documentID,
		WorkspaceID:         workspaceID,
		SourceType:          sourceType,
		Value:               value,
		Title:               title,
		RetrievedAt:         retrievedAt,
		ContentHash:         contentHash,
		RefreshIntervalDays: refreshIntervalDays,
		SourceDocumentID:    sourceDocumentID,
		CreatedBy:           createdBy,
		CreatedByUsername:   createdBy,
		CreatedAt:           "2025-01-01T00:00:00+00:00",
	}
	m.sources[srcKey(workspaceID, documentID)] = append(m.sources[srcKey(workspaceID, documentID)], src)
	copied := src
	return &copied, nil
}

// ListSources 实现 Repository 接口。
func (m *MockRepository) ListSources(ctx context.Context, workspaceID, documentID string, limit, offset int) (*ListSourcesResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	srcs := m.sources[srcKey(workspaceID, documentID)]
	total := len(srcs)
	if offset >= total {
		return &ListSourcesResult{Sources: nil, Total: total}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	result := make([]Source, end-offset)
	copy(result, srcs[offset:end])
	return &ListSourcesResult{Sources: result, Total: total}, nil
}

// DeleteSource 实现 Repository 接口。
func (m *MockRepository) DeleteSource(ctx context.Context, workspaceID, sourceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for wsDocKey, srcs := range m.sources {
		// 只删除同一 workspace 的 source
		parts := splitKey(wsDocKey)
		if parts[0] != workspaceID {
			continue
		}
		for i, src := range srcs {
			if src.ID == sourceID {
				m.sources[wsDocKey] = append(srcs[:i], srcs[i+1:]...)
				return nil
			}
		}
	}
	return sql.ErrNoRows
}

// GetWorkspaceMaxDocumentSize 实现 Repository 接口。
func (m *MockRepository) GetWorkspaceMaxDocumentSize(ctx context.Context, workspaceID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	size, ok := m.workspaceMaxSize[workspaceID]
	if !ok {
		return 2097152, nil // 默认 2MB
	}
	return size, nil
}

// GetWorkspaceRetentionConfig 实现 Repository 接口。
func (m *MockRepository) GetWorkspaceRetentionConfig(ctx context.Context, workspaceID string) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.workspaceRetention[workspaceID]
	if !ok {
		return 7, 30, nil
	}
	return cfg[0], cfg[1], nil
}

// CleanupRevisions 实现 Repository 接口。
func (m *MockRepository) CleanupRevisions(ctx context.Context, workspaceID, documentID string, retentionDays, maxCount int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	revs := m.revisions[revKey(workspaceID, documentID)]
	if len(revs) == 0 {
		return 0, nil
	}
	currentRev := revs[len(revs)-1].RevisionNumber
	cleaned := 0
	var kept []Revision
	for _, rev := range revs {
		if rev.RevisionNumber == currentRev {
			kept = append(kept, rev)
			continue
		}
		if rev.RevisionNumber <= currentRev-maxCount {
			cleaned++
			continue
		}
		kept = append(kept, rev)
	}
	m.revisions[revKey(workspaceID, documentID)] = kept
	return cleaned, nil
}

// CreateSpecialDocuments 实现 Repository 接口。
// mock 中不执行真实特殊文件创建（无真实事务）。
func (m *MockRepository) CreateSpecialDocuments(ctx context.Context, tx *sql.Tx, workspaceID, createdBy string, jobEnq TxJobEnqueuer) error {
	return nil
}

// splitKey 分割 "workspaceID:docID" 格式的键。
func splitKey(k string) [2]string {
	for i := 0; i < len(k); i++ {
		if k[i] == ':' {
			return [2]string{k[:i], k[i+1:]}
		}
	}
	return [2]string{k, ""}
}
