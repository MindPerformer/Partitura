// Package scheduler 实现基于 PostgreSQL 的周期性维护任务调度。
//
// 引入动机：design/05-OPERATIONS.md §Search Maintenance 要求：
//   - 每日 integrity validation、stale/missing/orphan repair、failed-job retry
//   - 每周 Level 1 profile tuning 与 evaluation replay
//   - 按管理员 retention 设置触发 revision cleanup 与 old-index cleanup
//
// 设计原则：
//   - 调度的去重和执行记录必须落在 PostgreSQL，多个 server replica 不得重复执行
//   - 时间触发本身可用内存 tick，但不得用内存锁作为分布式锁
//   - 不出现 XxxService/XxxManager/XxxController
//   - 不引入 Redis/Kafka
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	types "partitura/server/internal/search/types"
)

// ScheduleRepo 定义调度去重所需的数据访问接口。
// 引入动机：scheduler 需要通过 PG 唯一约束实现分布式去重，
// 接口化便于测试注入 mock。
type ScheduleRepo interface {
	// TryEnqueue 尝试为指定 schedule_key 入队一个 job。
	// 引入动机：利用 PG UNIQUE 约束实现分布式去重——
	// 多个实例同时调用，仅第一个成功插入 scheduler_runs 记录并 enqueue job，
	// 其余实例因唯一约束冲突而安全跳过。
	//
	// 参数：
	//   - ctx：请求 context
	//   - scheduleKey：调度唯一标识，格式为 "{task_type}:{period_key}"
	//   - taskType：调度类型标识
	//   - periodKey：调度周期标识
	//   - jobType：要入队的 job 类型
	//   - jobPayload：job 的 payload
	//
	// 返回入队的 job ID 和 nil 表示成功入队；
	// 返回空字符串和 nil 表示该周期已由其他实例入队（去重跳过）；
	// 返回错误表示入队失败。
	TryEnqueue(ctx context.Context, scheduleKey, taskType, periodKey, jobType string, jobPayload map[string]interface{}) (jobID string, err error)

	// CleanupOldRuns 清理过期的调度记录。
	// 引入动机：scheduler_runs 表会持续增长，需要定期清理。
	CleanupOldRuns(ctx context.Context, retentionDays int) error
}

// PGScheduleRepo 是 ScheduleRepo 的 PostgreSQL 实现。
// 引入动机：利用 PG 事务和 UNIQUE 约束实现分布式去重。
type PGScheduleRepo struct {
	db *sql.DB
}

// NewPGScheduleRepo 创建 PGScheduleRepo。
func NewPGScheduleRepo(db *sql.DB) *PGScheduleRepo {
	return &PGScheduleRepo{db: db}
}

// TryEnqueue 尝试为指定 schedule_key 入队一个 job。
// 引入动机：利用 PG 事务 + UNIQUE 约束实现多实例去重。
// 事务内先 INSERT scheduler_runs（唯一约束冲突则跳过），再 INSERT job。
func (r *PGScheduleRepo) TryEnqueue(ctx context.Context, scheduleKey, taskType, periodKey, jobType string, jobPayload map[string]interface{}) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("开启调度事务: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Error("调度事务回滚失败", "error", rbErr)
		}
	}()

	// 尝试插入 scheduler_runs 记录
	// 唯一约束冲突意味着该周期已由其他实例处理，安全跳过
	var runID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO scheduler_runs (schedule_key, task_type, period_key)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (schedule_key) DO NOTHING
		 RETURNING id`,
		scheduleKey, taskType, periodKey,
	).Scan(&runID)

	if err == sql.ErrNoRows {
		// 唯一约束冲突：该周期已由其他实例入队
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("插入 scheduler_runs: %w", err)
	}

	// 入队 job
	payloadJSON, err := jsonMarshal(jobPayload)
	if err != nil {
		return "", fmt.Errorf("序列化 job payload: %w", err)
	}

	var jobID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status) VALUES ($1, $2, 'pending') RETURNING id`,
		jobType, payloadJSON,
	).Scan(&jobID)
	if err != nil {
		return "", fmt.Errorf("入队 job: %w", err)
	}

	// 更新 scheduler_runs 记录关联的 job ID
	_, err = tx.ExecContext(ctx,
		`UPDATE scheduler_runs SET enqueued_job_id = $1 WHERE id = $2`,
		jobID, runID,
	)
	if err != nil {
		return "", fmt.Errorf("更新 scheduler_runs job_id: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("提交调度事务: %w", err)
	}

	slog.Info("调度任务已入队", "schedule_key", scheduleKey, "job_id", jobID, "job_type", jobType)
	return jobID, nil
}

// CleanupOldRuns 清理过期的调度记录。
func (r *PGScheduleRepo) CleanupOldRuns(ctx context.Context, retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM scheduler_runs WHERE created_at < now() - ($1 || ' days')::interval`,
		fmt.Sprintf("%d", retentionDays),
	)
	if err != nil {
		return fmt.Errorf("清理过期调度记录: %w", err)
	}
	return nil
}

// jsonMarshal 序列化 payload 为 JSON 字节。
// 引入动机：集中序列化逻辑，便于错误处理。
func jsonMarshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// ScheduleTask 定义一个周期性调度任务。
// 引入动机：design/05-OPERATIONS.md §Search Maintenance 定义了不同周期的维护任务，
// 每个任务有独立的类型、周期和 job payload 构造逻辑。
type ScheduleTask struct {
	// TaskType 是调度类型标识，用于 scheduler_runs 记录。
	TaskType string
	// JobType 是要入队的 job 类型。
	JobType string
	// Interval 是调度周期。
	// 引入动机：区分每日、每周等不同周期。
	Interval time.Duration
	// BuildPayload 构造 job payload。
	// 引入动机：某些 job 需要动态 payload（如当前时间、配置参数等）。
	BuildPayload func() map[string]interface{}
	// BuildPeriodKey 根据当前时间生成周期标识。
	// 引入动机：每日任务使用日期作为 period_key，确保同一天只入队一次。
	BuildPeriodKey func(now time.Time) string
}

// DailyTasks 返回每日维护任务列表。
// 引入动机：design/05-OPERATIONS.md §Search Maintenance 要求每天执行
// integrity validation、stale/missing/orphan repair、failed-job retry。
func DailyTasks() []ScheduleTask {
	return []ScheduleTask{
		{
			TaskType: "daily_integrity_repair",
			JobType:  types.JobRepairIndex,
			Interval: 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{
					"index_name": "",
				}
			},
			BuildPeriodKey: func(now time.Time) string {
				return now.Format("2006-01-02")
			},
		},
		{
			TaskType: "daily_failed_job_retry",
			JobType:  types.JobRepairIndex,
			Interval: 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{
					"index_name": "",
					"retry_dead": true,
				}
			},
			BuildPeriodKey: func(now time.Time) string {
				return now.Format("2006-01-02")
			},
		},
	}
}

// WeeklyTasks 返回每周维护任务列表。
// 引入动机：design/05-OPERATIONS.md §Search Maintenance 要求每周执行
// Level 1 profile tuning 与 evaluation replay。
func WeeklyTasks() []ScheduleTask {
	return []ScheduleTask{
		{
			TaskType: "weekly_level1_optimize",
			JobType:  types.JobOptimizeProfile,
			Interval: 7 * 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{
					"tuning_level": float64(types.TuningLevel1),
				}
			},
			BuildPeriodKey: func(now time.Time) string {
				_, week := now.ISOWeek()
				return fmt.Sprintf("%d-W%02d", now.Year(), week)
			},
		},
		{
			TaskType: "weekly_evaluation_replay",
			JobType:  types.JobEvaluateProfile,
			Interval: 7 * 24 * time.Hour,
			BuildPayload: func() map[string]interface{} {
				return map[string]interface{}{}
			},
			BuildPeriodKey: func(now time.Time) string {
				_, week := now.ISOWeek()
				return fmt.Sprintf("%d-W%02d", now.Year(), week)
			},
		},
	}
}

// AllTasks 返回全部周期性维护任务。
// 引入动机：Scheduler 启动时需要注册全部任务。
func AllTasks() []ScheduleTask {
	tasks := DailyTasks()
	tasks = append(tasks, WeeklyTasks()...)
	return tasks
}

// Scheduler 是周期性维护任务调度器。
// 引入动机：design/05-OPERATIONS.md §Search Maintenance 要求周期性执行维护任务。
// 使用内存 ticker 作为触发器，PG 唯一约束作为分布式锁实现去重。
type Scheduler struct {
	repo         ScheduleRepo
	tasks        []ScheduleTask
	tickInterval time.Duration
	stopCh       chan struct{}
	stopOnce     sync.Once
	done         chan struct{}
	nowFunc      func() time.Time
}

// NewScheduler 创建调度器。
// 引入动机：main.go 需要构造调度器并注入 PG-backed repo。
func NewScheduler(repo ScheduleRepo, tasks []ScheduleTask, tickInterval time.Duration) *Scheduler {
	if tickInterval <= 0 {
		tickInterval = 60 * time.Second
	}
	return &Scheduler{
		repo:         repo,
		tasks:        tasks,
		tickInterval: tickInterval,
		stopCh:       make(chan struct{}),
		done:         make(chan struct{}),
		nowFunc:      time.Now,
	}
}

// Start 启动调度器循环。
// 引入动机：scheduler 持续 tick，检查每个任务是否到期并尝试入队。
func (s *Scheduler) Start(ctx context.Context) {
	slog.Info("维护调度器启动", "tasks", len(s.tasks), "tick_interval", s.tickInterval)

	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	defer close(s.done)

	for {
		select {
		case <-ctx.Done():
			slog.Info("维护调度器停止")
			return
		case <-s.stopCh:
			slog.Info("维护调度器停止")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// Stop 停止调度器并等待退出。
// 幂等：多次调用安全，不会 panic。
// 引入动机：优雅关闭时需要停止调度器并等待其安全退出。
// 使用 sync.Once 确保 close(stopCh) 只执行一次，避免 close of closed channel panic。
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	<-s.done
}

// tick 执行一次调度检查。
// 引入动机：每个 tick 检查所有任务是否到期，到期则尝试入队。
// 使用 UTC 时间生成 period key，确保多实例在不同时区生成相同的 key，
// 使 PG 唯一约束能正确实现分布式去重。
func (s *Scheduler) tick(ctx context.Context) {
	now := s.nowFunc().UTC()

	for _, task := range s.tasks {
		periodKey := task.BuildPeriodKey(now)
		scheduleKey := fmt.Sprintf("%s:%s", task.TaskType, periodKey)

		payload := task.BuildPayload()

		jobID, err := s.repo.TryEnqueue(ctx, scheduleKey, task.TaskType, periodKey, task.JobType, payload)
		if err != nil {
			slog.Error("调度入队失败", "schedule_key", scheduleKey, "error", err)
			continue
		}
		if jobID != "" {
			slog.Info("调度任务已入队", "task_type", task.TaskType, "period_key", periodKey, "job_id", jobID)
		}
	}
}

// SetNowFunc 设置当前时间获取函数（用于测试）。
// 引入动机：测试需要控制时间以验证到期/未到期逻辑。
func (s *Scheduler) SetNowFunc(fn func() time.Time) {
	s.nowFunc = fn
}
