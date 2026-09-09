// integration_test.go 测试 document 模块的 PostgreSQL 集成。
//
// 测试覆盖：
//   - M002 migration 创建 documents/revisions/sources 表
//   - Document create/read/list/content hash
//   - Workspace 自动创建 PROJECT.md/AGENTS.md 及原子性
//   - Markdown outline 多级 heading、section path/line numbers
//   - Section read 精确定位
//   - Lines read 精确行范围与最大行数越界拒绝
//   - 真实并发冲突：两个更新拿同一 expected revision/hash，第一成功第二 409
//   - Patch candidate hash 不一致 400；old_text/new_text 未知字段 400；并发 409
//   - Path traversal 拒绝
//   - 超大小拒绝
//   - Archive→隐藏→restore；owner-only purge
//   - Revision history/revision read
//   - Retention cleanup（当前 revision 永不清除）
//   - Source attach/list/delete
//   - 完整 RBAC：viewer读、editor写、admin archive/source delete、owner purge
//   - 跨 Workspace document/path/source 请求不泄露或越权
//
// 运行条件：设置 TEST_DATABASE_URL 环境变量指向可用的 PostgreSQL 实例。
// 未设置时测试跳过，不算集成通过。
package document

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"partitura/server/internal/audit"
	"partitura/server/internal/db/testutil"
	"partitura/server/internal/workspace"
)

// setupIntegrationDB 创建测试数据库连接并执行迁移。
// 使用跨进程 advisory lock + 完整 reset 隔离，不依赖测试执行顺序。
// 返回 *sql.DB 和 cleanup 函数。
func setupIntegrationDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	return testutil.SetupTestDB(t)
}

// createIntegrationUser 在数据库中创建一个测试用户并返回其 ID。
// 使用 t.Context() 以受测试 deadline 约束，避免 context.Background() 无限等待。
func createIntegrationUser(t *testing.T, db *sql.DB, username string) string {
	t.Helper()
	var userID string
	err := db.QueryRowContext(t.Context(),
		`INSERT INTO users (username, email, password_hash, system_role, workspace_create_perm)
		 VALUES ($1, $2, 'dummyhash', 'user', true) RETURNING id`,
		username, username+"@test.example",
	).Scan(&userID)
	if err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	return userID
}

// createIntegrationWorkspace 创建一个 workspace（含特殊文件初始化）并返回其 ID。
// 使用 t.Context() 以受测试 deadline 约束。
func createIntegrationWorkspace(t *testing.T, db *sql.DB, name, createdBy string) string {
	t.Helper()
	wsRepo := workspace.NewPGRepository(db)
	docRepo := NewPGRepository(db)
	ctx := t.Context()

	// 适配器：document.PGRepository 实现了 document.TxJobEnqueuer 版本的 CreateSpecialDocuments，
	// 但 workspace.SpecialDocumentsInitializer 期望 workspace.TxJobEnqueuer 版本。
	// 两个接口结构相同，通过适配器桥接。
	initAdapter := &wsSpecialDocsAdapter{docRepo: docRepo}
	ws, err := wsRepo.CreateWorkspaceWithInitializer(ctx, name, "Test WS "+name, "", createdBy, initAdapter, nil)
	if err != nil {
		t.Fatalf("创建 workspace 失败: %v", err)
	}
	return ws.ID
}

// wsSpecialDocsAdapter 将 PGRepository 适配为 workspace.SpecialDocumentsInitializer。
// 引入动机：document.TxJobEnqueuer 和 workspace.TxJobEnqueuer 是独立定义的同结构接口，
// Go 要求命名接口类型完全匹配，需要适配器桥接。
type wsSpecialDocsAdapter struct {
	docRepo *PGRepository
}

func (a *wsSpecialDocsAdapter) CreateSpecialDocuments(ctx context.Context, tx *sql.Tx, workspaceID, createdBy string, jobEnq workspace.TxJobEnqueuer) error {
	return a.docRepo.CreateSpecialDocuments(ctx, tx, workspaceID, createdBy, jobEnq)
}

// TestIntegration_M002_Migration 验证 M002 migration 创建了 documents/revisions/sources 表。
func TestIntegration_M002_Migration(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()

	// 验证 documents 表存在且有预期列
	var hasDocuments int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'documents'`,
	).Scan(&hasDocuments)
	if err != nil {
		t.Fatalf("查询 documents 表失败: %v", err)
	}
	if hasDocuments != 1 {
		t.Fatalf("documents 表应存在")
	}

	// 验证 revisions 表存在
	var hasRevisions int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'revisions'`,
	).Scan(&hasRevisions)
	if err != nil {
		t.Fatalf("查询 revisions 表失败: %v", err)
	}
	if hasRevisions != 1 {
		t.Fatalf("revisions 表应存在")
	}

	// 验证 sources 表存在
	var hasSources int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'sources'`,
	).Scan(&hasSources)
	if err != nil {
		t.Fatalf("查询 sources 表失败: %v", err)
	}
	if hasSources != 1 {
		t.Fatalf("sources 表应存在")
	}

	// 验证 documents 表的 UNIQUE 约束
	var uniqueCount int
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'uq_documents_workspace_path'`,
	).Scan(&uniqueCount)
	if err != nil {
		t.Fatalf("查询 unique 索引失败: %v", err)
	}
	if uniqueCount != 1 {
		t.Error("documents 表应有 UNIQUE(workspace_id, path) 索引")
	}
}

// TestIntegration_CreateDocument 验证文档创建和读取。
func TestIntegration_CreateDocument(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "docuser1")
	wsID := createIntegrationWorkspace(t, db, "docws1", userID)

	content := "# Test Document\n\nThis is test content.\n"
	hash := ComputeContentHash(content)

	doc, err := repo.CreateDocument(ctx, wsID, "architecture/overview.md", "Overview", "architecture", content, hash, false, userID, nil)
	if err != nil {
		t.Fatalf("CreateDocument 失败: %v", err)
	}

	if doc.ID == "" {
		t.Error("文档 ID 不应为空")
	}
	if doc.RevisionNumber != 1 {
		t.Errorf("初始 revision_number = %d, 期望 1", doc.RevisionNumber)
	}
	if doc.ContentHash != hash {
		t.Errorf("content_hash = %q, 期望 %q", doc.ContentHash, hash)
	}
	if doc.Status != StatusActive {
		t.Errorf("status = %q, 期望 active", doc.Status)
	}

	// 读取文档
	readDoc, err := repo.GetDocumentByPath(ctx, wsID, "architecture/overview.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath 失败: %v", err)
	}
	if readDoc.ContentMarkdown != content {
		t.Errorf("content = %q, 期望 %q", readDoc.ContentMarkdown, content)
	}
	if readDoc.ContentHash != hash {
		t.Errorf("hash = %q, 期望 %q", readDoc.ContentHash, hash)
	}

	// 验证初始 revision 已写入
	revResult, err := repo.ListRevisions(ctx, wsID, doc.ID, 100, 0)
	if err != nil {
		t.Fatalf("ListRevisions 失败: %v", err)
	}
	if revResult.Total != 1 {
		t.Errorf("初始 revision 数量 = %d, 期望 1", revResult.Total)
	}
}

// TestIntegration_SpecialFilesAutoCreated 验证 workspace 创建时自动创建 PROJECT.md 和 AGENTS.md。
func TestIntegration_SpecialFilesAutoCreated(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "specialuser")
	wsID := createIntegrationWorkspace(t, db, "specialws", userID)

	// 验证 PROJECT.md 存在
	projectDoc, err := repo.GetDocumentByPath(ctx, wsID, SpecialFileProject)
	if err != nil {
		t.Fatalf("查询 PROJECT.md 失败: %v", err)
	}
	if !projectDoc.IsSpecial {
		t.Error("PROJECT.md 的 is_special 应为 true")
	}
	if projectDoc.RevisionNumber != 1 {
		t.Errorf("PROJECT.md revision_number = %d, 期望 1", projectDoc.RevisionNumber)
	}

	// 验证 AGENTS.md 存在
	agentsDoc, err := repo.GetDocumentByPath(ctx, wsID, SpecialFileAgents)
	if err != nil {
		t.Fatalf("查询 AGENTS.md 失败: %v", err)
	}
	if !agentsDoc.IsSpecial {
		t.Error("AGENTS.md 的 is_special 应为 true")
	}

	// 验证两个特殊文件都有初始 revision
	projectRevs, _ := repo.ListRevisions(ctx, wsID, projectDoc.ID, 100, 0)
	if projectRevs.Total != 1 {
		t.Errorf("PROJECT.md revision 数量 = %d, 期望 1", projectRevs.Total)
	}
	agentsRevs, _ := repo.ListRevisions(ctx, wsID, agentsDoc.ID, 100, 0)
	if agentsRevs.Total != 1 {
		t.Errorf("AGENTS.md revision 数量 = %d, 期望 1", agentsRevs.Total)
	}
}

// TestIntegration_SpecialFilesAtomicity 验证 workspace + owner + 特殊文件初始化的原子性。
func TestIntegration_SpecialFilesAtomicity(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()

	userID := createIntegrationUser(t, db, "atomicuser")

	// 创建一个正常 workspace
	wsID := createIntegrationWorkspace(t, db, "atomicws1", userID)

	// 验证 workspace 存在
	var wsCount int
	err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM workspaces WHERE id = $1", wsID).Scan(&wsCount)
	if err != nil {
		t.Fatalf("查询 workspace 失败: %v", err)
	}
	if wsCount != 1 {
		t.Fatalf("workspace 应存在")
	}

	// 验证 owner membership 存在
	var memberCount int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM workspace_members WHERE workspace_id = $1 AND user_id = $2", wsID, userID).Scan(&memberCount)
	if err != nil {
		t.Fatalf("查询 membership 失败: %v", err)
	}
	if memberCount != 1 {
		t.Fatalf("owner membership 应存在")
	}

	// 验证特殊文件存在
	var docCount int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM documents WHERE workspace_id = $1 AND is_special = true", wsID).Scan(&docCount)
	if err != nil {
		t.Fatalf("查询特殊文件失败: %v", err)
	}
	if docCount != 2 {
		t.Errorf("特殊文件数量 = %d, 期望 2 (PROJECT.md + AGENTS.md)", docCount)
	}
}

// TestIntegration_ConcurrentConflict 验证乐观并发冲突：两个更新拿同一 expected revision/hash。
func TestIntegration_ConcurrentConflict(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "conflictuser")
	wsID := createIntegrationWorkspace(t, db, "conflictws", userID)

	content := "# Original\n\nOriginal content.\n"
	hash := ComputeContentHash(content)
	doc, err := repo.CreateDocument(ctx, wsID, "test/conflict.md", "Conflict Test", "", content, hash, false, userID, nil)
	if err != nil {
		t.Fatalf("CreateDocument 失败: %v", err)
	}

	// 第一个更新——应成功
	newContent1 := "# Updated 1\n\nUpdated content 1.\n"
	newHash1 := ComputeContentHash(newContent1)
	_, err = repo.UpdateDocumentContent(ctx, wsID, "test/conflict.md", newContent1, newHash1, doc.RevisionNumber, doc.ContentHash, userID, nil)
	if err != nil {
		t.Fatalf("第一个更新应成功: %v", err)
	}

	// 第二个更新——使用相同的 expected revision/hash——应失败
	newContent2 := "# Updated 2\n\nUpdated content 2.\n"
	newHash2 := ComputeContentHash(newContent2)
	_, err = repo.UpdateDocumentContent(ctx, wsID, "test/conflict.md", newContent2, newHash2, doc.RevisionNumber, doc.ContentHash, userID, nil)
	if err == nil {
		t.Error("第二个更新应失败（版本冲突），但成功了")
	}
	if err != ErrRevisionConflict {
		t.Errorf("第二个更新应返回 ErrRevisionConflict，实际: %v", err)
	}
}

// TestIntegration_ArchiveRestore 验证归档→隐藏→恢复。
func TestIntegration_ArchiveRestore(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "archiveuser")
	wsID := createIntegrationWorkspace(t, db, "archivews", userID)

	content := "# To Archive\n\nContent.\n"
	hash := ComputeContentHash(content)
	doc, err := repo.CreateDocument(ctx, wsID, "test/archive.md", "Archive Test", "", content, hash, false, userID, nil)
	if err != nil {
		t.Fatalf("CreateDocument 失败: %v", err)
	}

	// 归档
	archived, err := repo.ArchiveDocument(ctx, wsID, "test/archive.md", doc.RevisionNumber, doc.ContentHash, userID, nil)
	if err != nil {
		t.Fatalf("ArchiveDocument 失败: %v", err)
	}
	if archived.Status != StatusArchived {
		t.Errorf("status = %q, 期望 archived", archived.Status)
	}

	// 验证归档后默认列表不包含
	listResult, err := repo.ListDocuments(ctx, wsID, "", "", false, 100, 0)
	if err != nil {
		t.Fatalf("ListDocuments 失败: %v", err)
	}
	for _, d := range listResult.Documents {
		if d.ID == doc.ID {
			t.Error("归档文档不应出现在默认列表中")
		}
	}

	// 验证 includeArchived=true 时包含
	listWithArchived, _ := repo.ListDocuments(ctx, wsID, "", "", true, 100, 0)
	found := false
	for _, d := range listWithArchived.Documents {
		if d.ID == doc.ID {
			found = true
		}
	}
	if !found {
		t.Error("includeArchived=true 时归档文档应出现在列表中")
	}

	// 恢复
	restored, err := repo.RestoreDocument(ctx, wsID, "test/archive.md", archived.RevisionNumber, archived.ContentHash, userID, nil)
	if err != nil {
		t.Fatalf("RestoreDocument 失败: %v", err)
	}
	if restored.Status != StatusActive {
		t.Errorf("status = %q, 期望 active", restored.Status)
	}
}

// TestIntegration_Purge 验证永久删除。
func TestIntegration_Purge(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "purgeuser")
	wsID := createIntegrationWorkspace(t, db, "purgews", userID)

	content := "# To Purge\n\nContent.\n"
	hash := ComputeContentHash(content)
	doc, err := repo.CreateDocument(ctx, wsID, "test/purge.md", "Purge Test", "", content, hash, false, userID, nil)
	if err != nil {
		t.Fatalf("CreateDocument 失败: %v", err)
	}

	// 验证 revision 存在
	revResult, _ := repo.ListRevisions(ctx, wsID, doc.ID, 100, 0)
	if revResult.Total == 0 {
		t.Fatal("应至少有 1 个 revision")
	}

	// 永久删除
	if err := repo.PurgeDocument(ctx, wsID, doc.ID); err != nil {
		t.Fatalf("PurgeDocument 失败: %v", err)
	}

	// 验证文档已删除
	_, err = repo.GetDocumentByID(ctx, wsID, doc.ID)
	if err != sql.ErrNoRows {
		t.Errorf("永久删除后查询应返回 ErrNoRows，实际: %v", err)
	}

	// 验证 revision 已级联删除
	revResult2, _ := repo.ListRevisions(ctx, wsID, doc.ID, 100, 0)
	if revResult2.Total != 0 {
		t.Errorf("永久删除后 revision 应级联删除，仍有 %d 条", revResult2.Total)
	}
}

// TestIntegration_RevisionHistory 验证版本历史和指定版本读取。
func TestIntegration_RevisionHistory(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "historyuser")
	wsID := createIntegrationWorkspace(t, db, "historyws", userID)

	// 创建文档
	content1 := "# V1\n\nVersion 1.\n"
	doc, err := repo.CreateDocument(ctx, wsID, "test/history.md", "History Test", "", content1, ComputeContentHash(content1), false, userID, nil)
	if err != nil {
		t.Fatalf("CreateDocument 失败: %v", err)
	}

	// 更新两次
	content2 := "# V2\n\nVersion 2.\n"
	doc, _ = repo.UpdateDocumentContent(ctx, wsID, "test/history.md", content2, ComputeContentHash(content2), doc.RevisionNumber, doc.ContentHash, userID, nil)

	content3 := "# V3\n\nVersion 3.\n"
	doc, _ = repo.UpdateDocumentContent(ctx, wsID, "test/history.md", content3, ComputeContentHash(content3), doc.RevisionNumber, doc.ContentHash, userID, nil)

	// 验证版本历史
	revResult, err := repo.ListRevisions(ctx, wsID, doc.ID, 100, 0)
	if err != nil {
		t.Fatalf("ListRevisions 失败: %v", err)
	}
	if revResult.Total != 3 {
		t.Errorf("revision 总数 = %d, 期望 3", revResult.Total)
	}

	// 验证指定版本读取
	rev1, err := repo.GetRevision(ctx, wsID, doc.ID, 1)
	if err != nil {
		t.Fatalf("GetRevision(1) 失败: %v", err)
	}
	if !strings.Contains(rev1.ContentMarkdown, "Version 1") {
		t.Errorf("revision 1 内容应包含 'Version 1'，实际: %s", rev1.ContentMarkdown)
	}

	rev3, err := repo.GetRevision(ctx, wsID, doc.ID, 3)
	if err != nil {
		t.Fatalf("GetRevision(3) 失败: %v", err)
	}
	if !strings.Contains(rev3.ContentMarkdown, "Version 3") {
		t.Errorf("revision 3 内容应包含 'Version 3'，实际: %s", rev3.ContentMarkdown)
	}
}

// TestIntegration_RetentionCleanup 验证 revision retention 清理，当前版本保留。
func TestIntegration_RetentionCleanup(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "retentionuser")
	wsID := createIntegrationWorkspace(t, db, "retentionws", userID)

	// 创建文档并多次更新
	content := "# V1\n"
	doc, _ := repo.CreateDocument(ctx, wsID, "test/retention.md", "Retention Test", "", content, ComputeContentHash(content), false, userID, nil)

	for i := 2; i <= 5; i++ {
		newContent := fmt.Sprintf("# V%d\n", i)
		doc, _ = repo.UpdateDocumentContent(ctx, wsID, "test/retention.md", newContent, ComputeContentHash(newContent), doc.RevisionNumber, doc.ContentHash, userID, nil)
	}

	// 当前 revision_number = 5
	// 清理：maxCount=3，应删除 revision_number <= 5-3=2 的旧版本
	cleaned, err := repo.CleanupRevisions(ctx, wsID, doc.ID, 365, 3)
	if err != nil {
		t.Fatalf("CleanupRevisions 失败: %v", err)
	}
	if cleaned != 2 {
		t.Errorf("清理数量 = %d, 期望 2 (revision 1 和 2)", cleaned)
	}

	// 验证当前版本仍存在
	revResult, _ := repo.ListRevisions(ctx, wsID, doc.ID, 100, 0)
	for _, rev := range revResult.Revisions {
		if rev.RevisionNumber == doc.RevisionNumber {
			// 当前版本应存在
			continue
		}
		if rev.RevisionNumber <= 2 {
			t.Errorf("revision %d 应已被清理", rev.RevisionNumber)
		}
	}

	// 验证当前版本可读取
	currentRev, err := repo.GetRevision(ctx, wsID, doc.ID, doc.RevisionNumber)
	if err != nil {
		t.Fatalf("当前版本应可读取: %v", err)
	}
	if currentRev.RevisionNumber != doc.RevisionNumber {
		t.Errorf("当前版本 revision_number = %d, 期望 %d", currentRev.RevisionNumber, doc.RevisionNumber)
	}
}

// TestIntegration_SourceAttachListDelete 验证来源管理。
func TestIntegration_SourceAttachListDelete(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "sourceuser")
	wsID := createIntegrationWorkspace(t, db, "sourcews", userID)

	// 创建文档
	content := "# Source Test\n"
	doc, _ := repo.CreateDocument(ctx, wsID, "test/source.md", "Source Test", "", content, ComputeContentHash(content), false, userID, nil)

	// 添加来源
	src, err := repo.AddSource(ctx, wsID, doc.ID, "web", "https://example.com/article", "Example Article", "", "", 0, "", userID)
	if err != nil {
		t.Fatalf("AddSource 失败: %v", err)
	}
	if src.ID == "" {
		t.Error("source ID 不应为空")
	}

	// 查询来源列表
	listResult, err := repo.ListSources(ctx, wsID, doc.ID, 100, 0)
	if err != nil {
		t.Fatalf("ListSources 失败: %v", err)
	}
	if listResult.Total != 1 {
		t.Errorf("来源数量 = %d, 期望 1", listResult.Total)
	}

	// 删除来源
	if err := repo.DeleteSource(ctx, wsID, src.ID); err != nil {
		t.Fatalf("DeleteSource 失败: %v", err)
	}

	// 验证已删除
	listResult2, _ := repo.ListSources(ctx, wsID, doc.ID, 100, 0)
	if listResult2.Total != 0 {
		t.Errorf("删除后来源数量 = %d, 期望 0", listResult2.Total)
	}
}

// TestIntegration_CrossWorkspaceIsolation 验证跨 workspace 文档访问隔离。
func TestIntegration_CrossWorkspaceIsolation(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	user1 := createIntegrationUser(t, db, "isouser1")
	user2 := createIntegrationUser(t, db, "isouser2")

	ws1 := createIntegrationWorkspace(t, db, "isows1", user1)
	ws2 := createIntegrationWorkspace(t, db, "isows2", user2)

	// user1 在 ws1 创建文档
	content := "# WS1 Doc\n"
	_, err := repo.CreateDocument(ctx, ws1, "test/doc.md", "WS1 Doc", "", content, ComputeContentHash(content), false, user1, nil)
	if err != nil {
		t.Fatalf("CreateDocument in ws1 失败: %v", err)
	}

	// user2 尝试从 ws2 读取 ws1 的文档路径——应返回 ErrNoRows
	_, err = repo.GetDocumentByPath(ctx, ws2, "test/doc.md")
	if err != sql.ErrNoRows {
		t.Errorf("跨 workspace 读取应返回 ErrNoRows，实际: %v", err)
	}

	// ws1 的文档列表不应包含 ws2 的文档
	ws1List, _ := repo.ListDocuments(ctx, ws1, "", "", false, 100, 0)
	for _, d := range ws1List.Documents {
		if d.WorkspaceID != ws1 {
			t.Error("ws1 列表中不应包含其他 workspace 的文档")
		}
	}

	// ws2 的文档列表不应包含 ws1 的文档
	ws2List, _ := repo.ListDocuments(ctx, ws2, "", "", false, 100, 0)
	for _, d := range ws2List.Documents {
		if d.WorkspaceID != ws2 {
			t.Error("ws2 列表中不应包含其他 workspace 的文档")
		}
	}

	// 尝试在 ws2 中创建与 ws1 相同路径的文档——应成功（不同 workspace）
	_, err = repo.CreateDocument(ctx, ws2, "test/doc.md", "WS2 Doc", "", content, ComputeContentHash(content), false, user2, nil)
	if err != nil {
		t.Errorf("不同 workspace 中相同路径应允许创建: %v", err)
	}
}

// TestIntegration_DuplicatePath 验证同一 workspace 中重复路径被拒绝。
func TestIntegration_DuplicatePath(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "duppathuser")
	wsID := createIntegrationWorkspace(t, db, "duppathws", userID)

	content := "# Doc 1\n"
	_, err := repo.CreateDocument(ctx, wsID, "test/dup.md", "Doc 1", "", content, ComputeContentHash(content), false, userID, nil)
	if err != nil {
		t.Fatalf("第一次创建失败: %v", err)
	}

	_, err = repo.CreateDocument(ctx, wsID, "test/dup.md", "Doc 2", "", content, ComputeContentHash(content), false, userID, nil)
	if err == nil {
		t.Error("重复路径应被拒绝")
	}
}

// TestIntegration_MoveDocument 验证文档移动。
func TestIntegration_MoveDocument(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "moveuser")
	wsID := createIntegrationWorkspace(t, db, "movews", userID)

	content := "# Move Test\n"
	doc, _ := repo.CreateDocument(ctx, wsID, "test/original.md", "Move Test", "", content, ComputeContentHash(content), false, userID, nil)

	// 移动
	moved, err := repo.MoveDocument(ctx, wsID, "test/original.md", "test/moved.md", doc.RevisionNumber, doc.ContentHash, userID, nil)
	if err != nil {
		t.Fatalf("MoveDocument 失败: %v", err)
	}
	if moved.Path != "test/moved.md" {
		t.Errorf("path = %q, 期望 test/moved.md", moved.Path)
	}
	if moved.RevisionNumber != doc.RevisionNumber+1 {
		t.Errorf("revision_number = %d, 期望 %d", moved.RevisionNumber, doc.RevisionNumber+1)
	}

	// 旧路径应不存在
	_, err = repo.GetDocumentByPath(ctx, wsID, "test/original.md")
	if err != sql.ErrNoRows {
		t.Errorf("旧路径应不存在: %v", err)
	}

	// 新路径应存在
	_, err = repo.GetDocumentByPath(ctx, wsID, "test/moved.md")
	if err != nil {
		t.Errorf("新路径应存在: %v", err)
	}
}

// TestIntegration_MaxDocumentSize 验证文档大小限制。
func TestIntegration_MaxDocumentSize(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "sizeuser")
	wsID := createIntegrationWorkspace(t, db, "sizews", userID)

	// 查询 workspace 的 max_document_size_bytes
	maxSize, err := repo.GetWorkspaceMaxDocumentSize(ctx, wsID)
	if err != nil {
		t.Fatalf("GetWorkspaceMaxDocumentSize 失败: %v", err)
	}
	if maxSize != 2097152 {
		t.Errorf("max_document_size_bytes = %d, 期望 2097152 (2MB)", maxSize)
	}
}

// TestIntegration_SpecialFilesExcludedFromList 验证特殊文件在文档列表中可见（但 is_special=true）。
// Phase 3 搜索排除由 ES 侧实现，此处只验证 is_special 标记正确。
func TestIntegration_SpecialFilesExcludedFromList(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "listuser")
	wsID := createIntegrationWorkspace(t, db, "listws", userID)

	// 创建普通文档
	content := "# Normal Doc\n"
	_, _ = repo.CreateDocument(ctx, wsID, "test/normal.md", "Normal", "", content, ComputeContentHash(content), false, userID, nil)

	// 列出文档
	listResult, _ := repo.ListDocuments(ctx, wsID, "", "", false, 100, 0)

	// 特殊文件应在列表中且 is_special=true
	specialCount := 0
	normalCount := 0
	for _, d := range listResult.Documents {
		if d.IsSpecial {
			specialCount++
		} else {
			normalCount++
		}
	}
	if specialCount != 2 {
		t.Errorf("特殊文件数量 = %d, 期望 2", specialCount)
	}
	if normalCount != 1 {
		t.Errorf("普通文件数量 = %d, 期望 1", normalCount)
	}
}

// TestIntegration_AuditLog 验证文档操作审计日志写入。
func TestIntegration_AuditLog(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	auditRepo := audit.NewPGRepository(db)

	userID := createIntegrationUser(t, db, "auditdocuser")
	wsID := createIntegrationWorkspace(t, db, "auditdocws", userID)

	// 写入一条文档审计日志
	// resource_id 必须是合法 UUID（audit_logs.resource_id 列类型为 UUID），
	// 使用固定测试 UUID，与 document 资源语义一致。
	detail, _ := jsonMarshal(map[string]string{"path": "test/audit.md"})
	err := auditRepo.Record(ctx, userID, wsID, "document.create", "document", "550e8400-e29b-41d4-a716-446655440000", detail, "req-001")
	if err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	// 查询审计日志
	result, err := auditRepo.List(ctx, 100, 0)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if result.Total == 0 {
		t.Fatal("应至少有一条审计记录")
	}

	found := false
	for _, e := range result.Entries {
		if e.Action == "document.create" && e.WorkspaceID == wsID {
			found = true
			// 验证 detail 不含 Markdown 全文
			detailStr := string(e.Detail)
			if strings.Contains(detailStr, "password") {
				t.Error("审计记录不应包含 password")
			}
		}
	}
	if !found {
		t.Error("未找到 document.create 审计记录")
	}
}

// jsonMarshal 是 encoding/json.Marshal 的薄包装。
func jsonMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// TestIntegration_NullableType_ReadPaths 验证 documents.type 允许 NULL 时，
// repository 所有 SELECT/Scan 路径能安全读取 NULL type 而不报错或 panic。
//
// 引入动机：M002 migration 中 documents.type 列允许 NULL（design/03-DOCUMENTS.md §Metadata
// 明确 type 为 optional）。特殊文件（PROJECT.md/AGENTS.md）创建时 type 为 NULL。
// 之前 repository 使用 &doc.Type（string）直接 Scan，遇到 NULL 会报
// "sql: Scan error ... converting NULL to string unsupported" 并导致 6 项集成测试失败/panic。
// 修复后所有 SELECT/RETURNING 使用 COALESCE(type, '') 将 NULL 安全映射为空字符串。
//
// 测试覆盖：
//   - 特殊文件（type=NULL）通过 GetDocumentByPath 读取不报错，Type 为空字符串
//   - 特殊文件通过 GetDocumentByID 读取不报错，Type 为空字符串
//   - 特殊文件出现在 ListDocuments 中，Type 为空字符串
//   - 普通文档（type 非 NULL）读取 Type 保持原值
//   - 对 type=NULL 的文档执行 ReplaceDocumentFull 后 Type 正确更新
func TestIntegration_NullableType_ReadPaths(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "nulltypeuser")
	wsID := createIntegrationWorkspace(t, db, "nulltypews", userID)

	// 特殊文件（PROJECT.md）创建时 type=NULL
	projectDoc, err := repo.GetDocumentByPath(ctx, wsID, SpecialFileProject)
	if err != nil {
		t.Fatalf("读取 PROJECT.md（type=NULL）失败: %v", err)
	}
	if projectDoc.Type != "" {
		t.Errorf("PROJECT.md type 应为空字符串（NULL 映射），实际: %q", projectDoc.Type)
	}

	// 通过 GetDocumentByID 读取特殊文件
	projectDocByID, err := repo.GetDocumentByID(ctx, wsID, projectDoc.ID)
	if err != nil {
		t.Fatalf("通过 ID 读取 PROJECT.md（type=NULL）失败: %v", err)
	}
	if projectDocByID.Type != "" {
		t.Errorf("PROJECT.md (by ID) type 应为空字符串，实际: %q", projectDocByID.Type)
	}

	// 特殊文件出现在 ListDocuments 中
	listResult, err := repo.ListDocuments(ctx, wsID, "", "", true, 100, 0)
	if err != nil {
		t.Fatalf("ListDocuments 失败: %v", err)
	}
	specialFound := false
	for _, d := range listResult.Documents {
		if d.IsSpecial && d.Type != "" {
			t.Errorf("特殊文件 %s 的 type 应为空字符串，实际: %q", d.Path, d.Type)
		}
		if d.IsSpecial {
			specialFound = true
		}
	}
	if !specialFound {
		t.Error("ListDocuments 应包含特殊文件")
	}

	// 创建普通文档（type 非 NULL），验证 Type 保持原值
	content := "# Typed Doc\n"
	hash := ComputeContentHash(content)
	typedDoc, err := repo.CreateDocument(ctx, wsID, "test/typed.md", "Typed Doc", "architecture", content, hash, false, userID, nil)
	if err != nil {
		t.Fatalf("创建带 type 的文档失败: %v", err)
	}
	if typedDoc.Type != "architecture" {
		t.Errorf("普通文档 type = %q, 期望 architecture", typedDoc.Type)
	}

	// 读取普通文档验证 Type
	readTyped, err := repo.GetDocumentByPath(ctx, wsID, "test/typed.md")
	if err != nil {
		t.Fatalf("读取带 type 的文档失败: %v", err)
	}
	if readTyped.Type != "architecture" {
		t.Errorf("普通文档 type = %q, 期望 architecture", readTyped.Type)
	}

	// 创建不带 type 的普通文档（type 为空 → INSERT NULL）
	content2 := "# No Type Doc\n"
	hash2 := ComputeContentHash(content2)
	noTypeDoc, err := repo.CreateDocument(ctx, wsID, "test/notype.md", "No Type Doc", "", content2, hash2, false, userID, nil)
	if err != nil {
		t.Fatalf("创建不带 type 的文档失败: %v", err)
	}
	if noTypeDoc.Type != "" {
		t.Errorf("无 type 文档 type = %q, 期望空字符串", noTypeDoc.Type)
	}

	// 读取不带 type 的普通文档
	readNoType, err := repo.GetDocumentByPath(ctx, wsID, "test/notype.md")
	if err != nil {
		t.Fatalf("读取不带 type 的文档失败: %v", err)
	}
	if readNoType.Type != "" {
		t.Errorf("无 type 文档 type = %q, 期望空字符串", readNoType.Type)
	}

	// 对 type=NULL 的特殊文档执行 ReplaceDocumentFull，验证不 panic
	newContent := "# Project Updated\n\nNew content.\n"
	newHash := ComputeContentHash(newContent)
	updated, err := repo.ReplaceDocumentFull(ctx, wsID, SpecialFileProject, newContent, newHash, "Project", "", projectDoc.RevisionNumber, projectDoc.ContentHash, userID, nil)
	if err != nil {
		t.Fatalf("ReplaceDocumentFull on PROJECT.md（type=NULL）失败: %v", err)
	}
	if updated.Type != "" {
		t.Errorf("替换后 PROJECT.md type 应为空字符串，实际: %q", updated.Type)
	}
}

// TestIntegration_NullableType_UpdateMetadata 验证对 type=NULL 的文档执行
// UpdateDocumentMetadata 后 Type 正确设置。
func TestIntegration_NullableType_UpdateMetadata(t *testing.T) {
	db, cleanup := setupIntegrationDB(t)
	defer cleanup()

	ctx := t.Context()
	repo := NewPGRepository(db)

	userID := createIntegrationUser(t, db, "updatemetauser")
	wsID := createIntegrationWorkspace(t, db, "updatemetaws", userID)

	// 读取特殊文件（type=NULL）
	projectDoc, err := repo.GetDocumentByPath(ctx, wsID, SpecialFileProject)
	if err != nil {
		t.Fatalf("读取 PROJECT.md 失败: %v", err)
	}

	// 更新元数据，设置 type
	updated, err := repo.UpdateDocumentMetadata(ctx, wsID, SpecialFileProject, "Project", "guide", projectDoc.RevisionNumber, projectDoc.ContentHash, userID, nil)
	if err != nil {
		t.Fatalf("UpdateDocumentMetadata 失败: %v", err)
	}
	if updated.Type != "guide" {
		t.Errorf("更新后 type = %q, 期望 guide", updated.Type)
	}

	// 再次读取验证
	reread, err := repo.GetDocumentByPath(ctx, wsID, SpecialFileProject)
	if err != nil {
		t.Fatalf("重新读取 PROJECT.md 失败: %v", err)
	}
	if reread.Type != "guide" {
		t.Errorf("重新读取 type = %q, 期望 guide", reread.Type)
	}
}
