// rebuild_reuse_recycle_test.go 测试"重建索引失控"的三处修复：
//   - A 优先复用已存在且维度正确的索引，不再每次重建都新建 knowledge_vN；
//   - B 本次新建的索引在 alias 切换成功之前失败时被回收，且原始错误原样返回；
//   - C 队列中已存在终态(dead)的 rebuild_index 时不再自动入队新任务；
//   - D 新建索引时把已有版本化索引数量记入日志，使增长可见。
//
// 测试策略：沿用 integrity_test.go 的 fake es.Client，真实调用 handleRebuildIndex /
// HandleJob / enqueueRebuildIfAbsent，断言真实副作用（CreateIndex/DeleteIndex 调用集、
// alias 指向、索引是否仍然存在、入队次数），不做源码字符串包含式断言。
package job

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"partitura/server/internal/es"
	types "partitura/server/internal/search/types"
)

// mustCreateIndex 预置一个索引，失败时终止测试。
func mustCreateIndex(t *testing.T, fakeES *fakeESClient, indexName string, dims int) {
	t.Helper()
	if err := fakeES.CreateIndex(context.Background(), indexName, es.BuildIndexMapping(dims, "standard")); err != nil {
		t.Fatalf("预置索引 %s（dims=%d）失败: %v", indexName, dims, err)
	}
}

// singleDocumentConnector 是"任何查询都返回同一篇文档"的 database/sql Connector。
//
// 引入动机：需要真实驱动 handleRebuildIndex 的逐文档索引步骤（缺陷 B 的失败路径），
// 即 readPGDocuments 必须返回至少一篇文档、reindexDocumentWithEmbedding 也必须能读到该文档。
// emptyResultDB 只能返回零行（等价于"没有可索引文档"），无法触达该分支；
// 该 connector 让 SELECT 恒返回同一行，从而在不依赖 PG 的情况下走完逐文档索引。
type singleDocumentConnector struct {
	doc DocumentForIndex
}

func (c singleDocumentConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return &singleDocumentConn{doc: c.doc}, nil
}

func (c singleDocumentConnector) Driver() driver.Driver { return singleDocumentDriver{} }

// singleDocumentDriver 仅用于满足 driver.Connector.Driver 契约。
type singleDocumentDriver struct{}

func (singleDocumentDriver) Open(name string) (driver.Conn, error) {
	return nil, errors.New("测试用单文档 connector 只能通过 Connect 建立连接")
}

// singleDocumentConn 是返回固定单行结果集的连接。
type singleDocumentConn struct {
	doc DocumentForIndex
}

func (c *singleDocumentConn) Prepare(query string) (driver.Stmt, error) {
	return emptyResultStmt{}, nil
}

func (c *singleDocumentConn) Close() error { return nil }

func (c *singleDocumentConn) Begin() (driver.Tx, error) {
	return singleDocumentTx{}, nil
}

// singleDocumentTx 是测试连接的轻量事务替身。
// 生产 fencing 通过 database/sql Tx 执行 SELECT ... FOR UPDATE；测试只需提供
// 正常 Commit/Rollback 生命周期，不能因为 fake 不支持事务而绕过真正的 ES 失败路径。
type singleDocumentTx struct{}

func (singleDocumentTx) Commit() error   { return nil }
func (singleDocumentTx) Rollback() error { return nil }

func (c *singleDocumentConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT revision_number, content_hash") {
		return &singleDocumentRows{
			columns: []string{"revision_number", "content_hash"},
			values:  []driver.Value{int64(c.doc.RevisionNumber), c.doc.ContentHash},
		}, nil
	}
	return &singleDocumentRows{columns: emptyDocumentColumns, values: singleDocumentValues(c.doc)}, nil
}

func (c *singleDocumentConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

// singleDocumentValues 按 readPGDocuments / reindexDocumentWithEmbedding 的 SELECT 列表顺序
// 构造一行数据（列顺序与 emptyDocumentColumns 一致）。
func singleDocumentValues(doc DocumentForIndex) []driver.Value {
	return []driver.Value{
		doc.ID, doc.WorkspaceID, doc.Path, doc.Title, doc.ContentMarkdown,
		doc.ContentHash, int64(doc.RevisionNumber), doc.Status, doc.IsSpecial,
	}
}

// singleDocumentRows 是恰好一行的结果集：第一次 Next 返回该行，之后返回 io.EOF。
type singleDocumentRows struct {
	columns []string
	values  []driver.Value
	read    bool
}

func (r *singleDocumentRows) Columns() []string { return r.columns }
func (r *singleDocumentRows) Close() error      { return nil }

func (r *singleDocumentRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	copy(dest, r.values)
	r.read = true
	return nil
}

// newSingleDocumentDB 返回"任何查询都返回同一篇文档"的 *sql.DB。
func newSingleDocumentDB(doc DocumentForIndex) *sql.DB {
	return sql.OpenDB(singleDocumentConnector{doc: doc})
}

// rebuildTestDocument 返回一篇用于 rebuild 逐文档索引的普通文档。
func rebuildTestDocument() DocumentForIndex {
	return DocumentForIndex{
		ID:              "doc-1",
		WorkspaceID:     "ws-1",
		Path:            "notes/doc-1.md",
		Title:           "文档 1",
		ContentMarkdown: "# 文档 1\n\n用于驱动 rebuild 逐文档索引的正文。\n",
		ContentHash:     "hash-doc-1",
		RevisionNumber:  1,
		Status:          "current",
	}
}

// newRebuildHandler 构造用于 rebuild 测试的 handler（默认不注入 embedding provider）。
func newRebuildHandler(db *sql.DB, fakeES *fakeESClient, profileRepo ProfileRepo, jobRepo JobEnqueuer) *IndexJobHandler {
	return NewIndexJobHandler(db, fakeES, nil, nil, profileRepo, jobRepo, nil)
}

// newRebuildJob 构造一个指向 indexName 的 rebuild_index job。
func newRebuildJob(indexName string) *Job {
	return &Job{
		Type:    types.JobRebuildIndex,
		Payload: map[string]interface{}{"index_name": indexName},
	}
}

// assertRebuildSucceeded 断言 rebuild 成功，失败时直接终止测试。
func assertRebuildSucceeded(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("rebuild 应成功，实际失败: %v", err)
	}
}

// assertNoCreateIndex 断言本次 rebuild 没有创建任何索引。
func assertNoCreateIndex(t *testing.T, fakeES *fakeESClient) {
	t.Helper()
	if len(fakeES.createIndexCalls) != 0 {
		t.Errorf("不应创建任何索引，实际创建: %v", fakeES.createIndexCalls)
	}
}

// assertNoDeleteIndex 断言没有删除任何索引（复用的索引绝不能被回收）。
func assertNoDeleteIndex(t *testing.T, fakeES *fakeESClient) {
	t.Helper()
	if len(fakeES.deleteIndexCalls) != 0 {
		t.Errorf("不应删除任何索引，实际删除: %v", fakeES.deleteIndexCalls)
	}
}

// assertIndexAlive 断言索引仍然存在于集群中。
func assertIndexAlive(t *testing.T, fakeES *fakeESClient, indexName string) {
	t.Helper()
	if !fakeES.indices[indexName] {
		t.Errorf("索引 %s 应仍然存在，实际索引集: %v", indexName, fakeES.indices)
	}
}

// assertIndexRecycled 断言索引已被回收（不在集群中）。
func assertIndexRecycled(t *testing.T, fakeES *fakeESClient, indexName string) {
	t.Helper()
	if fakeES.indices[indexName] {
		t.Errorf("索引 %s 应已被回收，实际索引集: %v", indexName, fakeES.indices)
	}
}

// TestHandleRebuildIndex_DimensionMismatch_ReusesCorrectDimensionIndex 验证缺陷 A：
// 目标索引维度与 profile 不一致、但集群中已存在维度正确的索引时，直接复用而不新建。
//
// 引入动机：线上事故中每次 rebuild 都用 NextIndexName 产出 knowledge_v{N+1}，索引被堆到
// knowledge_v7 而 alias 始终指向 knowledge_v1。只要已有维度正确的索引（例如上一次重建成功
// 创建、却在 alias 切换前失败的产物），就必须复用它。
func TestHandleRebuildIndex_DimensionMismatch_ReusesCorrectDimensionIndex(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	// 事故现场：alias 指向 1024 维的 knowledge_v1；另有两次失败重建留下的 4096 维索引。
	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	mustCreateIndex(t, fakeES, "knowledge_v2", 4096)
	mustCreateIndex(t, fakeES, "knowledge_v3", 4096)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil
	fakeES.getIndexDimensionsCalls = nil

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := newRebuildHandler(newEmptyResultDB(), fakeES, fakeRepo, nil)

	assertRebuildSucceeded(t, handler.HandleJob(ctx, newRebuildJob("knowledge_v1")))

	// 1) 复用路径绝不创建新索引。
	assertNoCreateIndex(t, fakeES)
	assertNoDeleteIndex(t, fakeES)

	// 2) 候选查询必须使用版本化索引前缀（knowledge_current 之类不会被当作候选）。
	if fakeES.lastListPattern != es.IndexNamePrefix+"*" {
		t.Errorf("复用候选查询 pattern 应为 %q，实际 %q", es.IndexNamePrefix+"*", fakeES.lastListPattern)
	}

	// 3) 多个同维度候选时确定性取版本号最大者（最近一次构建）。
	if fakeES.aliasIndex != "knowledge_v3" {
		t.Errorf("alias 应切到复用的 knowledge_v3，实际 %s", fakeES.aliasIndex)
	}

	// 4) 维度不一致的原目标索引绝不能被选中，且不得被删除。
	assertIndexAlive(t, fakeES, "knowledge_v1")
	if got := fakeES.indexDims["knowledge_v1"]; got != 1024 {
		t.Errorf("原目标索引 knowledge_v1 维度不应被改写，实际 %d", got)
	}

	// 5) 复用索引必须被回写到 profile，保证 profile 记录与 alias 一致。
	if len(fakeRepo.updatedIndexes) != 1 {
		t.Fatalf("期望 1 次 es_index_name 回写，实际 %d 次: %+v", len(fakeRepo.updatedIndexes), fakeRepo.updatedIndexes)
	}
	if got := fakeRepo.updatedIndexes[0]; got.ProfileID != "test-profile-id" || got.IndexName != "knowledge_v3" {
		t.Errorf("回写内容应为 (test-profile-id, knowledge_v3)，实际 %+v", got)
	}
}

// TestHandleRebuildIndex_DimensionMismatch_NoCorrectIndex_CreatesNextVersion 验证缺陷 A 的退回路径：
// 没有任何维度正确的候选时，才沿用"新建下一个版本化索引"的原有行为。
//
// 引入动机：必须确认"复用优先"不会退化成"永远不新建"——候选里只有维度不符的索引时，
// 仍然要用 profile 的 dimensions 创建正确 mapping 的新索引，且索引名不冲突。
func TestHandleRebuildIndex_DimensionMismatch_NoCorrectIndex_CreatesNextVersion(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	mustCreateIndex(t, fakeES, "knowledge_v2", 768)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := newRebuildHandler(newEmptyResultDB(), fakeES, fakeRepo, nil)

	assertRebuildSucceeded(t, handler.HandleJob(ctx, newRebuildJob("knowledge_v1")))

	if len(fakeES.createIndexCalls) != 1 || fakeES.createIndexCalls[0] != "knowledge_v3" {
		t.Fatalf("无可复用索引时应只创建 knowledge_v3，实际创建: %v", fakeES.createIndexCalls)
	}
	if got := mappingEmbeddingDims(fakeES.createIndexMappings["knowledge_v3"]); got != 4096 {
		t.Errorf("knowledge_v3 的 mapping embedding.dims 应为 4096，实际 %d", got)
	}
	if fakeES.aliasIndex != "knowledge_v3" {
		t.Errorf("alias 应指向 knowledge_v3，实际 %s", fakeES.aliasIndex)
	}
	// 旧索引保留以支持 rollback。
	assertIndexAlive(t, fakeES, "knowledge_v1")
	assertIndexAlive(t, fakeES, "knowledge_v2")
}

// TestHandleRebuildIndex_ListCandidatesFailure_FailsFast 验证列出候选索引失败时直接失败。
//
// 引入动机：如果列出候选失败时静默退回"新建"，恰恰会重新引入索引无限增长的问题，
// 因此必须 fail fast 暴露 ES 侧异常。
func TestHandleRebuildIndex_ListCandidatesFailure_FailsFast(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil
	fakeES.listIndicesErr = errors.New("ES 不可用")

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := newRebuildHandler(newEmptyResultDB(), fakeES, fakeRepo, nil)

	err := handler.HandleJob(ctx, newRebuildJob("knowledge_v1"))
	if err == nil {
		t.Fatal("列出候选索引失败时 rebuild 必须返回错误")
	}
	if !containsStr(err.Error(), "ES 不可用") {
		t.Errorf("错误信息应包含底层 ES 错误，实际: %v", err)
	}
	assertNoCreateIndex(t, fakeES)
}

// TestHandleRebuildIndex_DocIndexFailure_RecyclesCreatedIndexAndReturnsOriginalError 验证缺陷 B：
// 本次新建的索引在逐文档索引阶段失败时被回收，且 job 返回的仍是原始错误。
//
// 引入动机：生产事故里 bulk 429 让逐文档索引失败，新索引被遗留在集群且 alias 未切换；
// 每次写入再触发一次重建就又新增一个索引。回收必须发生，同时原始失败原因必须完整保留。
func TestHandleRebuildIndex_DocIndexFailure_RecyclesCreatedIndexAndReturnsOriginalError(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil
	// 逐文档索引的第一步（删除该文档旧 chunk）失败，模拟 ES 限流/不可用。
	fakeES.deleteByQueryErr = errors.New("bulk 429 限流")

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	// 注入 embedding provider，使单文档索引失败立即中止（而非累计到 failedCount）。
	handler := NewIndexJobHandler(newSingleDocumentDB(rebuildTestDocument()), fakeES,
		func(ctx context.Context, texts []string) ([][]float32, error) {
			return [][]float32{make([]float32, 4096)}, nil
		},
		func(ctx context.Context) bool { return true },
		fakeRepo, nil, nil)

	err := handler.HandleJob(ctx, newRebuildJob("knowledge_v1"))
	if err == nil {
		t.Fatal("逐文档索引失败时 rebuild 必须返回错误")
	}
	// 原始错误必须原样保留（不得被回收失败/回收动作替换）。
	if !containsStr(err.Error(), "重建索引时文档 doc-1 索引失败") {
		t.Errorf("错误信息应说明文档索引失败，实际: %v", err)
	}
	if !containsStr(err.Error(), "bulk 429 限流") {
		t.Errorf("错误信息应保留底层 bulk 失败原因，实际: %v", err)
	}

	// 本次新建的 knowledge_v2 必须被回收，旧索引 knowledge_v1 保留。
	if len(fakeES.createIndexCalls) != 1 || fakeES.createIndexCalls[0] != "knowledge_v2" {
		t.Fatalf("本次 rebuild 应只创建 knowledge_v2，实际创建: %v", fakeES.createIndexCalls)
	}
	if len(fakeES.deleteIndexCalls) != 1 || fakeES.deleteIndexCalls[0] != "knowledge_v2" {
		t.Fatalf("应回收本次新建的 knowledge_v2，实际删除: %v", fakeES.deleteIndexCalls)
	}
	assertIndexRecycled(t, fakeES, "knowledge_v2")
	assertIndexAlive(t, fakeES, "knowledge_v1")
	// alias 未切换，仍指向旧索引。
	if fakeES.aliasIndex != "knowledge_v1" {
		t.Errorf("失败时不应切换 alias，实际 %s", fakeES.aliasIndex)
	}
}

// TestHandleRebuildIndex_SwitchAliasFailure_RecyclesCreatedIndex 验证缺陷 B：
// alias 切换失败时回收本次新建的索引（它没有被任何 alias 引用），并返回原始错误。
func TestHandleRebuildIndex_SwitchAliasFailure_RecyclesCreatedIndex(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil
	fakeES.updateAliasErr = errors.New("ES 502 Bad Gateway")

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := newRebuildHandler(newEmptyResultDB(), fakeES, fakeRepo, nil)

	err := handler.HandleJob(ctx, newRebuildJob("knowledge_v1"))
	if err == nil {
		t.Fatal("alias 切换失败时 rebuild 必须返回错误")
	}
	if !containsStr(err.Error(), "alias 切换失败") || !containsStr(err.Error(), "ES 502") {
		t.Errorf("错误信息应保留 alias 切换失败原因，实际: %v", err)
	}

	if len(fakeES.deleteIndexCalls) != 1 || fakeES.deleteIndexCalls[0] != "knowledge_v2" {
		t.Fatalf("应回收本次新建的 knowledge_v2，实际删除: %v", fakeES.deleteIndexCalls)
	}
	assertIndexRecycled(t, fakeES, "knowledge_v2")
	assertIndexAlive(t, fakeES, "knowledge_v1")
	if fakeES.aliasIndex != "knowledge_v1" {
		t.Errorf("alias 应仍指向 knowledge_v1，实际 %s", fakeES.aliasIndex)
	}
}

// TestHandleRebuildIndex_ReusedIndexNotRecycledOnFailure 验证缺陷 B 的边界：
// 失败时只回收本次新建的索引，复用的索引绝不能删除。
//
// 引入动机：复用的索引可能是 alias 当前指向的索引（或同维度的干净索引），
// 一旦被误删会直接造成检索不可用。
func TestHandleRebuildIndex_ReusedIndexNotRecycledOnFailure(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	mustCreateIndex(t, fakeES, "knowledge_v2", 4096)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil
	fakeES.updateAliasErr = errors.New("ES 502 Bad Gateway")

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := newRebuildHandler(newEmptyResultDB(), fakeES, fakeRepo, nil)

	err := handler.HandleJob(ctx, newRebuildJob("knowledge_v1"))
	if err == nil {
		t.Fatal("alias 切换失败时 rebuild 必须返回错误")
	}
	if !containsStr(err.Error(), "alias 切换失败") {
		t.Errorf("错误信息应说明 alias 切换失败，实际: %v", err)
	}

	assertNoCreateIndex(t, fakeES)
	assertNoDeleteIndex(t, fakeES)
	assertIndexAlive(t, fakeES, "knowledge_v2")
	assertIndexAlive(t, fakeES, "knowledge_v1")
}

// TestHandleRebuildIndex_RecycleFailure_KeepsOriginalError 验证缺陷 B：
// 回收失败时必须记录但不掩盖原始错误——job 层需要看到真实失败原因。
func TestHandleRebuildIndex_RecycleFailure_KeepsOriginalError(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil
	fakeES.updateAliasErr = errors.New("ES 502 Bad Gateway")
	fakeES.deleteIndexErr = errors.New("删除索引失败")

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := newRebuildHandler(newEmptyResultDB(), fakeES, fakeRepo, nil)

	err := handler.HandleJob(ctx, newRebuildJob("knowledge_v1"))
	if err == nil {
		t.Fatal("alias 切换失败时 rebuild 必须返回错误")
	}
	// 返回的必须是原始 alias 切换错误，而不是回收失败的错误。
	if !containsStr(err.Error(), "alias 切换失败") || !containsStr(err.Error(), "ES 502") {
		t.Errorf("回收失败不得替换原始错误，实际: %v", err)
	}
	if containsStr(err.Error(), "删除索引失败") {
		t.Errorf("回收失败不应出现在 job 返回的错误中，实际: %v", err)
	}
	if len(fakeES.deleteIndexCalls) != 1 || fakeES.deleteIndexCalls[0] != "knowledge_v2" {
		t.Fatalf("应尝试回收 knowledge_v2，实际删除: %v", fakeES.deleteIndexCalls)
	}
}

// TestEnqueueRebuildIfAbsent_DeadRebuild_SkipsEnqueue 验证缺陷 C：
// 队列中已存在终态(dead)的 rebuild_index 时不再自动入队新任务。
//
// 引入动机：dead 是 Fail 在重试耗尽时产生的终态；若不去重，下一次 index_document 就会再入队
// 一个 rebuild_index，索引名再 +1，形成"每次都失败、每次都自建新索引"的无限增长。
func TestEnqueueRebuildIfAbsent_DeadRebuild_SkipsEnqueue(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	fakeES.aliasIndex = "knowledge_v1"

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	fakeJobRepo := &fakeJobEnqueuer{
		jobs: []Job{{ID: "dead-rebuild", Type: types.JobRebuildIndex, Status: "dead"}},
	}
	handler := newRebuildHandler(nil, fakeES, fakeRepo, fakeJobRepo)

	// index_document 兜底路径发现维度不一致 → 触发去重逻辑。
	err := handler.HandleJob(ctx, &Job{
		Type:    types.JobIndexDocument,
		Payload: map[string]interface{}{"document_id": "doc-1"},
	})
	if err == nil {
		t.Fatal("维度不一致时 index_document 必须返回错误")
	}
	if len(fakeJobRepo.enqueued) != 0 {
		t.Errorf("已存在 dead 的 rebuild_index 时不应入队，实际入队 %+v", fakeJobRepo.enqueued)
	}
}

// TestHandleRebuildIndex_LogsExistingVersionedIndexCount 验证缺陷 D：
// 新建索引时日志中带有"已有版本化索引数量"，使索引数量失控增长可直接观察。
func TestHandleRebuildIndex_LogsExistingVersionedIndexCount(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	mustCreateIndex(t, fakeES, "knowledge_v1", 1024)
	mustCreateIndex(t, fakeES, "knowledge_v2", 768)
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil

	// 捕获日志：slog 默认 logger 是全局的，本包测试串行执行，测试结束后恢复原 logger。
	var logBuffer bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuffer, nil)))
	defer slog.SetDefault(previousLogger)

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := newRebuildHandler(newEmptyResultDB(), fakeES, fakeRepo, nil)

	assertRebuildSucceeded(t, handler.HandleJob(ctx, newRebuildJob("knowledge_v1")))

	// 创建前已有 2 个版本化索引（knowledge_v1、knowledge_v2），该数量必须出现在"已创建新索引"日志里。
	if !strings.Contains(logBuffer.String(), `"existing_versioned_indexes":2`) {
		t.Errorf("新建索引日志应包含 existing_versioned_indexes=2，实际日志: %s", logBuffer.String())
	}
}
