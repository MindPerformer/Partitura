// recover_test.go 测试 RecoverStaleJobs 的行为。
//
// 引入动机：worker 崩溃或被 SIGKILL 后，其 claim 的 job 会永久停留在 running 状态。
// RecoverStaleJobs 将长时间 running 的 job 重置为 pending，使其重新可被 claim。
//
// 测试策略：集成测试，需要 PG（无 PG 时自动 skip）。
// 插入不同 locked_at 的 running job，验证只有超过 staleTimeout 的 job 被恢复。
package job

import (
	"context"
	"testing"
	"time"
)

// TestRecoverStaleJobs_ResetsStaleRunning 验证 RecoverStaleJobs 将长时间 running 的 job 重置为 pending。
func TestRecoverStaleJobs_ResetsStaleRunning(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 插入一个 stale running job（locked_at 为 10 分钟前）
	var jobID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status, locked_by, locked_at, started_at, attempt_count)
		 VALUES ('index_document', '{}', 'running',
		         '00000000-0000-0000-0000-000000000001',
		         now() - interval '10 minutes',
		         now() - interval '10 minutes', 1)
		 RETURNING id`,
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("插入 stale running job 失败: %v", err)
	}

	repo := NewPGRepository(db)
	count, err := repo.RecoverStaleJobs(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleJobs 失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("恢复 count = %d, 期望 1", count)
	}

	// 验证 job 已重置为 pending
	var status string
	var lockedBy *string
	err = db.QueryRowContext(ctx,
		`SELECT status, locked_by FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&status, &lockedBy)
	if err != nil {
		t.Fatalf("查询 job 状态失败: %v", err)
	}
	if status != "pending" {
		t.Errorf("job 状态应为 pending, 实际 %s", status)
	}
	if lockedBy != nil {
		t.Errorf("locked_by 应为 NULL, 实际 %s", *lockedBy)
	}
}

// TestRecoverStaleJobs_DoesNotResetRecentRunning 验证 RecoverStaleJobs 不重置刚 claim 的 running job。
func TestRecoverStaleJobs_DoesNotResetRecentRunning(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 插入一个刚 claim 的 running job（locked_at 为 now）
	var jobID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status, locked_by, locked_at, started_at, attempt_count)
		 VALUES ('index_document', '{}', 'running',
		         '00000000-0000-0000-0000-000000000002',
		         now(), now(), 1)
		 RETURNING id`,
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("插入 running job 失败: %v", err)
	}

	repo := NewPGRepository(db)
	count, err := repo.RecoverStaleJobs(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleJobs 失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("恢复 count = %d, 期望 0（刚 claim 的 job 不应被恢复）", count)
	}

	// 验证 job 仍为 running
	var status string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&status)
	if err != nil {
		t.Fatalf("查询 job 状态失败: %v", err)
	}
	if status != "running" {
		t.Errorf("job 状态应为 running, 实际 %s", status)
	}
}

// TestRecoverStaleJobs_DefaultTimeout 验证 staleTimeout <= 0 时使用默认超时。
func TestRecoverStaleJobs_DefaultTimeout(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 插入一个 stale running job（locked_at 为 1 小时前，超过默认 5 分钟超时）
	var jobID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status, locked_by, locked_at, started_at, attempt_count)
		 VALUES ('index_document', '{}', 'running',
		         '00000000-0000-0000-0000-000000000003',
		         now() - interval '1 hour',
		         now() - interval '1 hour', 1)
		 RETURNING id`,
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("插入 stale running job 失败: %v", err)
	}

	repo := NewPGRepository(db)
	// 使用 0 超时，应使用默认超时（5 分钟）
	count, err := repo.RecoverStaleJobs(ctx, 0)
	if err != nil {
		t.Fatalf("RecoverStaleJobs 失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("恢复 count = %d, 期望 1（1 小时前的 job 应超过默认 5 分钟超时）", count)
	}

	// 验证 job 已重置为 pending
	var status string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&status)
	if err != nil {
		t.Fatalf("查询 job 状态失败: %v", err)
	}
	if status != "pending" {
		t.Errorf("job 状态应为 pending, 实际 %s", status)
	}
}

// TestRecoverStaleJobs_DoesNotResetPending 验证 RecoverStaleJobs 不影响 pending 状态的 job。
func TestRecoverStaleJobs_DoesNotResetPending(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 插入一个 pending job
	var jobID string
	err := db.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status)
		 VALUES ('index_document', '{}', 'pending')
		 RETURNING id`,
	).Scan(&jobID)
	if err != nil {
		t.Fatalf("插入 pending job 失败: %v", err)
	}

	repo := NewPGRepository(db)
	count, err := repo.RecoverStaleJobs(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleJobs 失败: %v", err)
	}
	// pending job 不应被恢复（只有 running 才会被恢复）
	if count != 0 {
		t.Errorf("恢复 count = %d, 期望 0（pending job 不应被恢复）", count)
	}

	// 验证 job 仍为 pending
	var status string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM jobs WHERE id = $1`,
		jobID,
	).Scan(&status)
	if err != nil {
		t.Fatalf("查询 job 状态失败: %v", err)
	}
	if status != "pending" {
		t.Errorf("job 状态应为 pending, 实际 %s", status)
	}
}

// TestRecoverStaleJobs_MultipleStaleJobs 验证 RecoverStaleJobs 一次恢复多个 stale job。
func TestRecoverStaleJobs_MultipleStaleJobs(t *testing.T) {
	db := testJobDB(t)
	ctx := context.Background()

	// 插入 3 个 stale running job
	for i := 0; i < 3; i++ {
		_, err := db.ExecContext(ctx,
			`INSERT INTO jobs (type, payload, status, locked_by, locked_at, started_at, attempt_count)
			 VALUES ('index_document', '{}', 'running',
			         '00000000-0000-0000-0000-000000000004',
			         now() - interval '15 minutes',
			         now() - interval '15 minutes', 1)`,
		)
		if err != nil {
			t.Fatalf("插入 stale running job %d 失败: %v", i, err)
		}
	}

	repo := NewPGRepository(db)
	count, err := repo.RecoverStaleJobs(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleJobs 失败: %v", err)
	}
	if count != 3 {
		t.Fatalf("恢复 count = %d, 期望 3", count)
	}
}
