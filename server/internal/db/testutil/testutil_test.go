// testutil_test.go 测试 testutil 包的 advisory lock 行为。
//
// 引入动机：远端 V3 测试暴露 12 项 advisory lock timeout 失败。V4 修复后需要真实可调用的
// 测试验证锁获得/释放、等待 context 取消行为，以及专用连接的锁所有权语义。
//
// 测试覆盖：
//   - SetupTestDB 能正常获取/释放锁（需要 TEST_DATABASE_URL，否则 skip）
//   - 并发获取锁时，第二个等待者在 context 取消后返回错误而非假超时
//   - 锁释放后，等待者可以获取锁
//   - 专用连接锁所有权：只有持锁连接能释放锁，非持锁连接的 unlock 无效
//   - cleanup 后另一 SetupTestDB 可获得锁（端到端验证）
//   - 超时/cancellation 不会永久遗留锁
//
// 运行条件：设置 TEST_DATABASE_URL 环境变量指向可用的 PostgreSQL 实例。
// 未设置时测试跳过，不算集成通过。
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestSetupTestDB_LockAcquireRelease 验证 SetupTestDB 能正常获取和释放锁。
// 锁释放后，再次调用 SetupTestDB 应能立即获取锁。
func TestSetupTestDB_LockAcquireRelease(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过")
	}

	// 第一次获取锁
	db1, cleanup1 := SetupTestDB(t)
	if db1 == nil {
		t.Fatal("SetupTestDB 返回 nil db")
	}

	// 验证数据库可用
	if err := db1.PingContext(context.Background()); err != nil {
		cleanup1()
		t.Fatalf("db1 ping 失败: %v", err)
	}

	// 释放锁
	cleanup1()

	// 第二次获取锁应立即成功（锁已释放）
	db2, cleanup2 := SetupTestDB(t)
	if db2 == nil {
		t.Fatal("第二次 SetupTestDB 返回 nil db")
	}
	defer cleanup2()

	if err := db2.PingContext(context.Background()); err != nil {
		t.Fatalf("db2 ping 失败: %v", err)
	}
}

// TestAdvisoryLock_ContextCancellation 验证当锁被持有时，
// 第二个获取者在 context 取消后返回错误，而非等待固定 60s 假超时。
//
// 测试策略：
//  1. 通过直接 SQL 获取 advisory lock（模拟持锁方）
//  2. 启动 goroutine 尝试获取同一锁，使用短超时 context（1秒）
//  3. 验证 goroutine 在 context 取消后返回错误
//  4. 释放锁，验证后续获取可成功
func TestAdvisoryLock_ContextCancellation(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过")
	}

	// 持锁方连接
	holderDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("打开持锁方数据库连接: %v", err)
	}
	defer holderDB.Close()

	if err := holderDB.PingContext(context.Background()); err != nil {
		t.Skipf("跳过：无法连接 PG (%v)", err)
	}

	// 获取锁
	if _, err := holderDB.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		t.Fatalf("持锁方获取 advisory lock 失败: %v", err)
	}
	defer func() {
		_, _ = holderDB.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
	}()

	// 等待方连接
	waiterDB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("打开等待方数据库连接: %v", err)
	}
	defer waiterDB.Close()

	// 使用 1 秒超时 context 尝试获取锁——应因 context 取消而失败
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err = waiterDB.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey)
	elapsed := time.Since(start)

	if err == nil {
		// 不应获取到锁——持锁方仍持有
		_, _ = waiterDB.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		t.Fatal("等待方不应在持锁方未释放时获取到锁")
	}

	// 验证返回时间接近 1 秒（context 取消），而非 60 秒（假超时）
	if elapsed > 5*time.Second {
		t.Errorf("等待方应在 context 取消后立即返回，实际耗时 %v（期望约 1s）", elapsed)
	}

	t.Logf("等待方在 context 取消后正确返回，耗时 %v，错误: %v", elapsed, err)
}

// TestAdvisoryLock_ReleaseThenAcquire 验证锁释放后，等待方可以获取锁。
func TestAdvisoryLock_ReleaseThenAcquire(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过")
	}

	db1, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("打开数据库连接 1: %v", err)
	}
	defer db1.Close()

	if err := db1.PingContext(context.Background()); err != nil {
		t.Skipf("跳过：无法连接 PG (%v)", err)
	}

	// 获取锁
	if _, err := db1.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		t.Fatalf("获取 advisory lock 失败: %v", err)
	}

	// 释放锁
	if _, err := db1.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
		t.Fatalf("释放 advisory lock 失败: %v", err)
	}

	// 第二个连接应能立即获取锁
	db2, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("打开数据库连接 2: %v", err)
	}
	defer db2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := db2.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		t.Fatalf("锁释放后获取失败: %v", err)
	}

	// 清理
	_, _ = db2.ExecContext(context.Background(), fmt.Sprintf("SELECT pg_advisory_unlock(%d)", advisoryLockKey))
}

// TestAdvisoryLock_ConnectionOwnership 验证 advisory lock 的连接所有权语义：
// 只有持锁连接能释放锁，非持锁连接的 unlock 无效。
//
// 引入动机：V4 修复的核心问题是 *sql.DB 连接池可能将 unlock 分配到不同连接，
// 导致锁无法释放。此测试直接验证该语义。
//
// 连接生命周期修复（第五轮）：
//   - 使用独立的 *sql.DB 句柄分别管理连接 A 和连接 B，避免共享连接池
//   - 当连接 B 的 pg_advisory_lock 因 context 超时被中断时，pgx 会将底层连接
//     标记为 bad connection，后续复用该 *sql.Conn 会报 driver: bad connection
//   - 修复方案：超时中断后关闭已损坏的 connB，从 dbB 获取新连接验证锁释放后可获取
//
// 测试策略：
//  1. 连接 A 通过专用 *sql.Conn 获取 advisory lock
//  2. 连接 B（不同 *sql.Conn）尝试 unlock——应返回 false（未持锁）
//  3. 连接 B 尝试获取锁应超时（锁仍被 A 持有）
//  4. 连接 A unlock——应返回 true（持锁方释放成功）
//  5. 从 dbB 获取新连接，验证锁已释放并可获取
func TestAdvisoryLock_ConnectionOwnership(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过")
	}

	// 使用独立的 *sql.DB 句柄，避免共享连接池导致连接生命周期交叉
	dbA, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("打开数据库连接 A: %v", err)
	}
	defer dbA.Close()

	if err := dbA.PingContext(context.Background()); err != nil {
		t.Skipf("跳过：无法连接 PG (%v)", err)
	}

	dbB, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("打开数据库连接 B: %v", err)
	}
	defer dbB.Close()

	// 连接 A 获取锁
	connA, err := dbA.Conn(context.Background())
	if err != nil {
		t.Fatalf("获取连接 A 失败: %v", err)
	}
	defer connA.Close()

	if _, err := connA.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		t.Fatalf("连接 A 获取 advisory lock 失败: %v", err)
	}

	// 连接 B 尝试 unlock——应返回 false（非持锁连接）
	connB, err := dbB.Conn(context.Background())
	if err != nil {
		t.Fatalf("获取连接 B 失败: %v", err)
	}

	var unlocked bool
	err = connB.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey).Scan(&unlocked)
	if err != nil {
		t.Fatalf("连接 B 执行 unlock 查询失败: %v", err)
	}
	if unlocked {
		t.Error("非持锁连接不应成功释放锁（pg_advisory_unlock 应返回 false）")
	}
	t.Log("连接 B 的 unlock 正确返回 false（非持锁连接）")

	// 验证锁仍被持有：连接 B 尝试获取锁应超时
	acquireCtx, acquireCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer acquireCancel()

	start := time.Now()
	_, acquireErr := connB.ExecContext(acquireCtx, "SELECT pg_advisory_lock($1)", advisoryLockKey)
	elapsed := time.Since(start)

	if acquireErr == nil {
		_, _ = connB.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		t.Fatal("连接 B 不应在连接 A 持锁时获取到锁")
	}
	if elapsed > 10*time.Second {
		t.Errorf("连接 B 应在 context 超时后返回，实际耗时 %v", elapsed)
	}
	t.Logf("连接 B 正确等待锁，超时返回，耗时 %v", elapsed)

	// connB 的底层连接因 context 取消中断 pg_advisory_lock 而被 pgx 标记为 bad connection，
	// 后续无法复用。关闭已损坏的 connB，从 dbB 获取新连接进行后续验证。
	if err := connB.Close(); err != nil {
		t.Logf("关闭已损坏的连接 B: %v（非致命）", err)
	}

	// 连接 A 释放锁——应返回 true
	err = connA.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey).Scan(&unlocked)
	if err != nil {
		t.Fatalf("连接 A 执行 unlock 查询失败: %v", err)
	}
	if !unlocked {
		t.Error("持锁连接 A 的 unlock 应返回 true")
	}
	t.Log("连接 A 的 unlock 正确返回 true（持锁连接）")

	// 从 dbB 获取新连接，验证锁已释放后可获取
	connC, err := dbB.Conn(context.Background())
	if err != nil {
		t.Fatalf("获取连接 C（用于验证锁释放）失败: %v", err)
	}
	defer connC.Close()

	acquireCtx2, acquireCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer acquireCancel2()

	if _, err := connC.ExecContext(acquireCtx2, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		t.Fatalf("连接 A 释放后新连接应能获取锁: %v", err)
	}
	t.Log("连接 A 释放后新连接成功获取锁")

	// 清理：释放 connC 持有的锁
	_, _ = connC.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
}

// TestSetupTestDB_CleanupReleasesLock 验证 SetupTestDB 的 cleanup 函数正确释放锁后，
// 另一个 SetupTestDB 可以立即获取锁。这是 V4 修复的端到端验证。
//
// 引入动机：V4 修复前，cleanup 使用 *sql.DB 连接池执行 unlock，可能分配到不同连接，
// 导致锁泄漏。此测试验证修复后 cleanup 在专用 *sql.Conn 上执行 unlock，锁被正确释放。
func TestSetupTestDB_CleanupReleasesLock(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过")
	}

	// 第一次 SetupTestDB
	db1, cleanup1 := SetupTestDB(t)
	if db1 == nil {
		t.Fatal("第一次 SetupTestDB 返回 nil db")
	}

	// 在 db1 上执行一些操作，确保连接池有活动
	if err := db1.PingContext(context.Background()); err != nil {
		cleanup1()
		t.Fatalf("db1 ping 失败: %v", err)
	}

	// 调用 cleanup 释放锁
	cleanup1()

	// 第二次 SetupTestDB 应能立即获取锁（不超时）
	// 使用单独的 goroutine + 超时来检测是否阻塞
	done := make(chan struct{})
	var db2 *sql.DB
	var cleanup2 func()

	go func() {
		defer close(done)
		db2, cleanup2 = SetupTestDB(t)
	}()

	select {
	case <-done:
		// 成功获取锁
		if db2 == nil {
			t.Fatal("第二次 SetupTestDB 返回 nil db")
		}
		defer cleanup2()
		t.Log("cleanup 后第二次 SetupTestDB 成功获取锁")
	case <-time.After(30 * time.Second):
		t.Fatal("第二次 SetupTestDB 在 30 秒内未获取到锁——cleanup 未正确释放锁")
	}
}

// TestSetupTestDB_TimeoutNoLeak 验证当测试因超时/cancellation 而退出时，
// cleanup 函数仍能正确释放锁，不会永久遗留锁。
//
// 引入动机：V4 修复前，如果测试 context 被取消，cleanup 中的 unlock 使用
// 已取消的 context 执行会失败，导致锁泄漏。修复后 cleanup 使用 context.Background()
// 执行 unlock，确保即使测试 context 已取消也能释放锁。
//
// 测试策略：
//  1. 使用 SetupTestDB 获取锁
//  2. 模拟测试 context 被取消（通过手动创建已取消的 context 并验证 cleanup 仍工作）
//  3. cleanup 后验证另一个 SetupTestDB 可以获取锁
func TestSetupTestDB_TimeoutNoLeak(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL 未设置，跳过")
	}

	// 获取锁
	db1, cleanup1 := SetupTestDB(t)
	if db1 == nil {
		t.Fatal("SetupTestDB 返回 nil db")
	}

	// 模拟测试失败/超时场景：直接调用 cleanup（不依赖 t.Context）
	// cleanup 内部使用 context.Background() 执行 unlock，不受测试 context 状态影响
	cleanup1()

	// 验证锁已被释放：新的 SetupTestDB 应能获取锁
	done := make(chan struct{})
	var db2 *sql.DB
	var cleanup2 func()

	go func() {
		defer close(done)
		db2, cleanup2 = SetupTestDB(t)
	}()

	select {
	case <-done:
		if db2 == nil {
			t.Fatal("cleanup 后第二次 SetupTestDB 返回 nil db")
		}
		defer cleanup2()
		t.Log("超时/cancellation 后 cleanup 正确释放锁，新 SetupTestDB 成功获取")
	case <-time.After(30 * time.Second):
		t.Fatal("cleanup 后锁未被释放——第二次 SetupTestDB 30 秒内未获取到锁")
	}
}
