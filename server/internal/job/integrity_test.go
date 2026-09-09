// integrity_test.go 测试索引完整性检查和修复逻辑。
//
// 引入动机：F2 要求 integrity_test.go 必须使用完整可控 es.Client fake，
// 实际调用 RepairIssue，验证 orphan/missing/revision/hash/chunk 的最小修复操作。
//
// 测试策略：
//   - orphan 修复：纯单元测试，不需要 PG，验证 DeleteByQuery 被调用
//   - missing/revision_mismatch/hash_mismatch/chunk_count_mismatch 修复：
//     集成测试，需要 PG（无 PG 时 skip），在 PG 中插入文档，
//     调用 RepairIssue，验证 ES fake 上发生了 DeleteByQuery + BulkIndex
//
// 集成测试隔离策略：
//   - 每个集成测试通过 testJobDB 获取独立 PG 连接
//   - testJobDB 真实调用 migration runner 执行项目全部 migration（M001-M006）
//   - 不依赖测试执行顺序或全局已有 schema
//   - setupTestDocument 清理自身数据，testJobDB 的 Cleanup 清理全部表
package job

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"partitura/server/internal/db/migration"
	"partitura/server/internal/es"
	types "partitura/server/internal/search/types"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// advisoryLockKey 是跨进程 advisory lock 的固定键值。
// 与 testutil 包使用同一个键，确保跨包序列化 DB 访问。
const jobAdvisoryLockKey int64 = 9527

// fakeESClient 是 es.Client 接口的完整可控 fake 实现。
// 引入动机：F2 要求使用完整可控 es.Client fake，实际调用 RepairIssue，
// 验证各问题类型的最小修复操作。此 fake 记录所有操作以便测试断言。
type fakeESClient struct {
	indices       map[string]bool
	aliasIndex    string
	docs          map[string]map[string]map[string]interface{} // index -> docID -> body
	pingOK        bool
	deleteByQueryCalls []deleteByQueryCall
	bulkIndexCalls     []bulkIndexCall
	searchCalls         []searchCall
}

// deleteByQueryCall 记录一次 DeleteByQuery 调用的参数。
type deleteByQueryCall struct {
	IndexName string
	Query     map[string]interface{}
}

// bulkIndexCall 记录一次 BulkIndex 调用的参数。
type bulkIndexCall struct {
	IndexName string
	Docs      []es.IndexDoc
}

// searchCall 记录一次 Search 调用的参数。
type searchCall struct {
	IndexName string
	Query     map[string]interface{}
}

func newFakeESClient() *fakeESClient {
	return &fakeESClient{
		indices: make(map[string]bool),
		docs:    make(map[string]map[string]map[string]interface{}),
		pingOK:  true,
	}
}

func (c *fakeESClient) Ping(ctx context.Context) error {
	if !c.pingOK {
		return fmt.Errorf("ES 不可用")
	}
	return nil
}

func (c *fakeESClient) CreateIndex(ctx context.Context, indexName string, mapping map[string]interface{}) error {
	c.indices[indexName] = true
	c.docs[indexName] = make(map[string]map[string]interface{})
	return nil
}

func (c *fakeESClient) DeleteIndex(ctx context.Context, indexName string) error {
	delete(c.indices, indexName)
	delete(c.docs, indexName)
	return nil
}

func (c *fakeESClient) IndexExists(ctx context.Context, indexName string) (bool, error) {
	return c.indices[indexName], nil
}

func (c *fakeESClient) UpdateAlias(ctx context.Context, actions []es.AliasAction) error {
	for _, a := range actions {
		if a.Action == "add" {
			c.aliasIndex = a.Index
		}
	}
	return nil
}

func (c *fakeESClient) GetAliasIndex(ctx context.Context, alias string) (string, error) {
	if c.aliasIndex == "" {
		return "", fmt.Errorf("alias %s 不存在", alias)
	}
	return c.aliasIndex, nil
}

func (c *fakeESClient) BulkIndex(ctx context.Context, indexName string, docs []es.IndexDoc) error {
	c.bulkIndexCalls = append(c.bulkIndexCalls, bulkIndexCall{IndexName: indexName, Docs: docs})
	if c.docs[indexName] == nil {
		c.docs[indexName] = make(map[string]map[string]interface{})
	}
	for _, doc := range docs {
		c.docs[indexName][doc.ID] = doc.Body
	}
	return nil
}

func (c *fakeESClient) DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error {
	c.deleteByQueryCalls = append(c.deleteByQueryCalls, deleteByQueryCall{IndexName: indexName, Query: query})
	if c.docs[indexName] != nil {
		// 简化：按 document_id term 删除匹配文档
		if term, ok := query["term"].(map[string]interface{}); ok {
			if docID, ok := term["document_id"].(string); ok {
				for id := range c.docs[indexName] {
					if body := c.docs[indexName][id]; body != nil {
						if did, ok := body["document_id"].(string); ok && did == docID {
							delete(c.docs[indexName], id)
						}
					}
				}
				return nil
			}
		}
		// 无法解析 query，清空所有
		c.docs[indexName] = make(map[string]map[string]interface{})
	}
	return nil
}

// fakeDocEntry 表示一个匹配的文档条目。
type fakeDocEntry struct {
	id   string
	body map[string]interface{}
}

func (c *fakeESClient) Search(ctx context.Context, indexName string, query map[string]interface{}) (*es.SearchResponse, error) {
	c.searchCalls = append(c.searchCalls, searchCall{IndexName: indexName, Query: query})
	resp := &es.SearchResponse{}
	if c.docs[indexName] == nil {
		return resp, nil
	}

	// 提取 query 过滤条件
	queryFilter := query["query"]

	// 按 query 过滤文档
	var matching []fakeDocEntry
	for id, body := range c.docs[indexName] {
		if fakeMatchesQuery(body, queryFilter) {
			matching = append(matching, fakeDocEntry{id: id, body: body})
		}
	}

	// 如果是 aggregation 查询，构建 aggregation 响应
	if aggs, hasAggs := query["aggs"]; hasAggs {
		return buildFakeAggregationResponse(aggs, matching), nil
	}

	// 返回匹配的文档作为 hits
	for _, e := range matching {
		hit := struct {
			ID     string                 `json:"_id"`
			Score  float64                `json:"_score"`
			Source map[string]interface{} `json:"_source"`
		}{ID: e.id, Score: 1.0, Source: e.body}
		resp.Hits.Hits = append(resp.Hits.Hits, hit)
	}
	resp.Hits.Total.Value = int64(len(resp.Hits.Hits))
	return resp, nil
}

// fakeMatchesQuery 检查文档 body 是否匹配 ES query 过滤条件。
// 支持 nil query（match all）、term query 和 bool must query。
func fakeMatchesQuery(body map[string]interface{}, query interface{}) bool {
	if query == nil {
		return true
	}
	qm, ok := query.(map[string]interface{})
	if !ok {
		return true
	}

	// term query: {"term": {"field": value}}
	if term, ok := qm["term"].(map[string]interface{}); ok {
		for field, value := range term {
			if !fakeMatchTerm(body, field, value) {
				return false
			}
		}
		return true
	}

	// bool query: {"bool": {"must": [...]}}
	if boolQ, ok := qm["bool"].(map[string]interface{}); ok {
		if must, ok := boolQ["must"].([]interface{}); ok {
			for _, clause := range must {
				if clauseMap, ok := clause.(map[string]interface{}); ok {
					if !fakeMatchesQuery(body, clauseMap) {
						return false
					}
				}
			}
			return true
		}
	}

	return true
}

// fakeMatchTerm 检查文档 body 中指定字段的值是否匹配。
func fakeMatchTerm(body map[string]interface{}, field string, value interface{}) bool {
	fieldVal, ok := body[field]
	if !ok {
		return false
	}
	// 类型兼容比较（Go 的 int vs float64 等）
	return fmt.Sprintf("%v", fieldVal) == fmt.Sprintf("%v", value)
}

// buildFakeAggregationResponse 从匹配的文档构建 ES aggregation 响应。
// 支持 terms aggregation（按 document_id 分桶）和子聚合 value_count、max、terms。
func buildFakeAggregationResponse(aggs interface{}, matching []fakeDocEntry) *es.SearchResponse {
	resp := &es.SearchResponse{}
	aggsMap, ok := aggs.(map[string]interface{})
	if !ok {
		return resp
	}

	// 找到 terms aggregation 的字段名
	for aggName, aggDef := range aggsMap {
		aggDefMap, ok := aggDef.(map[string]interface{})
		if !ok {
			continue
		}

		// terms aggregation
		if termsDef, ok := aggDefMap["terms"].(map[string]interface{}); ok {
			field, _ := termsDef["field"].(string)
			if field == "" {
				continue
			}

			// 按 field 分桶
			buckets := make(map[string][]map[string]interface{})
			for _, e := range matching {
				key, _ := e.body[field].(string)
				if key == "" {
					// 尝试其他类型
					key = fmt.Sprintf("%v", e.body[field])
				}
				buckets[key] = append(buckets[key], e.body)
			}

			// 构建 buckets 响应
			var bucketsList []interface{}
			for key, docs := range buckets {
				bucket := map[string]interface{}{
					"key":       key,
					"doc_count": len(docs),
				}

				// 处理子聚合
				if subAggs, ok := aggDefMap["aggs"].(map[string]interface{}); ok {
					for subName, subDef := range subAggs {
						subDefMap, ok := subDef.(map[string]interface{})
						if !ok {
							continue
						}

						if _, hasVC := subDefMap["value_count"]; hasVC {
							vcField, _ := subDefMap["value_count"].(map[string]interface{})["field"].(string)
							count := 0
							for _, d := range docs {
								if _, ok := d[vcField]; ok {
									count++
								}
							}
							bucket[subName] = map[string]interface{}{"value": float64(count)}
						}

						if _, hasMax := subDefMap["max"]; hasMax {
							maxField, _ := subDefMap["max"].(map[string]interface{})["field"].(string)
							var maxVal float64
							for _, d := range docs {
								if v, ok := d[maxField]; ok {
									var fv float64
									switch n := v.(type) {
								 case int:
										fv = float64(n)
									case float64:
										fv = n
									default:
										continue
									}
									if fv > maxVal {
										maxVal = fv
									}
								}
							}
							bucket[subName] = map[string]interface{}{"value": maxVal}
						}

						if subTermsDef, ok := subDefMap["terms"].(map[string]interface{}); ok {
							subField, _ := subTermsDef["field"].(string)
							subSize := 1
							if s, ok := subTermsDef["size"].(int); ok {
								subSize = s
							}
							subBuckets := make(map[string]int)
							for _, d := range docs {
								if v, ok := d[subField].(string); ok {
									subBuckets[v]++
								}
							}
							var subBucketsList []interface{}
							count := 0
							for k, c := range subBuckets {
								if count >= subSize {
									break
								}
								subBucketsList = append(subBucketsList, map[string]interface{}{
									"key":       k,
									"doc_count": c,
								})
								count++
							}
							bucket[subName] = map[string]interface{}{"buckets": subBucketsList}
						}
					}
				}

				bucketsList = append(bucketsList, bucket)
			}

			resp.Aggregations = map[string]interface{}{
				aggName: map[string]interface{}{
					"buckets": bucketsList,
				},
			}
		}
	}

	return resp
}

func (c *fakeESClient) Count(ctx context.Context, indexName string, query map[string]interface{}) (int64, error) {
	if c.docs[indexName] == nil {
		return 0, nil
	}
	return int64(len(c.docs[indexName])), nil
}

func (c *fakeESClient) GetDocument(ctx context.Context, indexName, docID string) (map[string]interface{}, error) {
	if c.docs[indexName] == nil {
		return nil, nil
	}
	return c.docs[indexName][docID], nil
}

func (c *fakeESClient) Refresh(ctx context.Context, indexName string) error {
	return nil
}

func (c *fakeESClient) ListIndices(ctx context.Context, pattern string) ([]string, error) {
	result := make([]string, 0, len(c.indices))
	for name := range c.indices {
		result = append(result, name)
	}
	return result, nil
}

// --- 测试辅助 ---

// testJobDB 返回测试用 PG 连接，无 TEST_DATABASE_URL 时 skip。
// 使用跨进程 advisory lock + 完整 reset 隔离，不依赖测试执行顺序。
//
// 连接生命周期（V4 修复）：
//   - 使用专用 *sql.Conn 持有 session-level advisory lock
//   - lock、reset、unlock 和 close 全部在同一 Conn 上执行
//   - 避免连接池跨连接 unlock 导致锁泄漏
//
// advisory lock 等待策略：
//   - 使用 t.Context() 作为等待 context，不设固定 deadline
//   - 不同 Go test package 并行运行时，等待方会阻塞直到持锁方释放
//   - 不会因固定 60s 超时而假失败——等待时间仅受测试自身 deadline 约束
//   - cleanup 时一定释放锁（defer unlock + Conn.Close）
func testJobDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("跳过集成测试：未设置 TEST_DATABASE_URL 环境变量")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	// 使用独立的 ping context 检查连接可用性
	pingCtx, pingCancel := context.WithCancel(context.Background())
	if err := db.PingContext(pingCtx); err != nil {
		pingCancel()
		_ = db.Close()
		t.Skipf("跳过集成测试：无法连接 PG (%v)", err)
	}
	pingCancel()

	// 获取专用连接用于持有 session-level advisory lock。
	// pg_advisory_lock 绑定到执行该语句的连接，因此 unlock 必须在同一连接上执行。
	conn, err := db.Conn(t.Context())
	if err != nil {
		_ = db.Close()
		t.Fatalf("获取专用数据库连接失败: %v", err)
	}

	// 使用 t.Context() 获取 advisory lock——不设固定 deadline，
	// 等待时间仅受测试自身 deadline 约束。
	lockCtx := t.Context()
	if _, err := conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1)", jobAdvisoryLockKey); err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatalf("获取 advisory lock 失败: %v", err)
	}

	// 完整 reset：清理可能残留的表（依赖逆序，在同一连接上执行）
	cleanJobTestDBConn(lockCtx, conn)

	// 真实调用 migration runner 应用项目全部 migration
	migrationsDir := findJobMigrationsDir(t)
	migrations, err := migration.LoadMigrations(migrationsDir)
	if err != nil {
		releaseJobLockAndCloseConn(conn, db, t)
		t.Fatalf("加载迁移文件失败: %v", err)
	}
	runner := migration.NewRunner(db, migrations)
	if _, err := runner.Up(lockCtx); err != nil {
		releaseJobLockAndCloseConn(conn, db, t)
		t.Fatalf("执行迁移 Up 失败: %v", err)
	}

	t.Cleanup(func() {
		cleanJobTestDBConn(context.Background(), conn)
		releaseJobLockAndCloseConn(conn, db, t)
	})
	return db
}

// releaseJobLockAndCloseConn 在专用连接上释放 advisory lock，然后关闭连接和连接池。
// 必须在持有锁的同一 *sql.Conn 上执行 pg_advisory_unlock，否则 unlock 会发送到
// 不同连接，无法释放锁。使用 context.Background() 确保 cleanup 阶段即使测试
// context 已取消也能执行 unlock。
func releaseJobLockAndCloseConn(conn *sql.Conn, db *sql.DB, t *testing.T) {
	_, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", jobAdvisoryLockKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "释放 advisory lock 失败: %v\n", err)
		if t != nil {
			t.Errorf("释放 advisory lock 失败: %v", err)
		}
	}
	if err := conn.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "关闭专用数据库连接失败: %v\n", err)
		if t != nil {
			t.Errorf("关闭专用数据库连接失败: %v", err)
		}
	}
	_ = db.Close()
}

// cleanJobTestDBConn 在专用连接上清理测试数据库中的所有表和扩展。
func cleanJobTestDBConn(ctx context.Context, conn *sql.Conn) {
	tables := []string{
		"evaluation_results", "search_metrics", "search_feedback",
		"evaluation_items", "evaluation_datasets",
		"search_profile_candidates", "jobs", "search_profiles",
		"sources", "revisions", "documents",
		"audit_logs", "workspace_members", "workspaces",
		"device_authorizations",
		"device_sessions", "sessions", "users", "schema_migrations",
	}
	for _, table := range tables {
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table))
	}
	_, _ = conn.ExecContext(ctx, `DROP EXTENSION IF EXISTS "pgcrypto"`)
}

// findJobMigrationsDir 定位项目中的 migrations 目录。
// 引入动机：testJobDB 需要加载项目迁移文件以应用全部 migration。
// 使用 runtime.Caller 从本源码文件定位项目根目录的 migrations 子目录，
// 不依赖 CWD 或环境变量，对 go test ./... 各 package 不同 cwd 可靠。
func findJobMigrationsDir(t *testing.T) string {
	t.Helper()
	// 通过 runtime.Caller 获取本文件的绝对路径
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败，无法定位 integrity_test.go 源码文件")
	}
	// 本文件位于 server/internal/job/integrity_test.go
	// migrations 目录位于 server/migrations
	// 从本文件向上 3 级到达 server/ 目录
	dir := filepath.Dir(filename) // .../server/internal/job
	dir = filepath.Dir(dir)       // .../server/internal
	dir = filepath.Dir(dir)       // .../server
	migrationsDir := filepath.Join(dir, "migrations")

	info, err := os.Stat(migrationsDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("无法定位 migrations 目录: %s (从 %s 推导)", migrationsDir, filename)
	}

	return migrationsDir
}

// --- F2 测试：实际调用 RepairIssue，验证各问题类型的最小修复操作 ---

// TestRepairIssue_Orphan 验证 orphan 修复调用 DeleteByQuery。
// 引入动机：F2 要求实际调用 RepairIssue 验证 orphan 的最小修复操作。
// orphan 修复只需 ES DeleteByQuery，不需要 PG。
func TestRepairIssue_Orphan(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	// 预置 orphan chunk
	_ = fakeES.BulkIndex(ctx, "test_index", []es.IndexDoc{
		{ID: "orphan_doc_0", Body: map[string]interface{}{"document_id": "orphan_doc"}},
	})
	// 重置操作记录，只追踪 RepairIssue 触发的操作
	fakeES.deleteByQueryCalls = nil
	fakeES.bulkIndexCalls = nil

	issue := IntegrityIssue{
		Type:       "orphan",
		DocumentID: "orphan_doc",
		Detail:     "ES 中存在 chunk 但 PG 中文档已删除",
	}

	err := RepairIssue(ctx, nil, fakeES, "test_index", issue, nil, nil)
	if err != nil {
		t.Fatalf("RepairIssue orphan 失败: %v", err)
	}

	// 验证 DeleteByQuery 被调用
	if len(fakeES.deleteByQueryCalls) != 1 {
		t.Fatalf("期望 1 次 DeleteByQuery 调用，实际 %d 次", len(fakeES.deleteByQueryCalls))
	}
	call := fakeES.deleteByQueryCalls[0]
	if call.IndexName != "test_index" {
		t.Errorf("DeleteByQuery 索引名 = %s, 期望 test_index", call.IndexName)
	}
	// 验证 query 包含 document_id term filter
	term, ok := call.Query["term"].(map[string]interface{})
	if !ok {
		t.Fatal("DeleteByQuery query 应包含 term")
	}
	if term["document_id"] != "orphan_doc" {
		t.Errorf("term document_id = %v, 期望 orphan_doc", term["document_id"])
	}

	// 验证 orphan chunk 已从 ES fake 中删除
	if doc := fakeES.docs["test_index"]["orphan_doc_0"]; doc != nil {
		t.Error("orphan chunk 应已从 ES 删除")
	}

	// 验证没有 BulkIndex 调用（orphan 只删除，不重新索引）
	if len(fakeES.bulkIndexCalls) != 0 {
		t.Errorf("orphan 修复不应触发 BulkIndex，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
}

// TestRepairIssue_Missing 验证 missing 修复执行重新索引。
// 引入动机：F2 要求实际调用 RepairIssue 验证 missing 的最小修复操作。
// missing 修复需要从 PG 读取文档并写入 ES，需要 PG 连接。
func TestRepairIssue_Missing(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 准备测试数据：在 PG 中插入 workspace + document
	wsID, docID := setupTestDocument(t, db, "missing_test_doc", "Missing Test", "# Test\n\nContent for missing test.\n")

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	issue := IntegrityIssue{
		Type:        "missing",
		DocumentID:  docID,
		WorkspaceID: wsID,
		Detail:      "文档在 PG 中存在但 ES 中缺少 chunk",
	}

	err := RepairIssue(ctx, db, fakeES, "test_index", issue, nil, nil)
	if err != nil {
		t.Fatalf("RepairIssue missing 失败: %v", err)
	}

	// 验证 DeleteByQuery 被调用（删除旧 chunk）
	if len(fakeES.deleteByQueryCalls) < 1 {
		t.Fatal("期望至少 1 次 DeleteByQuery 调用（删除旧 chunk）")
	}

	// 验证 BulkIndex 被调用（写入新 chunk）
	if len(fakeES.bulkIndexCalls) != 1 {
		t.Fatalf("期望 1 次 BulkIndex 调用，实际 %d 次", len(fakeES.bulkIndexCalls))
	}
	bulkCall := fakeES.bulkIndexCalls[0]
	if bulkCall.IndexName != "test_index" {
		t.Errorf("BulkIndex 索引名 = %s, 期望 test_index", bulkCall.IndexName)
	}
	if len(bulkCall.Docs) == 0 {
		t.Error("BulkIndex 应写入至少 1 个 chunk")
	}
	// 验证 chunk 的 document_id 正确
	for _, doc := range bulkCall.Docs {
		if did, ok := doc.Body["document_id"].(string); ok {
			if did != docID {
				t.Errorf("chunk document_id = %s, 期望 %s", did, docID)
			}
		} else {
			t.Error("chunk body 缺少 document_id")
		}
	}
}

// TestRepairIssue_RevisionMismatch 验证 revision_mismatch 修复执行重新索引。
func TestRepairIssue_RevisionMismatch(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "rev_mismatch_doc", "Rev Mismatch", "# Rev\n\nRevision mismatch test content.\n")

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	issue := IntegrityIssue{
		Type:       "revision_mismatch",
		DocumentID: docID,
		WorkspaceID: wsID,
		Detail:     "ES revision 与 PG revision 不匹配",
	}

	err := RepairIssue(ctx, db, fakeES, "test_index", issue, nil, nil)
	if err != nil {
		t.Fatalf("RepairIssue revision_mismatch 失败: %v", err)
	}

	// 验证重新索引：DeleteByQuery + BulkIndex
	if len(fakeES.deleteByQueryCalls) < 1 {
		t.Fatal("期望至少 1 次 DeleteByQuery")
	}
	if len(fakeES.bulkIndexCalls) != 1 {
		t.Fatalf("期望 1 次 BulkIndex，实际 %d", len(fakeES.bulkIndexCalls))
	}

	// 验证写入的 chunk revision 与 PG 一致（revision_number = 1）
	for _, doc := range fakeES.bulkIndexCalls[0].Docs {
		if rev, ok := doc.Body["revision"].(int); ok {
			if rev != 1 {
				t.Errorf("chunk revision = %d, 期望 1", rev)
			}
		}
	}
}

// TestRepairIssue_HashMismatch 验证 hash_mismatch 修复执行重新索引。
func TestRepairIssue_HashMismatch(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "hash_mismatch_doc", "Hash Mismatch", "# Hash\n\nHash mismatch test content.\n")

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	issue := IntegrityIssue{
		Type:       "hash_mismatch",
		DocumentID: docID,
		WorkspaceID: wsID,
		Detail:     "ES content_hash 与 PG 不匹配",
	}

	err := RepairIssue(ctx, db, fakeES, "test_index", issue, nil, nil)
	if err != nil {
		t.Fatalf("RepairIssue hash_mismatch 失败: %v", err)
	}

	if len(fakeES.bulkIndexCalls) != 1 {
		t.Fatalf("期望 1 次 BulkIndex，实际 %d", len(fakeES.bulkIndexCalls))
	}
	// 验证 chunk 包含 content_hash
	for _, doc := range fakeES.bulkIndexCalls[0].Docs {
		if hash, ok := doc.Body["content_hash"].(string); ok {
			if hash == "" {
				t.Error("chunk content_hash 不应为空")
			}
		} else {
			t.Error("chunk body 缺少 content_hash")
		}
	}
}

// TestRepairIssue_ChunkCountMismatch 验证 chunk_count_mismatch 修复执行重新索引。
func TestRepairIssue_ChunkCountMismatch(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "chunk_mismatch_doc", "Chunk Mismatch", "# Chunk\n\nChunk count mismatch test.\n\n## Section 1\n\nMore content here.\n")

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	issue := IntegrityIssue{
		Type:       "chunk_count_mismatch",
		DocumentID: docID,
		WorkspaceID: wsID,
		Detail:     "ES chunk count 与 PG 期望不匹配",
	}

	err := RepairIssue(ctx, db, fakeES, "test_index", issue, nil, nil)
	if err != nil {
		t.Fatalf("RepairIssue chunk_count_mismatch 失败: %v", err)
	}

	if len(fakeES.bulkIndexCalls) != 1 {
		t.Fatalf("期望 1 次 BulkIndex，实际 %d", len(fakeES.bulkIndexCalls))
	}
	if len(fakeES.bulkIndexCalls[0].Docs) == 0 {
		t.Error("BulkIndex 应写入至少 1 个 chunk")
	}
}

// TestRepairIssue_UnknownType 验证未知问题类型返回错误。
func TestRepairIssue_UnknownType(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()

	issue := IntegrityIssue{
		Type:       "unknown_type",
		DocumentID: "doc1",
	}

	err := RepairIssue(ctx, nil, fakeES, "test_index", issue, nil, nil)
	if err == nil {
		t.Fatal("未知问题类型应返回错误")
	}
}

// --- 新增测试：stale 检测、特殊文档检测、rebuild 失败不切 alias ---

// TestCheckIntegrity_StaleDetection 验证 CheckIntegrity 正确检测 stale chunk
// （ES revision < PG revision）并分类为 "stale" 而非 "revision_mismatch"。
//
// 引入动机：design/01-SEARCH.md §Index Integrity 要求检查 stale chunks。
// stale 表示 ES 中的 chunk 已过时（PG 有更新版本），与 revision_mismatch
// （ES revision > PG revision，异常状态）区分。
//
// 测试策略：使用 fakeESClient 预置一个 revision=1 的 chunk，模拟 PG 中
// revision=2 的文档，验证 CheckIntegrity 返回 "stale" 类型 issue。
func TestCheckIntegrity_StaleDetection(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "stale_test_doc", "Stale Test", "# Stale\n\nContent.\n")
	_ = wsID

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	// 预置 ES chunk with revision=1（PG 中 revision_number=1，匹配）
	// 然后更新 PG 文档 revision 到 2，使 ES chunk 变为 stale
	_ = fakeES.BulkIndex(ctx, "test_index", []es.IndexDoc{
		{
			ID: docID + "_0",
			Body: map[string]interface{}{
				"document_id":  docID,
				"workspace_id": wsID,
				"path":          "stale_test_doc",
				"title":         "Stale Test",
				"content":       "# Stale\n\nContent.\n",
				"revision":      1,
				"content_hash":  "old_hash",
				"is_special":    false,
				"status":        "active",
			},
		},
	})

	// 更新 PG 文档 revision 到 2（模拟文档已更新但索引未跟上）
	_, err := db.ExecContext(ctx,
		`UPDATE documents SET revision_number = 2, content_hash = $2 WHERE id = $1`,
		docID, "new_hash",
	)
	if err != nil {
		t.Fatalf("更新文档 revision 失败: %v", err)
	}

	result, err := CheckIntegrity(ctx, db, fakeES, "test_index", "", 512, 64)
	if err != nil {
		t.Fatalf("CheckIntegrity 失败: %v", err)
	}

	if !result.CheckedOK {
		t.Fatal("CheckIntegrity 应完成检查")
	}

	// 查找 stale issue
	var staleIssue *IntegrityIssue
	for i := range result.Issues {
		if result.Issues[i].Type == "stale" {
			staleIssue = &result.Issues[i]
			break
		}
	}
	if staleIssue == nil {
		t.Fatalf("期望发现 stale 类型 issue，实际 issues: %v", result.Issues)
	}
	if staleIssue.DocumentID != docID {
		t.Errorf("stale issue document_id = %s, 期望 %s", staleIssue.DocumentID, docID)
	}
}

// TestCheckIntegrity_RevisionMismatch_HigherESRevision 验证当 ES revision > PG revision 时
// 分类为 "revision_mismatch" 而非 "stale"。
func TestCheckIntegrity_RevisionMismatch_HigherESRevision(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "rev_high_es", "Rev High ES", "# Rev\n\nContent.\n")
	_ = wsID

	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	// 预置 ES chunk with revision=5（PG 中 revision_number=1，ES revision 更高）
	_ = fakeES.BulkIndex(ctx, "test_index", []es.IndexDoc{
		{
			ID: docID + "_0",
			Body: map[string]interface{}{
				"document_id":  docID,
				"workspace_id": wsID,
				"path":          "rev_high_es",
				"title":         "Rev High ES",
				"content":       "# Rev\n\nContent.\n",
				"revision":      5,
				"content_hash":  "some_hash",
				"is_special":    false,
				"status":        "active",
			},
		},
	})

	result, err := CheckIntegrity(ctx, db, fakeES, "test_index", "", 512, 64)
	if err != nil {
		t.Fatalf("CheckIntegrity 失败: %v", err)
	}

	if !result.CheckedOK {
		t.Fatal("CheckIntegrity 应完成检查")
	}

	// 查找 revision_mismatch issue（不应是 stale）
	var mismatchIssue *IntegrityIssue
	for i := range result.Issues {
		if result.Issues[i].Type == "revision_mismatch" {
			mismatchIssue = &result.Issues[i]
			break
		}
	}
	if mismatchIssue == nil {
		t.Fatalf("期望发现 revision_mismatch 类型 issue，实际 issues: %v", result.Issues)
	}

	// 确保没有 stale issue
	for _, issue := range result.Issues {
		if issue.Type == "stale" {
			t.Error("ES revision > PG revision 不应分类为 stale")
		}
	}
}

// TestCheckIntegrity_SpecialDocsInES 验证 CheckIntegrity 检测 ES 中的特殊文档 chunk。
//
// 引入动机：design/00-MASTER.md §特殊文件 要求 PROJECT.md/AGENTS.md 不进入搜索索引，
// integrity check 应检测 ES 中是否存在 is_special=true 的 chunk 并报告为 orphan。
func TestCheckIntegrity_SpecialDocsInES(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	// 预置一个 is_special=true 的 chunk 到 ES（不应存在）
	_ = fakeES.BulkIndex(ctx, "test_index", []es.IndexDoc{
		{
			ID: "special_doc_0",
			Body: map[string]interface{}{
				"document_id":  "special_doc_id",
				"workspace_id": "ws1",
				"path":         "PROJECT.md",
				"title":        "PROJECT",
				"content":      "Project info",
				"revision":     1,
				"content_hash": "hash1",
				"is_special":   true,
				"status":       "active",
			},
		},
	})

	// CheckIntegrity 需要 db，但特殊文档检测不依赖 PG。
	// 使用 nil db 会导致 readPGDocuments panic，所以我们用 testJobDB。
	// 但如果没有 PG，我们仍然可以验证 checkSpecialDocsInES 函数。
	issues := checkSpecialDocsInES(ctx, fakeES, "test_index", "")
	if len(issues) == 0 {
		t.Fatal("应检测到 ES 中的特殊文档 chunk")
	}
	if issues[0].Type != "orphan" {
		t.Errorf("特殊文档 chunk 应分类为 orphan，实际 %s", issues[0].Type)
	}
	if issues[0].DocumentID != "special_doc_id" {
		t.Errorf("特殊文档 document_id = %s, 期望 special_doc_id", issues[0].DocumentID)
	}
}

// TestCheckIntegrity_NoSpecialDocsInES 验证 ES 中无特殊文档时不产生 orphan issue。
func TestCheckIntegrity_NoSpecialDocsInES(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	// 只预置非特殊文档
	_ = fakeES.BulkIndex(ctx, "test_index", []es.IndexDoc{
		{
			ID: "normal_doc_0",
			Body: map[string]interface{}{
				"document_id":  "normal_doc_id",
				"workspace_id": "ws1",
				"is_special":   false,
			},
		},
	})

	issues := checkSpecialDocsInES(ctx, fakeES, "test_index", "")
	if len(issues) != 0 {
		t.Fatalf("无特殊文档时不应产生 orphan issue，实际 %d 个", len(issues))
	}
}

// TestHandleRebuildIndex_LexicalFailure_NoAliasSwitch 验证 lexical-only 模式下
// 文档索引失败时不切换 alias。
//
// 引入动机：design/01-SEARCH.md §Index Version 要求"验证完整性 → alias 原子切换"，
// 新索引不完整时切换 alias 会导致搜索结果缺失。
func TestHandleRebuildIndex_LexicalFailure_NoAliasSwitch(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, _ := setupTestDocument(t, db, "lexical_fail_doc", "Lexical Fail",
		"# Test\n\nContent for lexical fail test.\n")
	_ = wsID

	// 使用可配置的 fake ES client，让 BulkIndex 返回错误
	failingES := &failingBulkESClient{
		fakeESClient:   newFakeESClient(),
		bulkIndexError: fmt.Errorf("ES bulk index 部分失败"),
	}
	_ = failingES.CreateIndex(ctx, "test_fail_index", nil)

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:              "test-profile",
			ESIndexName:     "test_fail_index",
			ChunkTargetSize: 512,
			ChunkOverlap:    64,
			EmbeddingDimensions: 1024,
			Analyzer:       "standard",
		},
	}

	// Provider 未配置（lexical-only）
	handler := NewIndexJobHandler(db, failingES, nil, nil, fakeRepo, nil)

	job := &Job{
		Type: types.JobRebuildIndex,
		Payload: map[string]interface{}{
			"index_name": "test_fail_index",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("lexical-only 模式下文档索引失败时 rebuild 应返回错误")
	}

	// 验证 alias 未切换
	if failingES.aliasIndex != "" {
		t.Errorf("alias 不应被切换，实际指向 %s", failingES.aliasIndex)
	}

	// 验证错误信息包含"不切换 alias"
	if !strings.Contains(err.Error(), "不切换 alias") {
		t.Errorf("错误信息应包含'不切换 alias'，实际: %s", err.Error())
	}
}

// TestHandleRebuildIndex_RepairFailure_NoAliasSwitch 验证 rebuild 后 integrity 修复失败时
// 不切换 alias。
//
// 引入动机：design/01-SEARCH.md §Index Version 要求"验证完整性 → alias 原子切换"，
// 修复失败意味着完整性未通过验证，不应切换 alias。
func TestHandleRebuildIndex_RepairFailure_NoAliasSwitch(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	wsID, docID := setupTestDocument(t, db, "repair_fail_doc", "Repair Fail",
		"# Test\n\nContent for repair fail test.\n")
	_ = wsID
	_ = docID

	// 使用 fake ES client，BulkIndex 成功但 DeleteByQuery 在第二次调用时失败
	// （模拟 repair 时 DeleteByQuery 失败）
	failingES := &failingDeleteByQueryESClient{
		fakeESClient:      newFakeESClient(),
		failAfterNCalls:   2, // 前两次成功（rebuild 时的 delete + bulk），第三次失败（repair 时的 delete）
		deleteByQueryCallCount: 0,
	}
	_ = failingES.CreateIndex(ctx, "test_repair_fail", nil)

	fakeRepo := &fakeProfileRepo{
		profile: &ProfileForJob{
			ID:              "test-profile",
			ESIndexName:     "test_repair_fail",
			ChunkTargetSize: 512,
			ChunkOverlap:    64,
			EmbeddingDimensions: 1024,
			Analyzer:       "standard",
		},
	}

	handler := NewIndexJobHandler(db, failingES, nil, nil, fakeRepo, nil)

	job := &Job{
		Type: types.JobRebuildIndex,
		Payload: map[string]interface{}{
			"index_name": "test_repair_fail",
		},
	}

	err := handler.HandleJob(ctx, job)
	if err == nil {
		t.Fatal("integrity 修复失败时 rebuild 应返回错误")
	}

	// 验证 alias 未切换
	if failingES.aliasIndex != "" {
		t.Errorf("alias 不应被切换，实际指向 %s", failingES.aliasIndex)
	}
}

// TestReadESDocumentMeta_FakeAggregation 验证 readESDocumentMeta 能正确解析
// fakeESClient 返回的 aggregation 响应。
//
// 引入动机：fakeESClient.Search 已增强为支持 aggregation 查询，
// readESDocumentMeta 依赖此功能获取每个文档的 chunk count、revision 和 content_hash。
// 此测试不需要 PG，纯单元测试验证 fake → readESDocumentMeta 链路。
func TestReadESDocumentMeta_FakeAggregation(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	// 预置两个文档的 chunk
	_ = fakeES.BulkIndex(ctx, "test_index", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id": "doc1", "revision": 3, "content_hash": "hash1",
		}},
		{ID: "doc1_1", Body: map[string]interface{}{
			"document_id": "doc1", "revision": 3, "content_hash": "hash1",
		}},
		{ID: "doc2_0", Body: map[string]interface{}{
			"document_id": "doc2", "revision": 1, "content_hash": "hash2",
		}},
	})

	meta, err := readESDocumentMeta(ctx, fakeES, "test_index", "")
	if err != nil {
		t.Fatalf("readESDocumentMeta 失败: %v", err)
	}

	if len(meta) != 2 {
		t.Fatalf("期望 2 个文档元信息，实际 %d", len(meta))
	}

	// 验证 doc1 元信息
	doc1Meta, ok := meta["doc1"]
	if !ok {
		t.Fatal("缺少 doc1 元信息")
	}
	if doc1Meta.ChunkCount != 2 {
		t.Errorf("doc1 chunk count = %d, 期望 2", doc1Meta.ChunkCount)
	}
	if doc1Meta.Revision != 3 {
		t.Errorf("doc1 revision = %d, 期望 3", doc1Meta.Revision)
	}
	if doc1Meta.ContentHash != "hash1" {
		t.Errorf("doc1 content_hash = %s, 期望 hash1", doc1Meta.ContentHash)
	}

	// 验证 doc2 元信息
	doc2Meta, ok := meta["doc2"]
	if !ok {
		t.Fatal("缺少 doc2 元信息")
	}
	if doc2Meta.ChunkCount != 1 {
		t.Errorf("doc2 chunk count = %d, 期望 1", doc2Meta.ChunkCount)
	}
	if doc2Meta.Revision != 1 {
		t.Errorf("doc2 revision = %d, 期望 1", doc2Meta.Revision)
	}
}

// TestReadESDocumentMeta_WorkspaceFilter 验证 readESDocumentMeta 的 workspace 过滤。
func TestReadESDocumentMeta_WorkspaceFilter(t *testing.T) {
	ctx := context.Background()
	fakeES := newFakeESClient()
	_ = fakeES.CreateIndex(ctx, "test_index", nil)

	_ = fakeES.BulkIndex(ctx, "test_index", []es.IndexDoc{
		{ID: "doc1_0", Body: map[string]interface{}{
			"document_id": "doc1", "workspace_id": "ws1", "revision": 1, "content_hash": "h1",
		}},
		{ID: "doc2_0", Body: map[string]interface{}{
			"document_id": "doc2", "workspace_id": "ws2", "revision": 1, "content_hash": "h2",
		}},
	})

	// 只查询 ws1
	meta, err := readESDocumentMeta(ctx, fakeES, "test_index", "ws1")
	if err != nil {
		t.Fatalf("readESDocumentMeta 失败: %v", err)
	}

	if len(meta) != 1 {
		t.Fatalf("期望 1 个文档（ws1），实际 %d", len(meta))
	}
	if _, ok := meta["doc1"]; !ok {
		t.Error("应包含 doc1")
	}
	if _, ok := meta["doc2"]; ok {
		t.Error("不应包含 doc2（属于 ws2）")
	}
}

// --- 辅助 fake ES clients ---

// failingBulkESClient 是 BulkIndex 始终返回错误的 fake ES client。
type failingBulkESClient struct {
	*fakeESClient
	bulkIndexError error
}

func (c *failingBulkESClient) BulkIndex(ctx context.Context, indexName string, docs []es.IndexDoc) error {
	c.bulkIndexCalls = append(c.bulkIndexCalls, bulkIndexCall{IndexName: indexName, Docs: docs})
	return c.bulkIndexError
}

// failingDeleteByQueryESClient 是 DeleteByQuery 在第 N 次调用后返回错误的 fake ES client。
type failingDeleteByQueryESClient struct {
	*fakeESClient
	failAfterNCalls      int
	deleteByQueryCallCount int
}

func (c *failingDeleteByQueryESClient) DeleteByQuery(ctx context.Context, indexName string, query map[string]interface{}) error {
	c.deleteByQueryCallCount++
	c.deleteByQueryCalls = append(c.deleteByQueryCalls, deleteByQueryCall{IndexName: indexName, Query: query})
	if c.deleteByQueryCallCount > c.failAfterNCalls {
		return fmt.Errorf("DeleteByQuery 模拟失败（第 %d 次调用）", c.deleteByQueryCallCount)
	}
	return c.fakeESClient.DeleteByQuery(ctx, indexName, query)
}

// --- 辅助函数 ---

// setupTestDocument 在 PG 中创建 user + workspace + document，返回 workspaceID 和 documentID。
// 引入动机：F2 集成测试需要在 PG 中预置文档数据以测试 RepairIssue 的重新索引逻辑。
func setupTestDocument(t *testing.T, db *sql.DB, path, title, content string) (wsID, docID string) {
	t.Helper()
	ctx := context.Background()

	// 创建测试用户（workspaces.created_by 和 documents.created_by 需要 user 引用）
	var userID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO users (username, email, password_hash, system_role) VALUES ($1, $2, $3, 'system_admin') RETURNING id`,
		"test_"+path, "test_"+path+"@example.com", "test_hash",
	).Scan(&userID)
	if err != nil {
		t.Fatalf("创建测试用户: %v", err)
	}

	// 创建测试 workspace
	err = db.QueryRowContext(ctx,
		`INSERT INTO workspaces (name, display_name, max_document_size_bytes, revision_retention_days, revision_max_count, created_by)
		 VALUES ($1, $2, 2097152, 365, 30, $3) RETURNING id`,
		"test_ws_"+path, "Test WS "+path, userID,
	).Scan(&wsID)
	if err != nil {
		t.Fatalf("创建测试 workspace: %v", err)
	}

	// 创建测试文档
	contentHash := computeTestHash(content)
	err = db.QueryRowContext(ctx,
		`INSERT INTO documents (workspace_id, path, title, content_markdown, content_hash, revision_number, is_special, created_by, updated_by)
		 VALUES ($1, $2, $3, $4, $5, 1, FALSE, $6, $6) RETURNING id`,
		wsID, path, title, content, contentHash, userID,
	).Scan(&docID)
	if err != nil {
		t.Fatalf("创建测试文档: %v", err)
	}

	// 创建初始 revision
	_, err = db.ExecContext(ctx,
		`INSERT INTO revisions (document_id, workspace_id, revision_number, path, title, content_markdown, content_hash, status, created_by)
		 VALUES ($1, $2, 1, $3, $4, $5, $6, 'active', $7)`,
		docID, wsID, path, title, content, contentHash, userID,
	)
	if err != nil {
		t.Fatalf("创建测试 revision: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM revisions WHERE workspace_id = $1`, wsID)
		_, _ = db.ExecContext(ctx, `DELETE FROM documents WHERE workspace_id = $1`, wsID)
		_, _ = db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, wsID)
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	return wsID, docID
}

// computeTestHash 计算内容的 SHA-256 摘要（十六进制）。
func computeTestHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h[:])
}
