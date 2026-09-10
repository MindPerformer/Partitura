// repository_integration_test.go 测试 Search Profile repository 的激活/停用/索引名更新
// 在真实 M003 schema 契约下的行为。
//
// 引入动机：Phase6 远程验收发现 ActivateProfile SQL 引用 search_profiles.updated_at 列，
// 但 M003 建表语句中该列不存在，导致 PostgreSQL 42703 错误，profile 激活失败，
// 阻断业务 rebuild_index job。
//
// 修复后 SQL 不再引用 updated_at 列。本测试在真实 M003 schema 下验证：
//   - ActivateProfile 能成功执行（不触发 42703）
//   - 激活后原 active profile 变为 inactive
//   - 激活后目标 profile 状态为 active 且 activated_at 非空
//   - DeactivateProfile 能成功执行
//   - UpdateProfileESIndex 能成功执行
//   - ArchiveProfile 的 SQL 守卫在真实 schema 下生效（active 拒绝、影响 0 行报错）
//
// 测试需要 TEST_DATABASE_URL 环境变量，未设置时自动跳过。
package profile

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"partitura/server/internal/db/migration"
	"partitura/server/internal/es"
)

// profileAdvisoryLockKey 是跨进程 advisory lock 的固定键值。
// 与 testutil 包使用同一个键，确保跨包序列化 DB 访问。
const profileAdvisoryLockKey int64 = 9527

// setupProfileTestDB 返回测试用 PG 连接，无 TEST_DATABASE_URL 时 skip。
// 使用跨进程 advisory lock + 完整 reset 隔离，与 testutil 包模式一致。
func setupProfileTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("跳过集成测试：未设置 TEST_DATABASE_URL 环境变量")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	pingCtx, pingCancel := context.WithCancel(context.Background())
	if err := db.PingContext(pingCtx); err != nil {
		pingCancel()
		_ = db.Close()
		t.Skipf("跳过集成测试：无法连接 PG (%v)", err)
	}
	pingCancel()

	conn, err := db.Conn(t.Context())
	if err != nil {
		_ = db.Close()
		t.Fatalf("获取专用数据库连接失败: %v", err)
	}

	lockCtx := t.Context()
	if _, err := conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1)", profileAdvisoryLockKey); err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatalf("获取 advisory lock 失败: %v", err)
	}

	cleanProfileTestDBConn(lockCtx, conn)

	migrationsDir := findProfileMigrationsDir(t)
	migrations, err := migration.LoadMigrations(migrationsDir)
	if err != nil {
		releaseProfileLockAndCloseConn(conn, db, t)
		t.Fatalf("加载迁移文件失败: %v", err)
	}
	runner := migration.NewRunner(db, migrations)
	if _, err := runner.Up(lockCtx); err != nil {
		releaseProfileLockAndCloseConn(conn, db, t)
		t.Fatalf("执行迁移 Up 失败: %v", err)
	}

	t.Cleanup(func() {
		cleanProfileTestDBConn(context.Background(), conn)
		releaseProfileLockAndCloseConn(conn, db, t)
	})
	return db
}

// releaseProfileLockAndCloseConn 在专用连接上释放 advisory lock，然后关闭连接和连接池。
func releaseProfileLockAndCloseConn(conn *sql.Conn, db *sql.DB, t *testing.T) {
	_, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", profileAdvisoryLockKey)
	if err != nil {
		t.Errorf("释放 advisory lock 失败: %v", err)
	}
	_ = conn.Close()
	_ = db.Close()
}

// cleanProfileTestDBConn 在专用连接上清理测试数据库中的所有表和扩展。
func cleanProfileTestDBConn(ctx context.Context, conn *sql.Conn) {
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
		_, _ = conn.ExecContext(ctx, "DROP TABLE IF EXISTS "+table+" CASCADE")
	}
	_, _ = conn.ExecContext(ctx, `DROP EXTENSION IF EXISTS "pgcrypto"`)
}

// findProfileMigrationsDir 定位项目中的 migrations 目录。
func findProfileMigrationsDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 失败")
	}
	dir := filepath.Dir(filename) // .../server/internal/profile
	dir = filepath.Dir(dir)       // .../server/internal
	dir = filepath.Dir(dir)       // .../server
	migrationsDir := filepath.Join(dir, "migrations")

	info, err := os.Stat(migrationsDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("无法定位 migrations 目录: %s", migrationsDir)
	}
	return migrationsDir
}

// createTestUserAndProfile 在 PG 中创建测试用户和 search profile，返回 profile ID。
// 引入动机：ActivateProfile 测试需要预置 user（外键引用）和 profile 记录。
func createTestUserAndProfile(t *testing.T, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO users (username, email, password_hash, system_role) VALUES ($1, $2, $3, 'system_admin') RETURNING id`,
		"test_profile_user", "test_profile@example.com", "test_hash",
	).Scan(&userID)
	if err != nil {
		t.Fatalf("创建测试用户: %v", err)
	}

	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM search_profiles WHERE created_by = $1`, userID)
		_, _ = db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	repo := NewPGRepository(db)
	profile, err := repo.CreateProfile(ctx, &CreateProfileInput{
		Name:                  "test_profile",
		EmbeddingProvider:     "openai-compatible",
		EmbeddingModel:        "qwen3-embedding",
		EmbeddingDimensions:   1024,
		ChunkTargetSize:       512,
		ChunkOverlap:          64,
		TitleBoost:            2.0,
		HeadingBoost:          1.5,
		PathBoost:             1.0,
		TagsBoost:             0.5,
		BodyBoost:             1.0,
		Analyzer:              "standard",
		LexicalTopK:           50,
		VectorTopK:            50,
		RRFK:                  60,
		RerankerProvider:      "openai-compatible",
		RerankerModel:         "qwen3-reranker",
		RerankerCandidateCount: 20,
		RerankerFinalCount:     10,
		MaxChunksPerDocument:  3,
		MergeAdjacentChunks:   true,
		MaxP95LatencyMs:       2000,
		MaxRerankerCostPerQuery: 0.01,
		CreatedBy:             userID,
	})
	if err != nil {
		t.Fatalf("创建测试 profile: %v", err)
	}
	return profile.ID
}

// TestActivateProfile_NoUpdatedAtColumn 验证 ActivateProfile 在真实 M003 schema 下成功执行。
// 引入动机：Phase6 远程验收发现 ActivateProfile SQL 引用 search_profiles.updated_at 列，
// 但 M003 建表语句中该列不存在，导致 PostgreSQL 42703 错误。
// 修复后 SQL 不再引用 updated_at，本测试验证激活操作在真实 schema 下成功。
func TestActivateProfile_NoUpdatedAtColumn(t *testing.T) {
	db := setupProfileTestDB(t)
	ctx := context.Background()
	repo := NewPGRepository(db)

	profileID := createTestUserAndProfile(t, db)

	// 执行激活——此前会因 updated_at 列不存在而失败
	err := repo.ActivateProfile(ctx, profileID)
	if err != nil {
		t.Fatalf("ActivateProfile 应在真实 schema 下成功，实际失败: %v", err)
	}

	// 验证 profile 状态为 active
	profile, err := repo.GetProfileByID(ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfileByID 失败: %v", err)
	}
	if profile.Status != "active" {
		t.Errorf("激活后 status 应为 active，实际为 %s", profile.Status)
	}
	if profile.ActivatedAt == "" {
		t.Error("激活后 activated_at 不应为空")
	}
}

// TestActivateProfile_SwitchesActive 验证激活新 profile 时原 active profile 变为 inactive。
// 引入动机：design/01-SEARCH.md §Index Version 要求 alias 原子切换，
// 激活新 profile 时原 active 必须变为 inactive。
func TestActivateProfile_SwitchesActive(t *testing.T) {
	db := setupProfileTestDB(t)
	ctx := context.Background()
	repo := NewPGRepository(db)

	// 创建并激活第一个 profile
	profileID1 := createTestUserAndProfile(t, db)
	if err := repo.ActivateProfile(ctx, profileID1); err != nil {
		t.Fatalf("激活第一个 profile 失败: %v", err)
	}

	// 创建第二个 profile
	var userID string
	err := db.QueryRowContext(ctx,
		`SELECT created_by FROM search_profiles WHERE id = $1`, profileID1,
	).Scan(&userID)
	if err != nil {
		t.Fatalf("查询 user_id 失败: %v", err)
	}

	profile2, err := repo.CreateProfile(ctx, &CreateProfileInput{
		Name:                  "test_profile",
		EmbeddingProvider:     "openai-compatible",
		EmbeddingModel:        "qwen3-embedding",
		EmbeddingDimensions:   1024,
		ChunkTargetSize:       512,
		ChunkOverlap:          64,
		TitleBoost:            2.0,
		HeadingBoost:          1.5,
		PathBoost:             1.0,
		TagsBoost:             0.5,
		BodyBoost:             1.0,
		Analyzer:              "standard",
		LexicalTopK:           50,
		VectorTopK:             50,
		RRFK:                  60,
		RerankerProvider:      "openai-compatible",
		RerankerModel:         "qwen3-reranker",
		RerankerCandidateCount: 20,
		RerankerFinalCount:     10,
		MaxChunksPerDocument:  3,
		MergeAdjacentChunks:   true,
		MaxP95LatencyMs:       2000,
		MaxRerankerCostPerQuery: 0.01,
		CreatedBy:             userID,
	})
	if err != nil {
		t.Fatalf("创建第二个 profile 失败: %v", err)
	}

	// 激活第二个 profile
	if err := repo.ActivateProfile(ctx, profile2.ID); err != nil {
		t.Fatalf("激活第二个 profile 失败: %v", err)
	}

	// 验证第一个 profile 变为 inactive
	p1, err := repo.GetProfileByID(ctx, profileID1)
	if err != nil {
		t.Fatalf("查询第一个 profile 失败: %v", err)
	}
	if p1.Status != "inactive" {
		t.Errorf("原 active profile 应变为 inactive，实际为 %s", p1.Status)
	}

	// 验证第二个 profile 为 active
	p2, err := repo.GetProfileByID(ctx, profile2.ID)
	if err != nil {
		t.Fatalf("查询第二个 profile 失败: %v", err)
	}
	if p2.Status != "active" {
		t.Errorf("新 profile 应为 active，实际为 %s", p2.Status)
	}
}

// TestDeactivateProfile_NoUpdatedAtColumn 验证 DeactivateProfile 在真实 M003 schema 下成功执行。
func TestDeactivateProfile_NoUpdatedAtColumn(t *testing.T) {
	db := setupProfileTestDB(t)
	ctx := context.Background()
	repo := NewPGRepository(db)

	profileID := createTestUserAndProfile(t, db)

	// 先激活
	if err := repo.ActivateProfile(ctx, profileID); err != nil {
		t.Fatalf("激活 profile 失败: %v", err)
	}

	// 停用——此前会因 updated_at 列不存在而失败
	err := repo.DeactivateProfile(ctx, profileID)
	if err != nil {
		t.Fatalf("DeactivateProfile 应在真实 schema 下成功，实际失败: %v", err)
	}

	// 验证状态为 inactive
	profile, err := repo.GetProfileByID(ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfileByID 失败: %v", err)
	}
	if profile.Status != "inactive" {
		t.Errorf("停用后 status 应为 inactive，实际为 %s", profile.Status)
	}
}

// TestUpdateProfileESIndex_NoUpdatedAtColumn 验证 UpdateProfileESIndex 在真实 M003 schema 下成功执行。
func TestUpdateProfileESIndex_NoUpdatedAtColumn(t *testing.T) {
	db := setupProfileTestDB(t)
	ctx := context.Background()
	repo := NewPGRepository(db)

	profileID := createTestUserAndProfile(t, db)

	// 更新 ES 索引名——此前会因 updated_at 列不存在而失败
	err := repo.UpdateProfileESIndex(ctx, profileID, "knowledge_v2")
	if err != nil {
		t.Fatalf("UpdateProfileESIndex 应在真实 schema 下成功，实际失败: %v", err)
	}

	// 验证索引名已更新
	profile, err := repo.GetProfileByID(ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfileByID 失败: %v", err)
	}
	if profile.ESIndexName != "knowledge_v2" {
		t.Errorf("ES 索引名应为 knowledge_v2，实际为 %s", profile.ESIndexName)
	}
}

// TestGetActiveProfile_AfterActivate 验证激活后 GetActiveProfile 返回正确的 profile。
// 引入动机：EnsureDefaultProfile 依赖 GetActiveProfile 和 ActivateProfile 的正确协作。
func TestGetActiveProfile_AfterActivate(t *testing.T) {
	db := setupProfileTestDB(t)
	ctx := context.Background()
	repo := NewPGRepository(db)

	profileID := createTestUserAndProfile(t, db)

	// 激活前 GetActiveProfile 应返回 sql.ErrNoRows
	_, err := repo.GetActiveProfile(ctx)
	if err == nil {
		t.Fatal("激活前不应有 active profile")
	}

	// 激活
	if err := repo.ActivateProfile(ctx, profileID); err != nil {
		t.Fatalf("激活 profile 失败: %v", err)
	}

	// 激活后 GetActiveProfile 应返回该 profile
	active, err := repo.GetActiveProfile(ctx)
	if err != nil {
		t.Fatalf("GetActiveProfile 失败: %v", err)
	}
	if active.ID != profileID {
		t.Errorf("active profile ID 应为 %s，实际为 %s", profileID, active.ID)
	}
}

// TestCreateProfile_ServerSideDraftVersionAndIndex 验证 CreateProfile 的服务端语义：
// 版本号按同名 profile 递增、es_index_name 按版本号生成、status 固定为 draft、activated_at 为空。
//
// 引入动机："新建版本"端点直接复用 CreateProfile，其契约（草稿/递增版本/服务端索引名）
// 由仓储层承担；handler 层测试只能观察传入参数，必须在真实 M003 schema 下验证落库结果，
// 否则"新版本一定不会被自动激活"这一关键约束就没有端到端证据。
func TestCreateProfile_ServerSideDraftVersionAndIndex(t *testing.T) {
	db := setupProfileTestDB(t)
	ctx := context.Background()
	repo := NewPGRepository(db)

	firstID := createTestUserAndProfile(t, db)
	first, err := repo.GetProfileByID(ctx, firstID)
	if err != nil {
		t.Fatalf("查询首个 profile 失败: %v", err)
	}
	if first.Version != 1 {
		t.Errorf("首个版本 version 应为 1，实际 %d", first.Version)
	}
	if first.Status != "draft" {
		t.Errorf("CreateProfile 应落库为 draft，实际 %s", first.Status)
	}
	if first.ActivatedAt != "" {
		t.Errorf("新建 profile 不应有 activated_at，实际 %q", first.ActivatedAt)
	}
	if first.ESIndexName != es.GenerateIndexName(1) {
		t.Errorf("es_index_name 应为 %s，实际 %s", es.GenerateIndexName(1), first.ESIndexName)
	}

	// 以同名创建第二个版本，验证版本递增与索引名随之变化
	second, err := repo.CreateProfile(ctx, &CreateProfileInput{
		Name:                    "test_profile",
		EmbeddingProvider:       "openai-compatible",
		EmbeddingModel:          "qwen3-embedding",
		EmbeddingDimensions:     1024,
		ChunkTargetSize:         512,
		ChunkOverlap:            64,
		Analyzer:                "standard",
		LexicalTopK:             50,
		VectorTopK:              50,
		RRFK:                    60,
		RerankerProvider:        "openai-compatible",
		RerankerModel:           "qwen3-reranker",
		RerankerCandidateCount:  20,
		RerankerFinalCount:      10,
		MaxChunksPerDocument:    3,
		MergeAdjacentChunks:     true,
		MaxP95LatencyMs:         2000,
		MaxRerankerCostPerQuery: 0.01,
		CreatedBy:               first.CreatedBy,
	})
	if err != nil {
		t.Fatalf("创建第二个版本失败: %v", err)
	}
	if second.Version != 2 {
		t.Errorf("第二个版本 version 应为 2，实际 %d", second.Version)
	}
	if second.Status != "draft" {
		t.Errorf("第二个版本应为 draft，实际 %s", second.Status)
	}
	if second.ESIndexName != es.GenerateIndexName(2) {
		t.Errorf("第二个版本 es_index_name 应为 %s，实际 %s", es.GenerateIndexName(2), second.ESIndexName)
	}
}

// TestArchiveProfile_GuardAndRetention 验证归档守卫与记录保留在真实 schema 下的行为。
//
// 引入动机：ArchiveProfile 的 UPDATE 带 status <> 'active' 守卫，影响 0 行时必须返回错误
// 而不是静默成功（并发状态变化或 id 不存在）。fake 不执行 SQL，无法覆盖该语义，
// 因此必须在真实 M003 schema 下验证：draft/inactive 可归档、active 被拒、记录保留、id 不存在报错。
func TestArchiveProfile_GuardAndRetention(t *testing.T) {
	db := setupProfileTestDB(t)
	ctx := context.Background()
	repo := NewPGRepository(db)

	profileID := createTestUserAndProfile(t, db)

	// draft → archived 成功，且记录保留
	if err := repo.ArchiveProfile(ctx, profileID); err != nil {
		t.Fatalf("归档 draft profile 应成功，实际失败: %v", err)
	}
	p, err := repo.GetProfileByID(ctx, profileID)
	if err != nil {
		t.Fatalf("归档后记录应保留，实际查询失败: %v", err)
	}
	if p.Status != "archived" {
		t.Errorf("归档后 status 应为 archived，实际 %s", p.Status)
	}

	// 归档是可逆的：archived 可被重新激活（既有 rollback 语义）
	if err := repo.ActivateProfile(ctx, profileID); err != nil {
		t.Fatalf("激活 archived profile 失败: %v", err)
	}

	// active → 被守卫拒绝，且状态不变
	if err := repo.ArchiveProfile(ctx, profileID); !errors.Is(err, ErrProfileNotArchivable) {
		t.Fatalf("active profile 归档应返回 ErrProfileNotArchivable，实际 %v", err)
	}
	p, err = repo.GetProfileByID(ctx, profileID)
	if err != nil {
		t.Fatalf("查询 profile 失败: %v", err)
	}
	if p.Status != "active" {
		t.Errorf("被拒绝的归档不应改变状态，实际 %s", p.Status)
	}

	// inactive → archived 成功
	if err := repo.DeactivateProfile(ctx, profileID); err != nil {
		t.Fatalf("停用 profile 失败: %v", err)
	}
	if err := repo.ArchiveProfile(ctx, profileID); err != nil {
		t.Fatalf("归档 inactive profile 应成功，实际失败: %v", err)
	}
	p, err = repo.GetProfileByID(ctx, profileID)
	if err != nil {
		t.Fatalf("查询 profile 失败: %v", err)
	}
	if p.Status != "archived" {
		t.Errorf("归档 inactive profile 后 status 应为 archived，实际 %s", p.Status)
	}

	// id 不存在 → 影响 0 行 → 必须报错而不是静默成功
	if err := repo.ArchiveProfile(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrProfileNotArchivable) {
		t.Fatalf("不存在 id 归档应返回 ErrProfileNotArchivable，实际 %v", err)
	}
}
