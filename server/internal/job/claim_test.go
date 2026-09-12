// claim_test.go 测试 Claim 方法的事务泄漏修复和正常流程。
//
// 引入动机：Claim 方法原先在 BeginTx/SELECT FOR UPDATE SKIP LOCKED 之后
// 解析 payload 时使用 := 遮蔽外层 err，JSON 解析错误直接返回但 defer 的
// if err != nil 不触发 Rollback，导致事务泄漏/idle in transaction。
//
// 测试策略：
//   - 非对象 payload：插入合法 JSONB 但非 JSON 对象（如 '[]'）的 pending job，
//     Claim 的 json.Unmarshal 到 map[string]interface{} 会失败，验证返回错误、
//     事务已释放（同一行可被 FOR UPDATE 锁定，不阻塞）、job 状态仍为 pending
//   - 正常流程：合法 payload 的 job 被 claim 后状态变为 running、payload 正确解析
//   - 无任务：空队列时 Claim 返回 nil job 且无错误
//
// 集成测试在 TEST_DATABASE_URL 未设置时自动 skip。
package job

import (
	"context"
	"testing"
	"time"

	types "partitura/server/internal/search/types"
)

// TestClaim_InvalidWorkerID 验证 worker ID 契约在进入数据库事务前 fail-fast。
// 保持 Claim 的 string 参数兼容现有调用方，同时拒绝无法写入 UUID 列的值。
func TestClaim_InvalidWorkerID(t *testing.T) {
	repo := NewPGRepository(nil)
	claimed, err := repo.Claim(context.Background(), "test-worker")
	if err == nil {
		t.Fatal("非法 worker ID 应返回错误")
	}
	if claimed != nil {
		t.Fatalf("非法 worker ID 不应返回 job: %+v", claimed)
	}
}

// TestClaim_NonObjectPayload 验证 Claim 在 payload 为合法 JSONB 但非 JSON 对象时
// 返回错误且事务已释放。
//
// 引入动机：修复前 Claim 方法中 json.Unmarshal 使用 := 遮蔽外层 err，
// 导致 JSON 解析错误返回时 defer 不触发 Rollback，事务泄漏为 idle in transaction。
//
// 设计约束：jobs.payload 列为 JSONB NOT NULL，PostgreSQL 在 INSERT 时即保证
// 值为合法 JSON。因此无法通过直接 SQL 插入"损坏 JSON"来测试——PG 会拒绝非法 JSON。
// 但 JSONB 允许存储任意合法 JSON 值（包括数组、布尔、数字等），而 Claim 使用
// json.Unmarshal 到 map[string]interface{}，仅接受 JSON 对象。
// 插入 '[]'（合法 JSON 数组）可绕过 PG 约束，同时触发 Claim 的类型不匹配错误路径。
//
// 此测试构造非对象 payload，调用 Claim 后验证：
//  1. 返回错误（非 nil），不返回 job
//  2. 事务已释放——同一行可被后续 FOR UPDATE 锁定（不阻塞超时）
//  3. job 状态仍为 pending，attempt_count 未增加（UPDATE 未执行或已回滚）
func TestClaim_NonObjectPayload(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 插入一条 payload 为合法 JSONB 但非 JSON 对象（空数组）的 pending job。
	// '[]' 是合法 JSON，PG 的 JSONB 约束允许存储，但 json.Unmarshal 到
	// map[string]interface{} 会报 "cannot unmarshal array into Go value of type map[string]interface{}"。
	var jobID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status) VALUES ('index_document', $1, 'pending') RETURNING id`,
		`[]`,
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("插入非对象 payload job 失败: %v", err)
	}

	repo := NewPGRepository(db)
	claimed, err := repo.Claim(ctx, "00000000-0000-0000-0000-000000000001")
	if err == nil {
		t.Fatal("payload 为非对象 JSON 时 Claim 应返回错误，但返回 nil")
	}
	if claimed != nil {
		t.Fatalf("payload 为非对象 JSON 时 Claim 不应返回 job，但返回了 %+v", claimed)
	}

	// 验证 job 状态仍为 pending，attempt_count 仍为 0
	var status string
	var attemptCount int
	err = db.QueryRowContext(ctx,
		`SELECT status, attempt_count FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&status, &attemptCount)
	if err != nil {
		t.Fatalf("查询 job 状态失败: %v", err)
	}
	if status != types.JobPending {
		t.Errorf("job 状态应为 pending，实际 %s", status)
	}
	if attemptCount != 0 {
		t.Errorf("attempt_count 应为 0，实际 %d", attemptCount)
	}

	// 验证事务已释放：用短超时 context 尝试 FOR UPDATE 锁定同一行。
	// 如果前一次 Claim 的事务未回滚（idle in transaction），行锁仍被持有，
	// 此操作会阻塞直至超时并返回 context deadline exceeded 错误。
	lockCtx, lockCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer lockCancel()

	var lockedID string
	err = db.QueryRowContext(lockCtx,
		`SELECT id FROM jobs WHERE id = $1 FOR UPDATE`,
		jobID,
	).Scan(&lockedID)
	if err != nil {
		t.Fatalf("无法锁定 job 行，可能前一次 Claim 事务未释放（idle in transaction）: %v", err)
	}
	if lockedID != jobID {
		t.Errorf("锁定的 job ID 不匹配: 期望 %s，实际 %s", jobID, lockedID)
	}
}

// TestClaim_NormalFlow 验证正常 Claim 流程：合法 payload 的 job 被 claim 后状态变为 running。
// 引入动机：确保事务泄漏修复未破坏正常 Claim 的状态/锁/返回语义。
func TestClaim_NormalFlow(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 插入一条合法 payload 的 pending job
	var jobID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status) VALUES ('index_document', $1, 'pending') RETURNING id`,
		`{"document_id":"doc-1","workspace_id":"ws-1"}`,
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("插入合法 payload job 失败: %v", err)
	}

	repo := NewPGRepository(db)
	workerID := "00000000-0000-0000-0000-000000000002"
	claimed, err := repo.Claim(ctx, workerID)
	if err != nil {
		t.Fatalf("Claim 正常 job 失败: %v", err)
	}
	if claimed == nil {
		t.Fatal("Claim 应返回 job，但返回 nil")
	}
	if claimed.ID != jobID {
		t.Errorf("claim 的 job ID 不匹配: 期望 %s，实际 %s", jobID, claimed.ID)
	}
	if claimed.Status != types.JobRunning {
		t.Errorf("claim 后 job 状态应为 running，实际 %s", claimed.Status)
	}
	if claimed.Payload == nil {
		t.Error("claim 后 payload 不应为 nil")
	}
	if claimed.Payload["document_id"] != "doc-1" {
		t.Errorf("payload document_id 应为 doc-1，实际 %v", claimed.Payload["document_id"])
	}

	// 验证数据库中状态已更新为 running，attempt_count 已递增
	var status string
	var attemptCount int
	err = db.QueryRowContext(ctx,
		`SELECT status, attempt_count FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&status, &attemptCount)
	if err != nil {
		t.Fatalf("查询 job 状态失败: %v", err)
	}
	if status != types.JobRunning {
		t.Errorf("数据库中 job 状态应为 running，实际 %s", status)
	}
	if attemptCount != 1 {
		t.Errorf("attempt_count 应为 1，实际 %d", attemptCount)
	}
}

// TestClaim_NoJobs 验证无待执行任务时 Claim 返回 nil job 且无错误。
// 引入动机：确保空队列场景的返回语义未被修复破坏。
func TestClaim_NoJobs(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	repo := NewPGRepository(db)
	claimed, err := repo.Claim(ctx, "00000000-0000-0000-0000-000000000003")
	if err != nil {
		t.Fatalf("无待执行任务时 Claim 不应返回错误: %v", err)
	}
	if claimed != nil {
		t.Fatalf("无待执行任务时 Claim 应返回 nil，但返回了 %+v", claimed)
	}
}
