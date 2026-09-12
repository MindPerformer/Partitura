// Package migration — 迁移框架测试。
//
// 测试覆盖：
//   - 文件名解析正确性
//   - 迁移文件加载与排序
//   - 集成测试：实际运行迁移到 PostgreSQL 并验证表结构（需要 TEST_DATABASE_URL）
//   - 集成测试：回退验证
//   - 集成测试：失败停止验证
//   - 集成测试：幂等性验证
package migration

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestParseMigrationName(t *testing.T) {
	tests := []struct {
		filename  string
		version   int
		name      string
		direction string
		ok        bool
	}{
		{"M001_phase1_foundation_up.sql", 1, "phase1_foundation", "up", true},
		{"M001_phase1_foundation_down.sql", 1, "phase1_foundation", "down", true},
		{"M002_documents_up.sql", 2, "documents", "up", true},
		{"M010_test_migration_down.sql", 10, "test_migration", "down", true},
		{"M001_up.sql", 1, "unnamed", "up", true},
		{"invalid.sql", 0, "", "", false},
		{"M001_foundation.sql", 0, "", "", false},
		{"Mabc_foundation_up.sql", 0, "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			v, n, d, ok := ParseMigrationName(tt.filename)
			if ok != tt.ok {
				t.Fatalf("ok = %v, 期望 %v", ok, tt.ok)
			}
			if ok {
				if v != tt.version {
					t.Errorf("version = %d, 期望 %d", v, tt.version)
				}
				if n != tt.name {
					t.Errorf("name = %q, 期望 %q", n, tt.name)
				}
				if d != tt.direction {
					t.Errorf("direction = %q, 期望 %q", d, tt.direction)
				}
			}
		})
	}
}

func TestLoadMigrations_FromProjectDir(t *testing.T) {
	// 定位项目中的 migrations 目录
	migrationsDir := findProjectMigrationsDir(t)

	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("LoadMigrations 失败: %v", err)
	}

	if len(migrations) == 0 {
		t.Fatal("期望至少加载一个迁移")
	}

	// 验证按版本升序
	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version <= migrations[i-1].Version {
			t.Errorf("迁移未按版本升序: index %d version %d <= index %d version %d",
				i, migrations[i].Version, i-1, migrations[i-1].Version)
		}
	}

	// 验证每个迁移都有 up 和 down SQL
	for _, m := range migrations {
		if m.UpSQL == "" {
			t.Errorf("迁移 v%d (%s) 缺少 UpSQL", m.Version, m.Name)
		}
		if m.DownSQL == "" {
			t.Errorf("迁移 v%d (%s) 缺少 DownSQL", m.Version, m.Name)
		}
	}
}

// findProjectMigrationsDir 定位项目中的 migrations 目录。
func findProjectMigrationsDir(t *testing.T) string {
	t.Helper()
	// 从当前测试工作目录向上查找 migrations 目录
	candidates := []string{
		"../../../migrations",
		"../../../../migrations",
		"../../../../../migrations",
		"../../../../../../migrations",
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			return abs
		}
	}
	t.Skip("未找到项目 migrations 目录")
	return ""
}

// --- 以下为集成测试，需要 PostgreSQL 实例 ---
// 通过环境变量 TEST_DATABASE_URL 提供 PostgreSQL 连接字符串。
// 格式：postgres://user:password@host:port/dbname?sslmode=disable
// 或：host=localhost port=5432 user=test password=test dbname=testdb sslmode=disable
// 可用 Docker 启动临时 PostgreSQL：
//   docker run --rm -d -p 5432:5432 -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=testdb --name pg-test postgres:16

// testDB 返回测试用数据库连接，如果未设置 TEST_DATABASE_URL 则跳过测试。
// 使用跨进程 advisory lock + 完整 reset 隔离，不依赖测试执行顺序。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("跳过集成测试：未设置 TEST_DATABASE_URL 环境变量。" +
			"要运行集成测试，请提供一个可用的 PostgreSQL 连接字符串，例如：" +
			"export TEST_DATABASE_URL=\"host=localhost port=5432 user=test password=test dbname=testdb sslmode=disable\"")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开测试数据库连接: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Skipf("跳过集成测试：无法连接到测试数据库 (%v)。请确认 PostgreSQL 实例可用。", err)
	}

	// 获取跨进程 advisory lock，序列化 DB 访问
	if _, err := db.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		_ = db.Close()
		t.Fatalf("获取 advisory lock 失败: %v", err)
	}

	// 完整 reset：清理可能残留的表和扩展
	cleanTestDB(ctx, db)

	t.Cleanup(func() {
		// 完整 reset 后释放锁
		cleanTestDB(context.Background(), db)
		_, _ = db.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		_ = db.Close()
	})

	return db
}

// advisoryLockKey 是跨进程 advisory lock 的固定键值。
// 与 testutil 包使用同一个键，确保跨包序列化 DB 访问。
const advisoryLockKey int64 = 9527

// cleanTestDB 清理测试数据库中的所有表和扩展。
func cleanTestDB(ctx context.Context, db *sql.DB) {
	// 按依赖逆序删除
	// device_authorizations 引用 users(id)，必须在 users 之前删除
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
		_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table))
	}
	// 删除扩展（pg_trgm 由 M011 引入，与 pgcrypto 一并清理保证干净起始状态）
	_, _ = db.ExecContext(ctx, `DROP EXTENSION IF EXISTS "pgcrypto"`)
	_, _ = db.ExecContext(ctx, `DROP EXTENSION IF EXISTS pg_trgm`)
}

func TestIntegration_MigrationUp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)

	// 执行 Up
	executed, err := runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}
	if executed == 0 {
		t.Fatal("期望至少执行 1 个迁移")
	}

	// 验证 schema_migrations 表有记录
	var count int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count)
	if err != nil {
		t.Fatalf("查询 schema_migrations: %v", err)
	}
	if count == 0 {
		t.Error("schema_migrations 表中应有记录")
	}

	// 验证 6 张表存在
	expectedTables := []string{"users", "sessions", "device_sessions", "workspaces", "workspace_members", "audit_logs"}
	for _, table := range expectedTables {
		var exists bool
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)", table).Scan(&exists)
		if err != nil {
			t.Fatalf("查询表 %s 是否存在: %v", table, err)
		}
		if !exists {
			t.Errorf("表 %s 不存在", table)
		}
	}
}

func TestIntegration_MigrationIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)

	// 第一次 Up
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("第一次迁移 Up 失败: %v", err)
	}

	// 第二次 Up — 应该是幂等的，不执行任何迁移
	executed, err := runner.Up(ctx)
	if err != nil {
		t.Fatalf("第二次迁移 Up 失败: %v", err)
	}
	if executed != 0 {
		t.Errorf("第二次 Up 应执行 0 个迁移，实际执行 %d 个", executed)
	}
}

func TestIntegration_MigrationDown(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)

	// 先 Up
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 计算当前最高迁移版本与其下一个最高版本，使断言不随新增迁移而过期。
	maxVersion := 0
	secondMax := 0
	for _, m := range migrations {
		if m.Version > maxVersion {
			secondMax = maxVersion
			maxVersion = m.Version
		} else if m.Version > secondMax {
			secondMax = m.Version
		}
	}

	// 回退 1 步——应回退最高版本
	reverted, err := runner.Down(ctx, 1)
	if err != nil {
		t.Fatalf("迁移 Down 失败: %v", err)
	}
	if reverted != 1 {
		t.Errorf("期望回退 1 个迁移，实际回退 %d 个", reverted)
	}

	// 验证 users 表仍存在（Down(1) 只回退最高 migration，不影响 M001）
	var exists bool
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'users')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询 users 表是否存在: %v", err)
	}
	if !exists {
		t.Error("回退最高版本后 users 表应仍存在（Down(1) 只回退最高 migration）")
	}

	// 验证最高版本已从 schema_migrations 删除，当前版本回落到第二高
	version, err := runner.CurrentVersion(ctx)
	if err != nil {
		t.Fatalf("查询当前版本: %v", err)
	}
	if version != secondMax {
		t.Errorf("回退最高版本（M%03d）后当前版本应为 %d（下一个最高），实际 %d", maxVersion, secondMax, version)
	}
}

func TestIntegration_MigrationUpThenDownThenUp(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)

	// Up → Down → Up 验证可重复执行
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("第一次 Up 失败: %v", err)
	}

	_, err = runner.Down(ctx, 1)
	if err != nil {
		t.Fatalf("Down 失败: %v", err)
	}

	executed, err := runner.Up(ctx)
	if err != nil {
		t.Fatalf("第二次 Up 失败: %v", err)
	}
	if executed != 1 {
		t.Errorf("第二次 Up 应执行 1 个迁移，实际 %d", executed)
	}
}

func TestIntegration_MigrationFailureStops(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// 构造一个会失败的迁移（引用不存在的表）
	migrations := []Migration{
		{
			Version: 1,
			Name:    "ok_migration",
			UpSQL:   "CREATE TABLE IF NOT EXISTS test_ok (id INT);",
			DownSQL: "DROP TABLE IF EXISTS test_ok;",
		},
		{
			Version: 2,
			Name:    "bad_migration",
			UpSQL:   "INSERT INTO nonexistent_table VALUES (1);",
			DownSQL: "DROP TABLE IF EXISTS nonexistent_table;",
		},
		{
			Version: 3,
			Name:    "should_not_run",
			UpSQL:   "CREATE TABLE IF NOT EXISTS test_should_not_exist (id INT);",
			DownSQL: "DROP TABLE IF EXISTS test_should_not_exist;",
		},
	}

	runner := NewRunner(db, migrations)

	executed, err := runner.Up(ctx)
	if err == nil {
		t.Fatal("期望迁移失败返回错误")
	}
	if executed != 1 {
		t.Errorf("期望执行 1 个成功迁移后失败，实际执行 %d 个", executed)
	}

	// 验证 v3 的表不存在（因为 v2 失败应停止）
	var exists bool
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'test_should_not_exist')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询表是否存在: %v", err)
	}
	if exists {
		t.Error("v3 的表不应存在——迁移在 v2 失败时应停止")
	}

	// 验证 v1 的表存在
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'test_ok')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询表是否存在: %v", err)
	}
	if !exists {
		t.Error("v1 的表应存在——v1 成功执行后才到 v2")
	}

	// 清理
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS test_ok")
	_, _ = db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version IN (1, 2, 3)")
}

func TestIntegration_MigrationVersionOrder(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// 故意打乱顺序传入
	migrations := []Migration{
		{
			Version: 3,
			Name:    "third",
			UpSQL:   "CREATE TABLE IF NOT EXISTS test_order_3 (id INT);",
			DownSQL: "DROP TABLE IF EXISTS test_order_3;",
		},
		{
			Version: 1,
			Name:    "first",
			UpSQL:   "CREATE TABLE IF NOT EXISTS test_order_1 (id INT);",
			DownSQL: "DROP TABLE IF EXISTS test_order_1;",
		},
		{
			Version: 2,
			Name:    "second",
			UpSQL:   "CREATE TABLE IF NOT EXISTS test_order_2 (id INT);",
			DownSQL: "DROP TABLE IF EXISTS test_order_2;",
		},
	}

	runner := NewRunner(db, migrations)

	_, err := runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 验证所有表都存在
	for _, table := range []string{"test_order_1", "test_order_2", "test_order_3"} {
		var exists bool
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = $1)", table).Scan(&exists)
		if err != nil {
			t.Fatalf("查询表 %s: %v", table, err)
		}
		if !exists {
			t.Errorf("表 %s 应存在", table)
		}
	}

	// 清理
	for _, table := range []string{"test_order_1", "test_order_2", "test_order_3"} {
		_, _ = db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
	}
	_, _ = db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version IN (1, 2, 3)")
}

func TestIntegration_M001SchemaConstraints(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 验证 users.system_role CHECK 约束
	_, err = db.ExecContext(ctx, "INSERT INTO users (username, email, password_hash, system_role) VALUES ('testuser', 'test@test.com', 'hash', 'invalid_role')")
	if err == nil {
		t.Error("应拒绝非法 system_role 值")
	}
	_, _ = db.ExecContext(ctx, "DELETE FROM users WHERE username = 'testuser'")

	// 验证 workspaces.status CHECK 约束
	// 先创建一个合法 user 作为 created_by
	_, err = db.ExecContext(ctx, "INSERT INTO users (username, email, password_hash) VALUES ('ws_creator', 'ws@test.com', 'hash')")
	if err != nil {
		t.Fatalf("创建测试用户失败: %v", err)
	}
	var userID string
	err = db.QueryRowContext(ctx, "SELECT id FROM users WHERE username = 'ws_creator'").Scan(&userID)
	if err != nil {
		t.Fatalf("查询测试用户 ID: %v", err)
	}

	_, err = db.ExecContext(ctx, "INSERT INTO workspaces (name, display_name, status, created_by) VALUES ('testws', 'Test WS', 'invalid_status', $1)", userID)
	if err == nil {
		t.Error("应拒绝非法 workspaces.status 值")
	}

	// 验证 workspace_members.role CHECK 约束
	_, err = db.ExecContext(ctx, "INSERT INTO workspaces (name, display_name, created_by) VALUES ('testws_ok', 'Test WS OK', $1)", userID)
	if err != nil {
		t.Fatalf("创建测试 workspace 失败: %v", err)
	}
	var wsID string
	err = db.QueryRowContext(ctx, "SELECT id FROM workspaces WHERE name = 'testws_ok'").Scan(&wsID)
	if err != nil {
		t.Fatalf("查询测试 workspace ID: %v", err)
	}

	_, err = db.ExecContext(ctx, "INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'super_admin')", wsID, userID)
	if err == nil {
		t.Error("应拒绝非法 workspace_members.role 值")
	}

	// 验证 workspace_members UNIQUE(workspace_id, user_id) 约束
	_, err = db.ExecContext(ctx, "INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')", wsID, userID)
	if err != nil {
		t.Fatalf("插入第一个 workspace_member 失败: %v", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'admin')", wsID, userID)
	if err == nil {
		t.Error("应拒绝重复的 workspace_member (UNIQUE 约束)")
	}

	// 验证 users.username UNIQUE 约束
	_, err = db.ExecContext(ctx, "INSERT INTO users (username, email, password_hash) VALUES ('ws_creator', 'another@test.com', 'hash')")
	if err == nil {
		t.Error("应拒绝重复 username (UNIQUE 约束)")
	}

	// 验证 users.email UNIQUE 约束
	_, err = db.ExecContext(ctx, "INSERT INTO users (username, email, password_hash) VALUES ('another_user', 'ws@test.com', 'hash')")
	if err == nil {
		t.Error("应拒绝重复 email (UNIQUE 约束)")
	}
}

func TestIntegration_M001SchemaIndexes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 验证关键索引存在
	expectedIndexes := []string{
		"idx_sessions_token_hash",
		"idx_sessions_user_id",
		"idx_device_sessions_access_hash",
		"idx_device_sessions_refresh_hash",
		"idx_wm_workspace_id",
		"idx_wm_user_id",
		"idx_audit_workspace_created",
		"idx_audit_user_created",
	}

	for _, idxName := range expectedIndexes {
		var exists bool
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT FROM pg_indexes WHERE indexname = $1)", idxName).Scan(&exists)
		if err != nil {
			t.Fatalf("查询索引 %s: %v", idxName, err)
		}
		if !exists {
			t.Errorf("索引 %s 不存在", idxName)
		}
	}
}

func TestIntegration_M001SchemaForeignKeys(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 验证外键关系存在
	// sessions.user_id → users.id
	// device_sessions.user_id → users.id
	// workspaces.created_by → users.id
	// workspace_members.workspace_id → workspaces.id
	// workspace_members.user_id → users.id
	expectedFKs := []struct {
		tableName     string
		columnName    string
		refTableName  string
		refColumnName string
	}{
		{"sessions", "user_id", "users", "id"},
		{"device_sessions", "user_id", "users", "id"},
		{"workspaces", "created_by", "users", "id"},
		{"workspace_members", "workspace_id", "workspaces", "id"},
		{"workspace_members", "user_id", "users", "id"},
	}

	for _, fk := range expectedFKs {
		var exists bool
		query := `SELECT EXISTS (
			SELECT FROM information_schema.referential_constraints rc
			JOIN information_schema.key_column_usage kcu ON rc.constraint_name = kcu.constraint_name
			JOIN information_schema.constraint_column_usage ccu ON rc.constraint_name = ccu.constraint_name
			WHERE kcu.table_name = $1 AND kcu.column_name = $2
			AND ccu.table_name = $3 AND ccu.column_name = $4
		)`
		err = db.QueryRowContext(ctx, query, fk.tableName, fk.columnName, fk.refTableName, fk.refColumnName).Scan(&exists)
		if err != nil {
			t.Fatalf("查询外键 %s.%s → %s.%s: %v", fk.tableName, fk.columnName, fk.refTableName, fk.refColumnName, err)
		}
		if !exists {
			t.Errorf("外键 %s.%s → %s.%s 不存在", fk.tableName, fk.columnName, fk.refTableName, fk.refColumnName)
		}
	}
}

func TestIntegration_M001SchemaTokenHashFields(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 验证 sessions 表有 token_hash 和 csrf_token_hash 字段
	for _, col := range []string{"token_hash", "csrf_token_hash"} {
		var exists bool
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.columns WHERE table_name = 'sessions' AND column_name = $1)", col).Scan(&exists)
		if err != nil {
			t.Fatalf("查询 sessions.%s: %v", col, err)
		}
		if !exists {
			t.Errorf("sessions.%s 字段不存在", col)
		}
	}

	// 验证 device_sessions 表有 access_token_hash 和 refresh_token_hash 字段
	for _, col := range []string{"access_token_hash", "refresh_token_hash"} {
		var exists bool
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.columns WHERE table_name = 'device_sessions' AND column_name = $1)", col).Scan(&exists)
		if err != nil {
			t.Fatalf("查询 device_sessions.%s: %v", col, err)
		}
		if !exists {
			t.Errorf("device_sessions.%s 字段不存在", col)
		}
	}

	// 验证 users 表有 password_hash 字段
	var exists bool
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'password_hash')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询 users.password_hash: %v", err)
	}
	if !exists {
		t.Error("users.password_hash 字段不存在")
	}
}

func TestIntegration_M001WorkspaceRetentionFields(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 验证 workspaces 表有 revision retention 和 document size 配置字段
	expectedCols := map[string]string{
		"revision_retention_days": "int",
		"revision_max_count":      "int",
		"max_document_size_bytes": "int",
	}

	for col, expectedType := range expectedCols {
		var dataType string
		err = db.QueryRowContext(ctx,
			"SELECT data_type FROM information_schema.columns WHERE table_name = 'workspaces' AND column_name = $1", col).Scan(&dataType)
		if err != nil {
			t.Fatalf("查询 workspaces.%s: %v", col, err)
		}
		if !strings.Contains(strings.ToLower(dataType), expectedType) {
			t.Errorf("workspaces.%s 类型为 %s, 期望包含 %s", col, dataType, expectedType)
		}
	}

	// 验证默认值
	var retentionDays, maxCount, maxDocSize int
	err = db.QueryRowContext(ctx,
		"SELECT revision_retention_days, revision_max_count, max_document_size_bytes FROM workspaces LIMIT 1").Scan(&retentionDays, &maxCount, &maxDocSize)
	// 表可能为空，用另一种方式验证默认值
	if err == sql.ErrNoRows {
		// 通过创建一个 workspace 验证默认值
		_, err = db.ExecContext(ctx, "INSERT INTO users (username, email, password_hash) VALUES ('retention_test', 'ret@test.com', 'hash')")
		if err != nil {
			t.Fatalf("创建测试用户: %v", err)
		}
		var uid string
		err = db.QueryRowContext(ctx, "SELECT id FROM users WHERE username = 'retention_test'").Scan(&uid)
		if err != nil {
			t.Fatalf("查询用户 ID: %v", err)
		}
		_, err = db.ExecContext(ctx, "INSERT INTO workspaces (name, display_name, created_by) VALUES ('retention_ws', 'Retention WS', $1)", uid)
		if err != nil {
			t.Fatalf("创建测试 workspace: %v", err)
		}
		err = db.QueryRowContext(ctx,
			"SELECT revision_retention_days, revision_max_count, max_document_size_bytes FROM workspaces WHERE name = 'retention_ws'").Scan(&retentionDays, &maxCount, &maxDocSize)
	}
	if err != nil {
		t.Fatalf("查询 retention 字段: %v", err)
	}

	if retentionDays != 7 {
		t.Errorf("revision_retention_days 默认值 = %d, 期望 7", retentionDays)
	}
	if maxCount != 30 {
		t.Errorf("revision_max_count 默认值 = %d, 期望 30", maxCount)
	}
	if maxDocSize != 2097152 {
		t.Errorf("max_document_size_bytes 默认值 = %d, 期望 2097152", maxDocSize)
	}
}

func TestLoadMigrations_SortedByVersion(t *testing.T) {
	// 创建临时目录测试文件加载排序
	tmpDir := t.TempDir()

	// 故意以乱序写入文件
	files := map[string]string{
		"M003_third_up.sql":    "CREATE TABLE t3();",
		"M003_third_down.sql":  "DROP TABLE t3;",
		"M001_first_up.sql":    "CREATE TABLE t1();",
		"M001_first_down.sql":  "DROP TABLE t1;",
		"M002_second_up.sql":   "CREATE TABLE t2();",
		"M002_second_down.sql": "DROP TABLE t2;",
	}

	for name, content := range files {
		path := filepath.Join(tmpDir, name)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("写入临时文件 %s: %v", name, err)
		}
	}

	migrations, err := LoadMigrations(tmpDir)
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}

	if len(migrations) != 3 {
		t.Fatalf("期望 3 个迁移，实际 %d", len(migrations))
	}

	expectedVersions := []int{1, 2, 3}
	for i, m := range migrations {
		if m.Version != expectedVersions[i] {
			t.Errorf("migrations[%d].Version = %d, 期望 %d", i, m.Version, expectedVersions[i])
		}
	}

	// 验证排序
	if !sort.SliceIsSorted(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	}) {
		t.Error("迁移未按版本升序排列")
	}
}

// TestLoadMigrations_ProjectDir_NoM005 验证项目迁移目录中不存在 M005 文件。
// 引入动机：F1 修复——M005 保留给 Phase 6 system_settings，evaluation_results 已并入 M004。
// Phase 4 新增 M006 device_authorizations，允许版本 6 存在。
// Phase 6 新增 M007 scheduler_runs，允许版本 7 存在；M008 device_auth_completed；M009 settings；M010 provider_secrets；M011 pg_trgm_fuzzy_indexes；M012 job/document 查询索引；M013 tuning overrides。
func TestLoadMigrations_ProjectDir_NoM005(t *testing.T) {
	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("LoadMigrations 失败: %v", err)
	}

	for _, m := range migrations {
		if m.Version == 5 {
			t.Fatalf("M005 不应存在——它保留给 Phase 6 system_settings，evaluation_results 已并入 M004")
		}
	}

	// 验证最高版本为 M013（tuning overrides）
	maxVersion := 0
	for _, m := range migrations {
		if m.Version > maxVersion {
			maxVersion = m.Version
		}
	}
	if maxVersion != 13 {
		t.Errorf("最高迁移版本应为 13（M013），实际 %d", maxVersion)
	}
}

// TestIntegration_M004EvaluationResultsTable 验证 M004 迁移创建了 evaluation_results 表。
// 引入动机：F1 修复——evaluation_results 从 M005 并入 M004，需验证迁移后表存在且结构正确。
func TestIntegration_M004EvaluationResultsTable(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 验证 evaluation_results 表存在
	var exists bool
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'evaluation_results')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询 evaluation_results 表: %v", err)
	}
	if !exists {
		t.Error("evaluation_results 表应存在（M004 创建）")
	}

	// 验证 evaluation_results 表的关键列
	expectedCols := []string{"id", "dataset_id", "profile_id", "job_id", "metrics", "item_count", "created_at"}
	for _, col := range expectedCols {
		var colExists bool
		err = db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT FROM information_schema.columns WHERE table_name = 'evaluation_results' AND column_name = $1)", col).Scan(&colExists)
		if err != nil {
			t.Fatalf("查询 evaluation_results.%s: %v", col, err)
		}
		if !colExists {
			t.Errorf("evaluation_results.%s 列不存在", col)
		}
	}

	// 验证 evaluation_results 的外键引用 evaluation_datasets
	var fkExists bool
	err = db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT FROM information_schema.referential_constraints rc
			JOIN information_schema.key_column_usage kcu ON rc.constraint_name = kcu.constraint_name
			JOIN information_schema.constraint_column_usage ccu ON rc.constraint_name = ccu.constraint_name
			WHERE kcu.table_name = 'evaluation_results' AND kcu.column_name = 'dataset_id'
			AND ccu.table_name = 'evaluation_datasets' AND ccu.column_name = 'id'
		)`).Scan(&fkExists)
	if err != nil {
		t.Fatalf("查询 evaluation_results 外键: %v", err)
	}
	if !fkExists {
		t.Error("evaluation_results.dataset_id → evaluation_datasets.id 外键不存在")
	}

	// 验证 evaluation_results 的外键引用 search_profiles
	err = db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT FROM information_schema.referential_constraints rc
			JOIN information_schema.key_column_usage kcu ON rc.constraint_name = kcu.constraint_name
			JOIN information_schema.constraint_column_usage ccu ON rc.constraint_name = ccu.constraint_name
			WHERE kcu.table_name = 'evaluation_results' AND kcu.column_name = 'profile_id'
			AND ccu.table_name = 'search_profiles' AND ccu.column_name = 'id'
		)`).Scan(&fkExists)
	if err != nil {
		t.Fatalf("查询 evaluation_results 外键: %v", err)
	}
	if !fkExists {
		t.Error("evaluation_results.profile_id → search_profiles.id 外键不存在")
	}

	// 验证 M005 (system_settings) 表不存在——M005 尚未创建
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'system_settings')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询 system_settings 表: %v", err)
	}
	if exists {
		t.Error("system_settings 表不应存在——M005 保留给 Phase 6")
	}
}

// TestIntegration_M004DownDropsEvaluationResults 验证 M004 down 删除 evaluation_results。
func TestIntegration_M004DownDropsEvaluationResults(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	migrationsDir := findProjectMigrationsDir(t)
	migrations, err := LoadMigrations(migrationsDir)
	if err != nil {
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	runner := NewRunner(db, migrations)
	_, err = runner.Up(ctx)
	if err != nil {
		t.Fatalf("迁移 Up 失败: %v", err)
	}

	// 回退到 M004：计算需要回退多少个高于 4 的迁移（M005 不存在，但 M006-M011 存在）。
	// 降序应用 Down，直到版本 4 被回退为止。
	aboveFour := 0
	for _, m := range migrations {
		if m.Version > 4 {
			aboveFour++
		}
	}
	reverted, err := runner.Down(ctx, aboveFour+1)
	if err != nil {
		t.Fatalf("迁移 Down 失败: %v", err)
	}
	if reverted != aboveFour+1 {
		t.Errorf("期望回退 %d 个迁移（所有高于 M004 的 + M004 自身），实际回退 %d 个", aboveFour+1, reverted)
	}

	// 验证 evaluation_results 表已删除
	var exists bool
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'evaluation_results')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询 evaluation_results 表: %v", err)
	}
	if exists {
		t.Error("回退 M004 后 evaluation_results 表不应存在")
	}

	// 验证 evaluation_datasets 表也已删除
	err = db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'evaluation_datasets')").Scan(&exists)
	if err != nil {
		t.Fatalf("查询 evaluation_datasets 表: %v", err)
	}
	if exists {
		t.Error("回退 M004 后 evaluation_datasets 表不应存在")
	}
}
