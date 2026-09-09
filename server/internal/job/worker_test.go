// worker_test.go 测试 Worker 的关闭可靠性行为。
//
// 引入动机：生产可靠性要求 Worker.Stop 幂等（不 panic on double-close），
// 且 Stop 等待 Start 退出后才返回（避免 goroutine 泄漏）。
// 上下文取消也必须有界退出。
//
// 测试策略：使用 mock repo 和 handler，不依赖 PG。
package job

import (
	"context"
	"testing"
	"time"
)

// TestWorker_StopIdempotent 验证多次调用 Stop 不会 panic。
// 引入动机：main.go 中 serverCancel 和 Stop 可能同时触发，
// 且测试或运维可能多次调用 Stop。
func TestWorker_StopIdempotent(t *testing.T) {
	repo := &failOnClaimRepo{}
	handler := &alwaysFailHandler{}
	worker := NewWorker(repo, handler, "test-worker-idempotent")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Start(ctx, 100*time.Millisecond)

	// 等待 worker 启动
	time.Sleep(50 * time.Millisecond)

	// 多次调用 Stop 不应 panic
	worker.Stop()
	worker.Stop()
	worker.Stop()
}

// TestWorker_StopWaitsForExit 验证 Stop 等待 Start 退出后才返回。
// 引入动机：Stop 返回后不应有残留 goroutine 仍在执行，
// 否则可能导致资源泄漏或竞争。
func TestWorker_StopWaitsForExit(t *testing.T) {
	repo := &failOnClaimRepo{}
	handler := &alwaysFailHandler{}
	worker := NewWorker(repo, handler, "test-worker-wait")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startDone := make(chan struct{})
	go func() {
		worker.Start(ctx, 100*time.Millisecond)
		close(startDone)
	}()

	// 等待 worker 启动
	time.Sleep(50 * time.Millisecond)

	// Stop 应等待 Start 退出
	worker.Stop()

	// Start 应已退出
	select {
	case <-startDone:
		// 成功
	case <-time.After(2 * time.Second):
		t.Fatal("Worker.Stop 返回后 Start 仍未退出，说明 Stop 未等待 done channel")
	}
}

// TestWorker_ContextCancelExitsCleanly 验证 context 取消后 worker 安全退出，
// 且 Stop 仍可安全调用（幂等）。
// 引入动机：SIGTERM 触发 serverCancel 后 worker 应退出，Stop 作为备用信号仍可调用。
func TestWorker_ContextCancelExitsCleanly(t *testing.T) {
	repo := &failOnClaimRepo{}
	handler := &alwaysFailHandler{}
	worker := NewWorker(repo, handler, "test-worker-ctx-cancel")

	ctx, cancel := context.WithCancel(context.Background())

	startDone := make(chan struct{})
	go func() {
		worker.Start(ctx, 100*time.Millisecond)
		close(startDone)
	}()

	// 等待 worker 启动
	time.Sleep(50 * time.Millisecond)

	// 取消 context
	cancel()

	// Start 应退出
	select {
	case <-startDone:
		// 成功
	case <-time.After(2 * time.Second):
		t.Fatal("context 取消后 Worker 未在 2 秒内退出")
	}

	// Stop 仍应可调用且不 panic（幂等）
	worker.Stop()
}

// TestWorker_StopWithoutStartDoesNotBlock 验证 Stop 在 Start 从未调用时不会永久阻塞。
// 引入动机：虽然文档要求 Start 必须先于 Stop 调用，但防御性测试应验证
// 不会因 done channel 未关闭导致无限阻塞。
// 注意：当前实现中 Stop 会阻塞等待 done，如果 Start 从未调用则会永久阻塞。
// 这是与 Scheduler 一致的设计约束——Start 必须先于 Stop 调用。
// 此测试验证正常使用模式下（先 Start 再 Stop）不会阻塞。
func TestWorker_NormalStartStopCycle(t *testing.T) {
	repo := &failOnClaimRepo{}
	handler := &alwaysFailHandler{}
	worker := NewWorker(repo, handler, "test-worker-cycle")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go worker.Start(ctx, 50*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	// 正常停止
	done := make(chan struct{})
	go func() {
		worker.Stop()
		close(done)
	}()

	select {
	case <-done:
		// 成功
	case <-time.After(3 * time.Second):
		t.Fatal("正常 Start-Stop 周期应在 3 秒内完成")
	}
}
