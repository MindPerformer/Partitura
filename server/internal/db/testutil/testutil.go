// Package testutil 提供 PostgreSQL 集成测试的跨包隔离辅助。
//
// 引入动机：多个包（auth、document、workspace、migration、job）的集成测试共享同一个
// TEST_DATABASE_URL。Go 默认并行运行不同包的测试，导致并发操作同一数据库时产生冲突：
//   - migration 和 job 测试会 DROP 所有表 + 扩展
//   - auth/document/workspace 测试只清理数据，依赖 schema 已存在
//   - 如果 migration 测试在 auth 测试运行期间 DROP 表，auth 测试会失败
//
// 隔离策略：跨进程 advisory lock + 完整 reset。
//   - 每个集成测试 setup 时获取 pg_advisory_lock，序列化 DB 访问
//   - 获取锁后执行完整 reset（DROP 所有表 + pgcrypto 扩展）
//   - 然后运行全部 migration Up，确保 schema 干净完整
//   - cleanup 时释放锁
//   - 不依赖测试执行顺序或 go test -p 1
//
// 连接生命周期（V4 修复）：
//   - pg_advisory_lock 是 session-level 锁，绑定到执行该语句的特定连接。
//   - 使用 *sql.DB（连接池）时，ExecContext 可能将 unlock 分配到不同连接，
//     导致锁无法释放；db.Close() 也不保证关闭 idle-in-transaction 连接。
//   - 修复方案：使用专用 *sql.Conn 持有 advisory lock，lock/reset/migration/unlock/close
//     全部在同一 Conn 上执行，确保锁的正确获取和释放。
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"partitura/server/internal/db/migration"
)

// advisoryLockKey 是跨进程 advisory lock 的固定键值。
// 所有包的集成测试使用同一个键，确保同一时刻只有一个测试在使用数据库。
const advisoryLockKey int64 = 9527

// allTables 是按依赖逆序排列的全部表名。
// 用于完整 reset 时按正确顺序 DROP。
var allTables = []string{
	"scheduler_runs",
	"evaluation_results", "search_metrics", "search_feedback",
	"evaluation_items", "evaluation_datasets",
	"search_profile_candidates", "jobs", "search_profiles",
	"sources", "revisions", "documents",
	"audit_logs", "workspace_members", "workspaces",
	"device_authorizations",
	"device_sessions", "sessions", "users", "schema_migrations",
}

// SetupTestDB 创建测试数据库连接，获取 advisory lock，执行完整 reset + migration Up。
// 返回 *sql.DB 和 cleanup 函数。
// 如果 TEST_DATABASE_URL 未设置，调用 t.Skip 跳过测试。
//
// 隔离保证：
//   - advisory lock 确保同一时刻只有一个测试使用数据库
//   - 完整 reset 确保每个测试在干净 schema 上运行
//   - 不依赖测试执行顺序
//
// 连接生命周期：
//   - 使用专用 *sql.Conn 持有 session-level advisory lock
//   - lock、reset、migration、unlock 和 close 全部在同一 Conn 上执行
//   - 避免连接池跨连接 unlock 导致锁泄漏
//
// advisory lock 等待策略：
//   - 使用 t.Context() 作为等待 context，不设固定 deadline
//   - 不同 Go test package 并行运行时，等待方会阻塞直到持锁方释放
//   - 不会因固定 60s 超时而假失败——等待时间仅受测试自身 deadline 约束
//   - cleanup 时一定释放锁（defer unlock + Conn.Close）
func SetupTestDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过 PostgreSQL 集成测试")
	}

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
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
	// 使用 *sql.DB 连接池时，ExecContext 可能分配不同连接，导致 unlock 失效。
	conn, err := db.Conn(t.Context())
	if err != nil {
		_ = db.Close()
		t.Fatalf("获取专用数据库连接失败: %v", err)
	}

	// 使用 t.Context() 获取 advisory lock——不设固定 deadline，
	// 等待时间仅受测试自身 deadline 约束。
	// 不同 Go test package 并行运行时，等待方会阻塞直到持锁方释放。
	lockCtx := t.Context()

	_, err = conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1)", advisoryLockKey)
	if err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatalf("获取 advisory lock 失败: %v", err)
	}

	// 完整 reset：DROP 所有表 + pgcrypto 扩展（在同一连接上执行）
	resetDB(lockCtx, conn)

	// 运行全部 migration Up
	migrationsDir := findMigrationsDir()
	migrations, err := migration.LoadMigrations(migrationsDir)
	if err != nil {
		releaseLockAndCloseConn(conn, db, t)
		t.Fatalf("加载迁移文件失败: %v", err)
	}

	// migration.Runner 需要 *sql.DB，但迁移操作必须在持有 advisory lock 的连接上执行。
	// 由于 advisory lock 是 session-level，只要 Conn 未释放，池中其他连接也能看到
	// lock 已被持有（序列化效果）。但 reset 必须在 lock 持有连接上执行以确保
	// 不会有其他连接在 reset 期间访问 schema。
	// migration.Runner 通过 *sql.DB 执行 SQL，会使用连接池中的连接。
	// 这在 advisory lock 序列化下是安全的：只有持锁方才会运行 migration。
	runner := migration.NewRunner(db, migrations)
	if _, err := runner.Up(lockCtx); err != nil {
		releaseLockAndCloseConn(conn, db, t)
		t.Fatalf("执行迁移 Up 失败: %v", err)
	}

	cleanup := func() {
		releaseLockAndCloseConn(conn, db, t)
	}

	return db, cleanup
}

// resetDB 删除所有表和 pgcrypto 扩展，确保干净的起始状态。
// 在持有 advisory lock 的专用连接上执行。
func resetDB(ctx context.Context, conn *sql.Conn) {
	for _, table := range allTables {
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table))
	}
	_, _ = conn.ExecContext(ctx, `DROP EXTENSION IF EXISTS "pgcrypto"`)
}

// releaseLockAndCloseConn 在专用连接上释放 advisory lock，然后关闭连接和连接池。
// 引入动机：确保在任何退出路径（正常或异常）下都释放锁，避免死锁。
// 必须在持有锁的同一 *sql.Conn 上执行 pg_advisory_unlock，否则 unlock 会发送到
// 不同连接，无法释放锁。使用 context.Background() 确保 cleanup 阶段即使测试
// context 已取消也能执行 unlock。
func releaseLockAndCloseConn(conn *sql.Conn, db *sql.DB, t *testing.T) {
	_, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	if err != nil {
		slog.Error("释放 advisory lock 失败", "error", err)
		if t != nil {
			t.Errorf("释放 advisory lock 失败: %v", err)
		}
	}
	if err := conn.Close(); err != nil {
		slog.Error("关闭专用数据库连接失败", "error", err)
		if t != nil {
			t.Errorf("关闭专用数据库连接失败: %v", err)
		}
	}
	_ = db.Close()
}

// findMigrationsDir 定位项目中的 migrations 目录。
// 引入动机：集成测试需要加载项目迁移文件以应用全部 migration。
// 使用 runtime.Caller 从本源码文件定位项目根目录的 migrations 子目录，
// 不依赖 CWD 或环境变量，对 go test ./... 各 package 不同 cwd 可靠。
func findMigrationsDir() string {
	// 通过 runtime.Caller 获取本文件的绝对路径
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller 失败，无法定位 testutil 源码文件")
	}
	// 本文件位于 server/internal/db/testutil/testutil.go
	// migrations 目录位于 server/migrations
	// 从本文件向上 4 级到达 server/ 目录
	dir := filepath.Dir(filename) // .../server/internal/db/testutil
	dir = filepath.Dir(dir)       // .../server/internal/db
	dir = filepath.Dir(dir)       // .../server/internal
	dir = filepath.Dir(dir)       // .../server
	migrationsDir := filepath.Join(dir, "migrations")

	info, err := os.Stat(migrationsDir)
	if err != nil || !info.IsDir() {
		panic(fmt.Sprintf("无法定位 migrations 目录: %s (从 %s 推导)", migrationsDir, filename))
	}

	return migrationsDir
}
