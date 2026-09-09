// reliability_test.go 测试 Scheduler 的关闭可靠性和 UTC 时间行为。
//
// 引入动机：生产可靠性要求
//   - Scheduler.Stop 幂等（不 panic on double-close）
//   - tick 使用 UTC 时间生成 period key，确保多实例分布式去重
package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestStop_IdempotentMultipleCalls 验证多次调用 Stop 不会 panic。
// 引入动机：main.go 中 serverCancel 和 Stop 可能同时触发，
// 且测试或运维可能多次调用 Stop。
func TestStop_IdempotentMultipleCalls(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{}

	s := NewScheduler(repo, tasks, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.Start(ctx)

	time.Sleep(50 * time.Millisecond)

	// 多次调用 Stop 不应 panic
	s.Stop()
	s.Stop()
	s.Stop()
}

// TestTick_UsesUTC 验证 tick 使用 UTC 时间生成 period key。
// 引入动机：多实例部署在不同时区时，period key 必须一致才能实现分布式去重。
// 如果使用本地时间，不同时区的实例可能生成不同的 period key，导致重复入队。
func TestTick_UsesUTC(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{
		{
			TaskType: "daily_utc_test",
			JobType:  "test",
			Interval: 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{}
			},
			BuildPeriodKey: func(now time.Time) string {
				return now.Format("2006-01-02")
			},
		},
	}

	s := NewScheduler(repo, tasks, time.Second)

	// 使用 UTC+5:30 时区，本地日期为 2024-06-16 01:00，但 UTC 日期为 2024-06-15 19:30
	loc := time.FixedZone("IST", 5*3600+30*60)
	localTime := time.Date(2024, 6, 16, 1, 0, 0, 0, loc)
	s.SetNowFunc(func() time.Time { return localTime })

	s.tick(context.Background())

	if len(repo.enqueued) != 1 {
		t.Fatalf("入队任务数 = %d, 期望 1", len(repo.enqueued))
	}

	// period key 应使用 UTC 日期 2024-06-15，而非本地日期 2024-06-16
	expectedKey := "daily_utc_test:2024-06-15"
	if repo.enqueued[0].scheduleKey != expectedKey {
		t.Errorf("schedule key = %s, 期望 %s (UTC date)", repo.enqueued[0].scheduleKey, expectedKey)
	}
}

// TestTick_UTCConsistentAcrossTimezones 验证不同时区的 nowFunc 生成相同的 period key。
// 引入动机：分布式部署中，不同实例可能在不同时区运行，
// UTC 转换确保它们生成相同的 period key，使 PG 唯一约束正确去重。
func TestTick_UTCConsistentAcrossTimezones(t *testing.T) {
	// 同一时刻在 UTC 和 UTC+8 两种时区下应生成相同的 period key
	utcTime := time.Date(2024, 6, 15, 20, 0, 0, 0, time.UTC)
	loc8 := time.FixedZone("CST", 8*3600)
	cstTime := time.Date(2024, 6, 16, 4, 0, 0, 0, loc8) // 2024-06-16 04:00 CST = 2024-06-15 20:00 UTC

	tasks := []ScheduleTask{
		{
			TaskType: "daily_tz_test",
			JobType:  "test",
			Interval: 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{}
			},
			BuildPeriodKey: func(now time.Time) string {
				return now.Format("2006-01-02")
			},
		},
	}

	// UTC 实例
	repoUTC := &mockScheduleRepo{}
	sUTC := NewScheduler(repoUTC, tasks, time.Second)
	sUTC.SetNowFunc(func() time.Time { return utcTime })
	sUTC.tick(context.Background())

	// CST 实例
	repoCST := &mockScheduleRepo{}
	sCST := NewScheduler(repoCST, tasks, time.Second)
	sCST.SetNowFunc(func() time.Time { return cstTime })
	sCST.tick(context.Background())

	if len(repoUTC.enqueued) != 1 || len(repoCST.enqueued) != 1 {
		t.Fatalf("UTC 入队 %d, CST 入队 %d, 期望各 1", len(repoUTC.enqueued), len(repoCST.enqueued))
	}

	if repoUTC.enqueued[0].scheduleKey != repoCST.enqueued[0].scheduleKey {
		t.Errorf("UTC 和 CST 的 schedule key 不一致: UTC=%s, CST=%s",
			repoUTC.enqueued[0].scheduleKey, repoCST.enqueued[0].scheduleKey)
	}

	// 两者都应为 2024-06-15
	expected := "daily_tz_test:2024-06-15"
	if repoUTC.enqueued[0].scheduleKey != expected {
		t.Errorf("UTC schedule key = %s, 期望 %s", repoUTC.enqueued[0].scheduleKey, expected)
	}
}
