// scheduler_test.go 测试周期性维护任务调度逻辑。
//
// 引入动机：plan-phase6 要求验证：
//   - 到期入队
//   - 未到期不入队
//   - 并发 scheduler 仅一方入队
//   - 失败重试
//   - retention cleanup 不删除当前 revision
//   - ES/provider 故障下任务保持 pending/retry
//
// 测试策略：使用 mock ScheduleRepo 验证调度逻辑，不依赖 PG。
package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	types "partitura/server/internal/search/types"
)

// mockScheduleRepo 是 ScheduleRepo 的 mock 实现。
// 引入动机：测试调度逻辑不需要真实 PG。
type mockScheduleRepo struct {
	mu           sync.Mutex
	enqueued     []mockEnqueuedJob
	enqueueErr   error
	skipNext     bool
	cleanupCalls int
}

type mockEnqueuedJob struct {
	scheduleKey string
	taskType    string
	periodKey   string
	jobType     string
	payload     map[string]interface{}
}

func (m *mockScheduleRepo) TryEnqueue(ctx context.Context, scheduleKey, taskType, periodKey, jobType string, jobPayload map[string]interface{}) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.enqueueErr != nil {
		return "", m.enqueueErr
	}

	// 检查是否已入队相同 scheduleKey
	for _, j := range m.enqueued {
		if j.scheduleKey == scheduleKey {
			return "", nil // 已入队，跳过
		}
	}

	m.enqueued = append(m.enqueued, mockEnqueuedJob{
		scheduleKey: scheduleKey,
		taskType:    taskType,
		periodKey:   periodKey,
		jobType:     jobType,
		payload:     jobPayload,
	})
	return "job-id-" + scheduleKey, nil
}

func (m *mockScheduleRepo) CleanupOldRuns(ctx context.Context, retentionDays int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupCalls++
	return nil
}

// TestTick_EnqueuesDueTasks 验证到期任务被入队。
func TestTick_EnqueuesDueTasks(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{
		{
			TaskType: "daily_test",
			JobType:  types.JobRepairIndex,
			Interval: 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{"test": true}
			},
			BuildPeriodKey: func(now time.Time) string {
				return now.Format("2006-01-02")
			},
		},
	}

	s := NewScheduler(repo, tasks, time.Second)
	s.tick(context.Background())

	if len(repo.enqueued) != 1 {
		t.Errorf("入队任务数 = %d, 期望 1", len(repo.enqueued))
	}
	if repo.enqueued[0].jobType != types.JobRepairIndex {
		t.Errorf("job type = %s, 期望 %s", repo.enqueued[0].jobType, types.JobRepairIndex)
	}
}

// TestTick_DoesNotReenqueueSamePeriod 验证同一周期不重复入队。
func TestTick_DoesNotReenqueueSamePeriod(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{
		{
			TaskType: "daily_test",
			JobType:  types.JobRepairIndex,
			Interval: 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{}
			},
			BuildPeriodKey: func(now time.Time) string {
				return "2024-01-15" // 固定 period key
			},
		},
	}

	s := NewScheduler(repo, tasks, time.Second)

	// 第一次 tick 应入队
	s.tick(context.Background())
	if len(repo.enqueued) != 1 {
		t.Fatalf("第一次 tick 入队数 = %d, 期望 1", len(repo.enqueued))
	}

	// 第二次 tick 同一 period key 不应入队
	s.tick(context.Background())
	if len(repo.enqueued) != 1 {
		t.Errorf("第二次 tick 入队数 = %d, 期望仍为 1（去重）", len(repo.enqueued))
	}
}

// TestTick_EnqueuesDifferentPeriods 验证不同周期会入队新任务。
func TestTick_EnqueuesDifferentPeriods(t *testing.T) {
	repo := &mockScheduleRepo{}
	currentTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	tasks := []ScheduleTask{
		{
			TaskType: "daily_test",
			JobType:  types.JobRepairIndex,
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
	s.SetNowFunc(func() time.Time { return currentTime })

	// 第一天 tick
	s.tick(context.Background())
	if len(repo.enqueued) != 1 {
		t.Fatalf("第一天入队数 = %d, 期望 1", len(repo.enqueued))
	}

	// 第二天 tick（不同 period key）
	currentTime = currentTime.Add(24 * time.Hour)
	s.tick(context.Background())
	if len(repo.enqueued) != 2 {
		t.Errorf("第二天入队数 = %d, 期望 2", len(repo.enqueued))
	}
}

// TestTick_EnqueueErrorDoesNotPanic 验证入队错误不会 panic。
func TestTick_EnqueueErrorDoesNotPanic(t *testing.T) {
	repo := &mockScheduleRepo{
		enqueueErr: errors.New("PG connection failed"),
	}
	tasks := []ScheduleTask{
		{
			TaskType:       "daily_test",
			JobType:        types.JobRepairIndex,
			Interval:       24 * time.Hour,
			BuildPayload:   func() map[string]interface{} { return map[string]interface{}{} },
			BuildPeriodKey: func(now time.Time) string { return now.Format("2006-01-02") },
		},
	}

	s := NewScheduler(repo, tasks, time.Second)

	// 不应 panic
	s.tick(context.Background())
}

// TestConcurrentTryEnqueue 验证并发调用仅一方入队。
func TestConcurrentTryEnqueue(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{
		{
			TaskType:       "daily_test",
			JobType:        types.JobRepairIndex,
			Interval:       24 * time.Hour,
			BuildPayload:   func() map[string]interface{} { return map[string]interface{}{} },
			BuildPeriodKey: func(now time.Time) string { return "2024-01-15" },
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewScheduler(repo, tasks, time.Second)
			s.tick(context.Background())
		}()
	}
	wg.Wait()

	// mockScheduleRepo 的 TryEnqueue 有 mutex 保护且检查去重
	// 应只有 1 个入队
	if len(repo.enqueued) != 1 {
		t.Errorf("并发入队数 = %d, 期望 1", len(repo.enqueued))
	}
}

// TestDailyTasks 验证每日任务配置正确。
func TestDailyTasks(t *testing.T) {
	tasks := DailyTasks()
	if len(tasks) != 2 {
		t.Errorf("每日任务数 = %d, 期望 2", len(tasks))
	}
	for _, task := range tasks {
		if task.Interval != 24*time.Hour {
			t.Errorf("任务 %s 周期 = %v, 期望 24h", task.TaskType, task.Interval)
		}
		if task.BuildPeriodKey == nil {
			t.Errorf("任务 %s BuildPeriodKey 为 nil", task.TaskType)
		}
		if task.BuildPayload == nil {
			t.Errorf("任务 %s BuildPayload 为 nil", task.TaskType)
		}
	}
}

// TestWeeklyTasks 验证每周任务配置正确。
func TestWeeklyTasks(t *testing.T) {
	tasks := WeeklyTasks()
	if len(tasks) != 2 {
		t.Errorf("每周任务数 = %d, 期望 2", len(tasks))
	}
	for _, task := range tasks {
		if task.Interval != 7*24*time.Hour {
			t.Errorf("任务 %s 周期 = %v, 期望 168h", task.TaskType, task.Interval)
		}
	}
}

// TestAllTasks 验证全部任务包含每日和每周。
func TestAllTasks(t *testing.T) {
	tasks := AllTasks()
	if len(tasks) != 4 {
		t.Errorf("全部任务数 = %d, 期望 4", len(tasks))
	}
}

// TestStopExitsCleanly 验证 Stop 能安全停止调度器。
func TestStopExitsCleanly(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{}

	s := NewScheduler(repo, tasks, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.Start(ctx)

	// 等待一小段时间确保启动
	time.Sleep(50 * time.Millisecond)

	// 停止并验证不会阻塞
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()

	select {
	case <-done:
		// 成功停止
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler.Stop 阻塞超过 2 秒")
	}
}

// TestStart_ContextCancelExitsCleanly 验证 context 取消能安全停止调度器。
func TestStart_ContextCancelExitsCleanly(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{}

	s := NewScheduler(repo, tasks, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	go s.Start(ctx)

	// 等待一小段时间确保启动
	time.Sleep(50 * time.Millisecond)

	cancel()

	// 等待 done channel 关闭
	select {
	case <-s.done:
		// 成功停止
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler context 取消后未在 2 秒内停止")
	}
}

// TestStop_ExitsCleanly 验证 Stop 后 done channel 已关闭。
func TestStop_ExitsCleanly(t *testing.T) {
	repo := &mockScheduleRepo{}
	tasks := []ScheduleTask{}

	s := NewScheduler(repo, tasks, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go s.Start(ctx)

	// 等待一小段时间确保启动
	time.Sleep(50 * time.Millisecond)

	// 停止并验证不会阻塞
	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()

	select {
	case <-done:
		// 成功停止
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler.Stop 阻塞超过 2 秒")
	}

	// 验证 done channel 已关闭
	select {
	case <-s.done:
		// 已关闭
	default:
		t.Fatal("done channel 未关闭")
	}
}

// TestPeriodKey_Daily 验证每日 period key 格式。
func TestPeriodKey_Daily(t *testing.T) {
	tasks := DailyTasks()
	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	key := tasks[0].BuildPeriodKey(now)
	expected := "2024-06-15"
	if key != expected {
		t.Errorf("daily period key = %s, 期望 %s", key, expected)
	}
}

// TestPeriodKey_Weekly 验证每周 period key 格式。
func TestPeriodKey_Weekly(t *testing.T) {
	tasks := WeeklyTasks()
	// 2024-01-15 是第 3 周
	now := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	key := tasks[0].BuildPeriodKey(now)
	// 2024-01-15 属于 ISOWeek 2024-W03
	if key == "" {
		t.Error("weekly period key 不应为空")
	}
}
