// index_dimension_selfheal_test.go 测试"embedding 维度不一致自动重建"的自愈逻辑。
//
// 引入动机：ES 的 dense_vector.dims 建好后不可变。当 search profile 的 embedding 维度与既有索引
// mapping 维度不一致时，复用旧索引会让写入必然被 ES 以 400 拒绝。本文件覆盖两条路径：
//   - rebuild_index：发现目标索引维度与 profile 不一致 → 自动创建正确维度的新索引 →
//     全量重建 → 切换 alias → 把新索引名回写 profile。
//   - index_document 兜底：发现 alias 指向索引维度与 profile 不一致 → fail fast 并投递一次
//     rebuild_index（已存在 pending/running 的 rebuild 时不重复投递）。
//
// 测试策略：使用 fake es.Client + fake ProfileRepo + fake JobEnqueuer 断言真实副作用
// （CreateIndex 的 mapping、alias 指向、回写调用、入队调用），不使用源码字符串包含式断言。
// rebuild 主流程在确定目标索引后需要读取 PG 文档，这里通过 emptyResultConnector 提供一个
// 零行结果集的 database/sql 连接，从而在不依赖 TEST_DATABASE_URL/PG 的前提下完整驱动流程。
package job

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"partitura/server/internal/es"
	types "partitura/server/internal/search/types"
)

// emptyResultConnector 是只返回零行结果集的 database/sql Connector。
//
// 引入动机：rebuild 主流程在创建/复用目标索引后需要 readPGDocuments + CheckIntegrity 读 PG。
// 本仓库的 PG 集成测试依赖 TEST_DATABASE_URL，未设置时会 skip，导致维度自愈无法被完整驱动。
// 该 connector 让查询"成功但零行"，等价于"PG 中没有可索引文档"，使 rebuild 能走到
// alias 切换与 profile 回写，无需外部数据库，也不引入新依赖。
type emptyResultConnector struct{}

func (emptyResultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return emptyResultConn{}, nil
}

func (emptyResultConnector) Driver() driver.Driver { return emptyResultDriver{} }

// emptyResultDriver 仅用于满足 driver.Connector.Driver 契约。
type emptyResultDriver struct{}

func (emptyResultDriver) Open(name string) (driver.Conn, error) { return emptyResultConn{}, nil }

// emptyResultConn 是不执行任何真实 SQL 的连接。
type emptyResultConn struct{}

func (emptyResultConn) Prepare(query string) (driver.Stmt, error) { return emptyResultStmt{}, nil }
func (emptyResultConn) Close() error                              { return nil }
func (emptyResultConn) Begin() (driver.Tx, error) {
	return nil, errors.New("测试用空结果 connector 不支持事务")
}

func (emptyResultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return emptyResultRows{}, nil
}

func (emptyResultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}

// emptyResultStmt 供不实现 QueryerContext 的调用路径使用。
type emptyResultStmt struct{}

func (emptyResultStmt) Close() error  { return nil }
func (emptyResultStmt) NumInput() int { return -1 }
func (emptyResultStmt) Exec(args []driver.Value) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}
func (emptyResultStmt) Query(args []driver.Value) (driver.Rows, error) { return emptyResultRows{}, nil }

// emptyResultRows 是零行结果集：Next 立即返回 io.EOF。
type emptyResultRows struct{}

// emptyDocumentColumns 与 readPGDocuments 的 SELECT 列表长度一致。
var emptyDocumentColumns = []string{
	"id", "workspace_id", "path", "title", "content_markdown",
	"content_hash", "revision_number", "status", "is_special",
}

func (emptyResultRows) Columns() []string              { return emptyDocumentColumns }
func (emptyResultRows) Close() error                   { return nil }
func (emptyResultRows) Next(dest []driver.Value) error { return io.EOF }

// newEmptyResultDB 返回一个查询成功但零行的 *sql.DB，用于在不依赖 PG 的情况下驱动 rebuild 主流程。
func newEmptyResultDB() *sql.DB {
	return sql.OpenDB(emptyResultConnector{})
}

// fakeJobEnqueuer 是可配置的 JobEnqueuer fake。
//
// 引入动机：index_document 兜底路径必须在维度不一致时恰好投递一次 rebuild_index，
// 并在已有 pending/running 的 rebuild_index 时跳过；测试需要断言 job 类型、payload 与调用次数。
type fakeJobEnqueuer struct {
	// jobs 是 fake 队列中的任务（用于 List 查询）。
	jobs []Job
	// enqueued 按顺序记录入队请求。
	enqueued []enqueuedJob
	// listErr 非 nil 时 List 返回该错误。
	listErr error
	// enqueueErr 非 nil 时 Enqueue 返回该错误。
	enqueueErr error
}

// enqueuedJob 记录一次 Enqueue 调用的参数。
type enqueuedJob struct {
	JobType string
	Payload map[string]interface{}
	ID      string
}

func (f *fakeJobEnqueuer) Enqueue(ctx context.Context, jobType string, payload map[string]interface{}) (string, error) {
	if f.enqueueErr != nil {
		return "", f.enqueueErr
	}
	id := "job-" + jobType + "-" + itoaForTest(len(f.jobs)+1)
	f.enqueued = append(f.enqueued, enqueuedJob{JobType: jobType, Payload: payload, ID: id})
	f.jobs = append(f.jobs, Job{ID: id, Type: jobType, Payload: payload, Status: "pending"})
	return id, nil
}

func (f *fakeJobEnqueuer) List(ctx context.Context, statusFilter string, limit, offset int) (*ListResult, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	result := &ListResult{}
	for _, j := range f.jobs {
		if j.Status == statusFilter {
			result.Jobs = append(result.Jobs, j)
		}
	}
	result.Total = len(result.Jobs)
	return result, nil
}

// itoaForTest 是避免引入 strconv 的最小整数转字符串实现（仅用于生成 fake job ID）。
func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// newDimensionSelfHealProfile 构造一个 embedding 维度为 dimensions 的 active profile。
func newDimensionSelfHealProfile(dimensions int, esIndexName string) *fakeProfileRepo {
	return &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:                  "test-profile-id",
			ESIndexName:         esIndexName,
			ChunkTargetSize:     512,
			ChunkOverlap:        64,
			EmbeddingDimensions: dimensions,
			Analyzer:            "standard",
		},
	}
}

// TestHandleRebuildIndex_DimensionMismatch_RebuildsIntoNewIndexAndWritesBackProfile
// 验证 rebuild 遇到"索引维度 1024 / profile 维度 4096"时自动创建正确维度的新索引、
// 切换 alias 并把新索引名回写到 profile。
//
// 引入动机：这就是"修改 profile 维度并激活 → 自动重建"的落地实现，必须端到端断言，
// 否则维度不一致只会表现为写入 4096 维向量时 ES 返回 400，系统无法自愈。
func TestHandleRebuildIndex_DimensionMismatch_RebuildsIntoNewIndexAndWritesBackProfile(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	// 事故现场：目标索引已存在，但 mapping 维度是旧的 1024，且里面已有数据。
	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(1024, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	if err := fakeES.BulkIndex(ctx, "knowledge_v1", []es.IndexDoc{
		{ID: "old_chunk_0", Body: map[string]interface{}{"document_id": "old_doc"}},
	}); err != nil {
		t.Fatalf("预置旧索引文档失败: %v", err)
	}
	// 只关注 rebuild 自身的动作，清空预置阶段的调用记录。
	fakeES.createIndexCalls = nil
	fakeES.getIndexDimensionsCalls = nil
	fakeES.aliasIndex = "knowledge_v1"

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := NewIndexJobHandler(newEmptyResultDB(), fakeES, nil, nil, fakeRepo, nil, nil)

	job := &Job{
		Type: types.JobRebuildIndex,
		Payload: map[string]interface{}{
			"index_name": "knowledge_v1",
		},
	}

	if err := handler.HandleJob(ctx, job); err != nil {
		t.Fatalf("维度不一致时 rebuild 应自愈成功，实际失败: %v", err)
	}

	// 1) 必须先读取真实索引维度，才能发现不一致。
	if len(fakeES.getIndexDimensionsCalls) == 0 || fakeES.getIndexDimensionsCalls[0] != "knowledge_v1" {
		t.Fatalf("rebuild 应先读取目标索引 knowledge_v1 的维度，实际调用: %v", fakeES.getIndexDimensionsCalls)
	}

	// 2) 必须新建 knowledge_v2，且 mapping 使用 profile 的 4096 维。
	if !fakeES.indices["knowledge_v2"] {
		t.Fatalf("维度不一致时应创建新索引 knowledge_v2，实际索引集: %v", fakeES.indices)
	}
	if got := mappingEmbeddingDims(fakeES.createIndexMappings["knowledge_v2"]); got != 4096 {
		t.Errorf("knowledge_v2 的 mapping embedding.dims 应为 4096，实际 %d", got)
	}
	if got, err := fakeES.GetIndexDimensions(ctx, "knowledge_v2"); err != nil || got != 4096 {
		t.Errorf("GetIndexDimensions(knowledge_v2) 应返回 (4096, nil)，实际 (%d, %v)", got, err)
	}

	// 3) 本次 rebuild 只应创建 knowledge_v2（不得重建/覆盖旧维度的 knowledge_v1）。
	if len(fakeES.createIndexCalls) != 1 || fakeES.createIndexCalls[0] != "knowledge_v2" {
		t.Errorf("本次 rebuild 只应创建 knowledge_v2，实际创建: %v", fakeES.createIndexCalls)
	}

	// 4) alias 必须切到新索引。
	if fakeES.aliasIndex != "knowledge_v2" {
		t.Errorf("alias 应指向 knowledge_v2，实际 %s", fakeES.aliasIndex)
	}

	// 5) 新索引名必须回写到 profile（payload 未带 profile_id，应回退 active profile.ID）。
	if len(fakeRepo.updatedIndexes) != 1 {
		t.Fatalf("期望 1 次 es_index_name 回写，实际 %d 次: %+v", len(fakeRepo.updatedIndexes), fakeRepo.updatedIndexes)
	}
	if got := fakeRepo.updatedIndexes[0]; got.ProfileID != "test-profile-id" || got.IndexName != "knowledge_v2" {
		t.Errorf("回写内容应为 (test-profile-id, knowledge_v2)，实际 %+v", got)
	}

	// 6) 旧索引必须保留以支持 rollback，且内容不被清空。
	if !fakeES.indices["knowledge_v1"] {
		t.Error("旧索引 knowledge_v1 应保留以支持 rollback")
	}
	if doc := fakeES.docs["knowledge_v1"]["old_chunk_0"]; doc == nil {
		t.Error("旧索引 knowledge_v1 中的文档应保持不变")
	}
}

// TestHandleRebuildIndex_DimensionConsistent_ReusesExistingIndex 验证维度一致时复用现有索引。
//
// 引入动机：维度一致时必须跳过创建，避免覆盖已有数据 / 触发无谓的全量重建；
// 此处 payload 不带 index_name，同时覆盖"回退 active profile.ESIndexName"的取索引名逻辑。
func TestHandleRebuildIndex_DimensionConsistent_ReusesExistingIndex(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(4096, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	// 记录 mapping，用于确认复用路径没有重新创建索引（CreateIndex 会重置 docs 与 mapping）。
	reusedMapping := fakeES.createIndexMappings["knowledge_v1"]
	fakeES.createIndexCalls = nil
	fakeES.getIndexDimensionsCalls = nil
	fakeES.aliasIndex = "knowledge_v1"

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := NewIndexJobHandler(newEmptyResultDB(), fakeES, nil, nil, fakeRepo, nil, nil)

	// 不指定 index_name：应回退到 active profile.ESIndexName。
	job := &Job{
		Type:    types.JobRebuildIndex,
		Payload: map[string]interface{}{},
	}

	if err := handler.HandleJob(ctx, job); err != nil {
		t.Fatalf("维度一致时 rebuild 应成功，实际失败: %v", err)
	}

	// 维度一致 → 不新建任何索引。
	if len(fakeES.createIndexCalls) != 0 {
		t.Errorf("维度一致时不应创建索引，实际创建: %v", fakeES.createIndexCalls)
	}
	// 复用路径不得重新创建索引（CreateIndex 会重置 docs/mapping，导致既有数据丢失），
	// 且必须读取的是真实索引维度。
	if len(fakeES.getIndexDimensionsCalls) == 0 || fakeES.getIndexDimensionsCalls[0] != "knowledge_v1" {
		t.Fatalf("rebuild 应读取 active profile 索引名 knowledge_v1 的维度，实际调用: %v", fakeES.getIndexDimensionsCalls)
	}
	if fakeES.aliasIndex != "knowledge_v1" {
		t.Errorf("alias 应指向 knowledge_v1，实际 %s", fakeES.aliasIndex)
	}
	if savedMapping := fakeES.createIndexMappings["knowledge_v1"]; savedMapping == nil {
		t.Fatal("复用的 knowledge_v1 应保留原有 mapping")
	} else if got := mappingEmbeddingDims(savedMapping); got != mappingEmbeddingDims(reusedMapping) {
		t.Errorf("复用路径不应改写 knowledge_v1 的 mapping 维度，实际 %d", got)
	}
	// 回写仍按实现语义执行：索引名不变，但保证 profile 记录与 alias 一致。
	if len(fakeRepo.updatedIndexes) != 1 {
		t.Fatalf("期望 1 次 es_index_name 回写，实际 %d 次: %+v", len(fakeRepo.updatedIndexes), fakeRepo.updatedIndexes)
	}
	if got := fakeRepo.updatedIndexes[0]; got.ProfileID != "test-profile-id" || got.IndexName != "knowledge_v1" {
		t.Errorf("回写内容应为 (test-profile-id, knowledge_v1)，实际 %+v", got)
	}
}

// TestHandleRebuildIndex_WriteBackFailure_FailsJob 验证 profile 回写失败会让 job 失败。
//
// 引入动机：alias 已切到新索引但 profile 未记录新索引名时，profile 与 alias 会永久不一致；
// 该错误绝不能被吞掉，必须让 job 失败以暴露问题。
func TestHandleRebuildIndex_WriteBackFailure_FailsJob(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(1024, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	fakeES.aliasIndex = "knowledge_v1"

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	fakeRepo.updateErr = errors.New("PG 写入失败")
	handler := NewIndexJobHandler(newEmptyResultDB(), fakeES, nil, nil, fakeRepo, nil, nil)

	job := &Job{
		Type:    types.JobRebuildIndex,
		Payload: map[string]interface{}{"index_name": "knowledge_v1"},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("UpdateProfileESIndex 失败时 rebuild 必须返回错误")
	}
	if !containsStr(err.Error(), "回写 profile") {
		t.Errorf("错误信息应说明回写 profile 失败，实际: %v", err)
	}
	// 记录真实时序：alias 已切换，回写失败仍视为 job 失败。
	if fakeES.aliasIndex != "knowledge_v2" {
		t.Errorf("alias 应已切到 knowledge_v2，实际 %s", fakeES.aliasIndex)
	}
}

// TestHandleRebuildIndex_InvalidDimensions_FailsWithoutFallback 验证 dimensions <= 0 直接失败。
//
// 引入动机：原实现用 `dimensions = 1024` 静默兜底，正是本次维度事故的隐患来源；
// 移除兜底后必须显式报错，且不得创建任何索引。
func TestHandleRebuildIndex_InvalidDimensions_FailsWithoutFallback(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	fakeRepo := newDimensionSelfHealProfile(0, "knowledge_v1")
	handler := NewIndexJobHandler(newEmptyResultDB(), fakeES, nil, nil, fakeRepo, nil, nil)

	job := &Job{
		Type:    types.JobRebuildIndex,
		Payload: map[string]interface{}{"index_name": "knowledge_v1"},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("dimensions <= 0 时 rebuild 必须返回错误")
	}
	if !containsStr(err.Error(), "维度无效") {
		t.Errorf("错误信息应说明维度无效，实际: %v", err)
	}
	if len(fakeES.createIndexCalls) != 0 {
		t.Errorf("维度无效时不应创建索引，实际创建: %v", fakeES.createIndexCalls)
	}
}

// TestEnsureAliasAndIndex_InvalidDimensions_Fails 验证 alias 不存在且 dimensions <= 0 直接失败。
//
// 引入动机：ensureAliasAndIndex 中原有第二处 `dimensions = 1024` 兜底同样已被移除，
// 需要确认它不再静默创建 1024 维索引。
func TestEnsureAliasAndIndex_InvalidDimensions_Fails(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	handler := NewIndexJobHandler(newEmptyResultDB(), fakeES, nil, nil, nil, nil, nil)

	err := handler.ensureAliasAndIndex(ctx, 0, "standard", "test-profile-id")
	if err == nil {
		t.Fatal("dimensions <= 0 时 ensureAliasAndIndex 必须返回错误")
	}
	if !containsStr(err.Error(), "维度无效") {
		t.Errorf("错误信息应说明维度无效，实际: %v", err)
	}
	if len(fakeES.createIndexCalls) != 0 {
		t.Errorf("维度无效时不应创建索引，实际创建: %v", fakeES.createIndexCalls)
	}
}

// TestHandleIndexDocument_AliasDimensionMismatch_EnqueuesRebuildOnce 验证 index_document 兜底路径：
// alias 指向索引维度与 profile 不一致时 fail fast 并恰好投递一次 rebuild_index，重复调用不重复入队。
//
// 引入动机：这是"改了 profile 维度但没激活/重建"的兜底，必须自动请求重建并避免每次保存文档
// 都重复入队多个全量重建任务。
func TestHandleIndexDocument_AliasDimensionMismatch_EnqueuesRebuildOnce(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	// alias 指向一个 1024 维的旧索引。
	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(1024, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	fakeJobRepo := &fakeJobEnqueuer{}
	handler := NewIndexJobHandler(nil, fakeES, nil, nil, fakeRepo, fakeJobRepo, nil)

	job := &Job{
		Type:    types.JobIndexDocument,
		Payload: map[string]interface{}{"document_id": "doc-1"},
	}

	// 第一次：fail fast（写入 4096 维向量到 1024 维索引必然失败）。
	firstErr := handler.HandleJob(ctx, job)
	if firstErr == nil {
		t.Fatal("alias 索引维度与 profile 不一致时必须返回错误")
	}
	if !containsStr(firstErr.Error(), "不一致") {
		t.Errorf("错误信息应说明维度不一致，实际: %v", firstErr)
	}

	// 恰好 1 次 rebuild_index 入队，payload 指向新索引名与 profile。
	if len(fakeJobRepo.enqueued) != 1 {
		t.Fatalf("期望 1 次 rebuild_index 入队，实际 %d 次: %+v", len(fakeJobRepo.enqueued), fakeJobRepo.enqueued)
	}
	enqueued := fakeJobRepo.enqueued[0]
	if enqueued.JobType != types.JobRebuildIndex {
		t.Errorf("入队类型应为 %s，实际 %s", types.JobRebuildIndex, enqueued.JobType)
	}
	if got, _ := enqueued.Payload["index_name"].(string); got != "knowledge_v2" {
		t.Errorf("入队 payload 的 index_name 应为下一个可用索引 knowledge_v2，实际 %q", got)
	}
	if got, _ := enqueued.Payload["profile_id"].(string); got != "test-profile-id" {
		t.Errorf("入队 payload 的 profile_id 应为 test-profile-id，实际 %q", got)
	}

	// 兜底路径只请求重建，不得自行创建索引。
	if len(fakeES.createIndexCalls) != 0 {
		t.Errorf("兜底路径不应创建索引，实际创建: %v", fakeES.createIndexCalls)
	}

	// 第二次：队列中已有 pending 的 rebuild_index，不得重复入队。
	secondErr := handler.HandleJob(ctx, job)
	if secondErr == nil {
		t.Fatal("重复调用仍应因维度不一致返回错误")
	}
	if len(fakeJobRepo.enqueued) != 1 {
		t.Errorf("已存在 pending rebuild_index 时应跳过重复入队，实际入队 %d 次", len(fakeJobRepo.enqueued))
	}
}

// TestIndexDocumentEnqueue_ThenRebuild_SelfHealsDimensionMismatch 验证兜底链路闭环：
// index_document 因维度不一致入队的 rebuild_index payload，能被 rebuild 原样消费并完成自愈。
//
// 引入动机：两条路径之间的契约（入队 payload 的 index_name/profile_id 字段）必须真实可用，
// 否则兜底只会入队一个无法修复维度的 job。
func TestIndexDocumentEnqueue_ThenRebuild_SelfHealsDimensionMismatch(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	// 现场：alias 指向 1024 维旧索引，profile 期望 4096 维。
	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(1024, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	fakeJobRepo := &fakeJobEnqueuer{}
	handler := NewIndexJobHandler(newEmptyResultDB(), fakeES, nil, nil, fakeRepo, fakeJobRepo, nil)

	// 第一步：文档保存发现维度不一致 → fail fast + 入队 rebuild_index。
	if err := handler.HandleJob(ctx, &Job{
		Type:    types.JobIndexDocument,
		Payload: map[string]interface{}{"document_id": "doc-1"},
	}); err == nil {
		t.Fatal("维度不一致时 index_document 应返回错误")
	}
	if len(fakeJobRepo.enqueued) != 1 {
		t.Fatalf("期望 1 次 rebuild_index 入队，实际 %d 次", len(fakeJobRepo.enqueued))
	}

	// 第二步：模拟 worker 取出该 job 并执行（payload 原样传递）。
	if err := handler.HandleJob(ctx, &Job{
		Type:    types.JobRebuildIndex,
		Payload: fakeJobRepo.enqueued[0].Payload,
	}); err != nil {
		t.Fatalf("消费兜底入队的 rebuild_index 应成功完成自愈，实际失败: %v", err)
	}

	// 自愈结果：新索引维度正确、alias 指向它、profile 记录同步、旧索引保留。
	if got := mappingEmbeddingDims(fakeES.createIndexMappings["knowledge_v2"]); got != 4096 {
		t.Errorf("knowledge_v2 的 mapping embedding.dims 应为 4096，实际 %d", got)
	}
	if fakeES.aliasIndex != "knowledge_v2" {
		t.Errorf("alias 应指向 knowledge_v2，实际 %s", fakeES.aliasIndex)
	}
	if len(fakeRepo.updatedIndexes) != 1 {
		t.Fatalf("期望 1 次 es_index_name 回写，实际 %d 次: %+v", len(fakeRepo.updatedIndexes), fakeRepo.updatedIndexes)
	}
	if got := fakeRepo.updatedIndexes[0]; got.ProfileID != "test-profile-id" || got.IndexName != "knowledge_v2" {
		t.Errorf("回写内容应为 (test-profile-id, knowledge_v2)，实际 %+v", got)
	}
	if !fakeES.indices["knowledge_v1"] {
		t.Error("旧索引 knowledge_v1 应保留以支持 rollback")
	}
}

// TestHandleIndexDocument_AliasDimensionMismatch_RunningRebuildSkipsEnqueue 验证去重覆盖 running 状态。
// 引入动机：正在执行的 rebuild_index 同样不应被重复投递，否则会并发创建多个新索引。
func TestHandleIndexDocument_AliasDimensionMismatch_RunningRebuildSkipsEnqueue(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(1024, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	fakeES.aliasIndex = "knowledge_v1"

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	fakeJobRepo := &fakeJobEnqueuer{
		jobs: []Job{{ID: "running-rebuild", Type: types.JobRebuildIndex, Status: "running"}},
	}
	handler := NewIndexJobHandler(nil, fakeES, nil, nil, fakeRepo, fakeJobRepo, nil)

	job := &Job{
		Type:    types.JobIndexDocument,
		Payload: map[string]interface{}{"document_id": "doc-1"},
	}

	if err := handler.HandleJob(ctx, job); err == nil {
		t.Fatal("维度不一致时必须返回错误")
	}
	if len(fakeJobRepo.enqueued) != 0 {
		t.Errorf("已存在 running 的 rebuild_index 时不应入队，实际入队 %+v", fakeJobRepo.enqueued)
	}
}

// TestHandleIndexDocument_AliasDimensionMismatch_NoJobRepo_Fails 验证未注入 jobRepo 时不静默跳过。
//
// 引入动机：若在维度不一致时静默返回成功/继续，索引将永久不可写且没有任何自愈路径，
// 因此必须显式报错，让问题在 job 状态与日志中可见。
func TestHandleIndexDocument_AliasDimensionMismatch_NoJobRepo_Fails(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(1024, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	fakeES.aliasIndex = "knowledge_v1"

	fakeRepo := newDimensionSelfHealProfile(4096, "knowledge_v1")
	handler := NewIndexJobHandler(nil, fakeES, nil, nil, fakeRepo, nil, nil)

	job := &Job{
		Type:    types.JobIndexDocument,
		Payload: map[string]interface{}{"document_id": "doc-1"},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("维度不一致且 jobRepo 未注入时必须返回错误，不能静默跳过")
	}
	if !containsStr(err.Error(), "jobRepo 未注入") {
		t.Errorf("错误信息应说明 jobRepo 未注入导致无法自动重建，实际: %v", err)
	}
}

// TestEnsureAliasAndIndex_DimensionConsistent_NoEnqueue 验证 alias 维度与 profile 一致时无错误、无入队。
//
// 引入动机：兜底检查不得影响正常写入路径，维度一致时既不应报错也不应请求重建。
func TestEnsureAliasAndIndex_DimensionConsistent_NoEnqueue(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	if err := fakeES.CreateIndex(ctx, "knowledge_v1", es.BuildIndexMapping(4096, "standard")); err != nil {
		t.Fatalf("预置 knowledge_v1 失败: %v", err)
	}
	fakeES.aliasIndex = "knowledge_v1"
	fakeES.createIndexCalls = nil

	fakeJobRepo := &fakeJobEnqueuer{}
	handler := NewIndexJobHandler(nil, fakeES, nil, nil, nil, fakeJobRepo, nil)

	if err := handler.ensureAliasAndIndex(ctx, 4096, "standard", "test-profile-id"); err != nil {
		t.Fatalf("维度一致时 ensureAliasAndIndex 不应返回错误，实际: %v", err)
	}
	if len(fakeJobRepo.enqueued) != 0 {
		t.Errorf("维度一致时不应入队 rebuild_index，实际入队 %+v", fakeJobRepo.enqueued)
	}
	if len(fakeES.createIndexCalls) != 0 {
		t.Errorf("alias 已存在且维度一致时不应创建索引，实际创建: %v", fakeES.createIndexCalls)
	}
}
