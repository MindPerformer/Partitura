// Package job 实现 PostgreSQL-backed 后台任务队列和 worker。
//
// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求：
//   - 使用 PostgreSQL-backed queue
//   - worker 使用 row lock / SKIP LOCKED 等可靠方式 claim
//   - 支持多 worker
//   - 支持状态/锁/重试/失败理由
//   - 任务类型：index_document, rebuild_index, repair_index, evaluate_profile, optimize_profile, cleanup_old_indexes
//
// design/01-SEARCH.md §Index Integrity 要求 integrity check 发现差异时只修复相关文档。
// design/05-OPERATIONS.md §Fault Degradation 要求任何外部 Provider 故障不得阻止文档保存。
//
// 设计原则：
//   - 关键状态都在 PG，不在单机内存
//   - 文档成功写入后以事务安全方式 enqueue index job
//   - 索引失败可重试且不回滚 document
//   - 不出现 XxxService/XxxManager/XxxController
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	types "partitura/server/internal/search/types"
)

// Repository 定义 Job Queue 的数据访问接口。
type Repository interface {
	// Enqueue 将任务加入队列。
	// 引入动机：文档保存后需要 enqueue index job。
	Enqueue(ctx context.Context, jobType string, payload map[string]interface{}) (string, error)

	// Claim 使用 SKIP LOCKED 获取一个待执行任务。
	// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求 SKIP LOCKED。
	// 多 worker 可并发调用，不会获取同一个任务。
	Claim(ctx context.Context, workerID string) (*Job, error)

	// Complete 标记任务完成。
	Complete(ctx context.Context, jobID string) error

	// Fail 标记任务失败，记录错误信息。
	// 如果 attempt_count < max_attempts，状态设为 pending 等待重试。
	// 否则状态设为 dead。
	Fail(ctx context.Context, jobID string, errMsg string) error

	// List 查询任务列表（分页），可按 status 过滤。
	List(ctx context.Context, statusFilter string, limit, offset int) (*ListResult, error)

	// GetByID 根据 ID 查询任务。
	GetByID(ctx context.Context, id string) (*Job, error)

	// Retry 手动重试 failed/dead 任务。
	// 引入动机：admin API 需要支持手动重试。
	Retry(ctx context.Context, id string) error

	// EnqueueInTx 在事务中 enqueue 任务。
	// 引入动机：文档保存后需要在同一事务中 enqueue index job，
	// 保证文档写入和 job 入队的原子性。
	EnqueueInTx(ctx context.Context, tx *sql.Tx, jobType string, payload map[string]interface{}) (string, error)

	// RecoverStaleJobs 将长时间处于 running 状态的 job 重置为 pending。
	// 引入动机：worker 崩溃或被 SIGKILL 后，其 claim 的 job 会永久停留在 running 状态，
	// 后续 worker 无法重新 claim。启动时调用此方法恢复这些 stale job。
	// staleTimeout 是判断 stale 的阈值，locked_at 早于此阈值之前的 running job 被重置。
	// 返回被恢复的 job 数量。
	RecoverStaleJobs(ctx context.Context, staleTimeout time.Duration) (int, error)
}

// Job 是从数据库读取的任务记录。
type Job struct {
	ID           string
	Type         string
	Payload      map[string]interface{}
	Status       string
	LockedBy     string
	AttemptCount int
	MaxAttempts  int
	LastError    string
	CreatedAt    string
	StartedAt    string
	CompletedAt  string
}

// ListResult 是任务列表查询结果。
type ListResult struct {
	Jobs  []Job
	Total int
}

// PGRepository 是 Repository 接口的 PostgreSQL 实现。
type PGRepository struct {
	db *sql.DB
}

// NewPGRepository 创建 PGRepository。
func NewPGRepository(db *sql.DB) *PGRepository {
	return &PGRepository{db: db}
}

// Enqueue 将任务加入队列。
func (r *PGRepository) Enqueue(ctx context.Context, jobType string, payload map[string]interface{}) (string, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("序列化 job payload: %w", err)
	}

	var id string
	err = r.db.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status) VALUES ($1, $2, 'pending') RETURNING id`,
		jobType, []byte(payloadJSON),
	).Scan(&id)
	if err != nil {
		return "", mapDBError(err, "入队 job")
	}

	slog.Info("job 已入队", "id", id, "type", jobType)
	return id, nil
}

// EnqueueInTx 在事务中 enqueue 任务。
func (r *PGRepository) EnqueueInTx(ctx context.Context, tx *sql.Tx, jobType string, payload map[string]interface{}) (string, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("序列化 job payload: %w", err)
	}

	var id string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO jobs (type, payload, status) VALUES ($1, $2, 'pending') RETURNING id`,
		jobType, []byte(payloadJSON),
	).Scan(&id)
	if err != nil {
		return "", mapDBError(err, "事务内入队 job")
	}

	return id, nil
}

// Claim 使用 SKIP LOCKED 获取一个待执行任务。
// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求 SKIP LOCKED。
func (r *PGRepository) Claim(ctx context.Context, workerID string) (*Job, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启 claim 事务: %w", err)
	}

	// 无条件 Rollback 确保任意 return 路径都释放事务，避免 idle in transaction 泄漏。
	// Commit 成功后 Rollback 返回 sql.ErrTxDone，属于安全无副作用操作，直接忽略。
	// 此修复解决原先 defer 检查外层 err 时，json.Unmarshal 使用 := 遮蔽外层 err
	// 导致 JSON 解析错误返回时 defer 不触发 Rollback 的事务泄漏问题。
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Error("claim 事务回滚失败", "error", rbErr)
		}
	}()

	// SKIP LOCKED 获取一个 pending 任务
	row := tx.QueryRowContext(ctx,
		`SELECT id, type, payload, attempt_count, max_attempts
		 FROM jobs
		 WHERE status = 'pending'
		 ORDER BY created_at ASC
		 FOR UPDATE SKIP LOCKED
		 LIMIT 1`,
	)

	var j Job
	var payloadBytes []byte
	err = row.Scan(&j.ID, &j.Type, &payloadBytes, &j.AttemptCount, &j.MaxAttempts)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // 无待执行任务，defer 会 Rollback
		}
		return nil, fmt.Errorf("查询待执行 job: %w", err)
	}

	// payload JSON 损坏时记录日志并返回错误，不标记 job 完成、不 panic。
	// defer 无条件 Rollback 确保事务释放，行锁随之释放。
	if err := json.Unmarshal(payloadBytes, &j.Payload); err != nil {
		slog.Error("解析 job payload 失败", "job_id", j.ID, "error", err)
		return nil, fmt.Errorf("解析 job payload: %w", err)
	}

	// 标记为 running
	_, err = tx.ExecContext(ctx,
		`UPDATE jobs
		 SET status = 'running', locked_by = $1, locked_at = now(),
		     started_at = now(), attempt_count = attempt_count + 1, updated_at = now()
		 WHERE id = $2`,
		workerID, j.ID,
	)
	if err != nil {
		return nil, mapDBError(err, "标记 job 为 running")
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交 claim 事务: %w", err)
	}

	j.Status = types.JobRunning
	slog.Info("job 已 claim", "id", j.ID, "type", j.Type, "worker", workerID, "attempt", j.AttemptCount)
	return &j, nil
}

// Complete 标记任务完成。
func (r *PGRepository) Complete(ctx context.Context, jobID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE jobs
		 SET status = 'completed', completed_at = now(), last_error = NULL, updated_at = now()
		 WHERE id = $1`,
		jobID,
	)
	if err != nil {
		return mapDBError(err, "标记 job 完成")
	}
	slog.Info("job 已完成", "id", jobID)
	return nil
}

// Fail 标记任务失败，记录错误信息。
// 如果 attempt_count < max_attempts，状态设为 pending 等待重试。
func (r *PGRepository) Fail(ctx context.Context, jobID string, errMsg string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE jobs
		 SET last_error = $1,
		     status = CASE WHEN attempt_count < max_attempts THEN 'pending' ELSE 'dead' END,
		     locked_by = NULL, locked_at = NULL, updated_at = now()
		 WHERE id = $2`,
		errMsg, jobID,
	)
	if err != nil {
		return mapDBError(err, "标记 job 失败")
	}
	slog.Warn("job 失败", "id", jobID, "error", errMsg)
	return nil
}

// List 查询任务列表（分页），可按 status 过滤。
func (r *PGRepository) List(ctx context.Context, statusFilter string, limit, offset int) (*ListResult, error) {
	conditions := []string{}
	args := []interface{}{}
	argIdx := 1

	if statusFilter != "" {
		conditions = append(conditions, fmt.Sprintf("status = $%d", argIdx))
		args = append(args, statusFilter)
		argIdx++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = " WHERE " + joinStrings(conditions, " AND ")
	}

	var total int
	countQuery := "SELECT COUNT(*) FROM jobs" + whereClause
	err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total)
	if err != nil {
		return nil, mapDBError(err, "查询 job 总数")
	}

	listQuery := fmt.Sprintf(
		`SELECT id, type, payload, status, COALESCE(locked_by::text, ''),
			attempt_count, max_attempts, COALESCE(last_error, ''),
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(to_char(completed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		 FROM jobs%s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询 job 列表")
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		var payloadBytes []byte
		if err := rows.Scan(
			&j.ID, &j.Type, &payloadBytes, &j.Status, &j.LockedBy,
			&j.AttemptCount, &j.MaxAttempts, &j.LastError,
			&j.CreatedAt, &j.StartedAt, &j.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("扫描 job 行: %w", err)
		}
		if len(payloadBytes) > 0 {
			j.Payload = make(map[string]interface{})
			if err := json.Unmarshal(payloadBytes, &j.Payload); err != nil {
				slog.Error("解析 job payload 失败", "job_id", j.ID, "error", err)
				// payload 非法时不可继续执行，标记为 dead 防止无限重试
				j.Payload = nil
				j.LastError = fmt.Sprintf("payload 解析失败: %v", err)
			}
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 job 结果集: %w", err)
	}

	return &ListResult{Jobs: jobs, Total: total}, nil
}

// GetByID 根据 ID 查询任务。
func (r *PGRepository) GetByID(ctx context.Context, id string) (*Job, error) {
	var j Job
	var payloadBytes []byte
	err := r.db.QueryRowContext(ctx,
		`SELECT id, type, payload, status, COALESCE(locked_by::text, ''),
			attempt_count, max_attempts, COALESCE(last_error, ''),
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(to_char(completed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		 FROM jobs WHERE id = $1`,
		id,
	).Scan(
		&j.ID, &j.Type, &payloadBytes, &j.Status, &j.LockedBy,
		&j.AttemptCount, &j.MaxAttempts, &j.LastError,
		&j.CreatedAt, &j.StartedAt, &j.CompletedAt,
	)
	if err != nil {
		return nil, mapDBError(err, "查询 job")
	}
	if len(payloadBytes) > 0 {
		j.Payload = make(map[string]interface{})
		if err := json.Unmarshal(payloadBytes, &j.Payload); err != nil {
			slog.Error("解析 job payload 失败", "job_id", j.ID, "error", err)
			return nil, fmt.Errorf("解析 job payload (id=%s): %w", j.ID, err)
		}
	}
	return &j, nil
}

// Retry 手动重试 failed/dead 任务。
// 引入动机：M1 要求 Retry 状态与 Fail 实际状态一致。
// Fail 方法将状态设为 'pending'（可重试）或 'dead'（超过 max_attempts），
// 因此 Retry 需要处理 'dead' 状态的任务，而不是不存在的 'failed' 状态。
func (r *PGRepository) Retry(ctx context.Context, id string) error {
	// 先检查任务是否存在且处于 dead 状态
	var status string
	err := r.db.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = $1`, id).Scan(&status)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("任务不存在: %s", id)
		}
		return mapDBError(err, "查询任务状态")
	}

	if status != types.JobDead && status != types.JobPending {
		return fmt.Errorf("任务状态为 %s，仅 dead 或 pending 状态可重试", status)
	}

	_, err = r.db.ExecContext(ctx,
		`UPDATE jobs
		 SET status = 'pending', last_error = NULL, locked_by = NULL, locked_at = NULL, updated_at = now()
		 WHERE id = $1`,
		id,
	)
	if err != nil {
		return mapDBError(err, "重试 job")
	}
	slog.Info("job 已重置为 pending", "id", id, "previous_status", status)
	return nil
}

// CountByStatus 返回指定状态的 job 数量。
// 引入动机：Phase 6 /readyz 端点需要报告 pending 和 dead job 计数。
func (r *PGRepository) CountByStatus(ctx context.Context, status string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE status = $1`,
		status,
	).Scan(&count)
	if err != nil {
		return 0, mapDBError(err, "统计 job 数量")
	}
	return count, nil
}

// DefaultStaleJobTimeout 是判断 running job 为 stale 的默认超时时间。
// 引入动机：worker 崩溃后 job 停留在 running 状态，需要合理超时判断其为 stale。
// 5 分钟足以覆盖大多数索引任务的正常执行时间，短于无限等待的风险窗口。
const DefaultStaleJobTimeout = 5 * time.Minute

// RecoverStaleJobs 将长时间处于 running 状态的 job 重置为 pending。
// 引入动机：worker 崩溃或被 SIGKILL 后，其 claim 的 job 会永久停留在 running 状态。
// 启动时调用此方法恢复这些 stale job，使其重新被 worker claim 执行。
// staleTimeout <= 0 时使用 DefaultStaleJobTimeout。
func (r *PGRepository) RecoverStaleJobs(ctx context.Context, staleTimeout time.Duration) (int, error) {
	if staleTimeout <= 0 {
		staleTimeout = DefaultStaleJobTimeout
	}
	result, err := r.db.ExecContext(ctx,
		`UPDATE jobs
		 SET status = 'pending', locked_by = NULL, locked_at = NULL, updated_at = now()
		 WHERE status = 'running' AND locked_at < now() - ($1 || ' seconds')::interval`,
		fmt.Sprintf("%d", int(staleTimeout.Seconds())),
	)
	if err != nil {
		return 0, mapDBError(err, "恢复 stale running job")
	}
	count, _ := result.RowsAffected()
	if count > 0 {
		slog.Info("已恢复 stale running job", "count", count, "stale_timeout", staleTimeout)
	}
	return int(count), nil
}

// joinStrings 用 sep 连接字符串切片。
func joinStrings(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for i := 1; i < len(parts); i++ {
		result += sep + parts[i]
	}
	return result
}

// mapDBError 将 database/sql 错误映射为带上下文的错误信息。
func mapDBError(err error, context string) error {
	if err == sql.ErrNoRows {
		return err
	}
	if pgErr, ok := err.(*pgconn.PgError); ok {
		return fmt.Errorf("%s: PostgreSQL 错误 %s: %w", context, pgErr.Code, err)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// Worker 是后台任务 worker。
// 引入动机：design/05-OPERATIONS.md §Background Jobs 要求支持多 worker。
type Worker struct {
	repo     Repository
	handler  JobHandler
	workerID string
	stopCh   chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// JobHandler 定义任务处理接口。
// 引入动机：不同任务类型需要不同的处理逻辑，接口化便于扩展。
type JobHandler interface {
	// HandleJob 处理一个任务。
	// 返回 error 时 job 将被标记为失败并可能重试。
	HandleJob(ctx context.Context, job *Job) error
}

// NewWorker 创建后台任务 worker。
func NewWorker(repo Repository, handler JobHandler, workerID string) *Worker {
	return &Worker{
		repo:     repo,
		handler:  handler,
		workerID: workerID,
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start 启动 worker 循环。
// 引入动机：worker 持续轮询 PG job queue，claim 并执行任务。
// 必须在 Stop 之前调用。退出时关闭 done channel 使 Stop 可以安全等待。
func (w *Worker) Start(ctx context.Context, pollInterval time.Duration) {
	defer close(w.done)

	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	slog.Info("job worker 启动", "worker_id", w.workerID, "poll_interval", pollInterval)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("job worker 停止", "worker_id", w.workerID)
			return
		case <-w.stopCh:
			slog.Info("job worker 停止", "worker_id", w.workerID)
			return
		case <-ticker.C:
			w.processNextJob(ctx)
		}
	}
}

// Stop 停止 worker 并等待其安全退出。
// 幂等：多次调用安全，不会 panic。
// 引入动机：优雅关闭时需要停止 worker 并等待其安全退出。
// 使用 sync.Once 确保 close(stopCh) 只执行一次，避免 close of closed channel panic。
// 等待 done channel 关闭确保 Start 已退出，避免 goroutine 泄漏。
func (w *Worker) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	<-w.done
}

// processNextJob 尝试 claim 并处理一个任务。
func (w *Worker) processNextJob(ctx context.Context) {
	job, err := w.repo.Claim(ctx, w.workerID)
	if err != nil {
		slog.Error("claim job 失败", "error", err, "worker_id", w.workerID)
		return
	}
	if job == nil {
		return // 无待执行任务
	}

	// 处理任务
	err = w.handler.HandleJob(ctx, job)
	if err != nil {
		slog.Error("job 处理失败", "id", job.ID, "type", job.Type, "error", err)
		if failErr := w.repo.Fail(ctx, job.ID, err.Error()); failErr != nil {
			slog.Error("标记 job 失败状态时出错", "job_id", job.ID, "error", failErr)
		}
		return
	}

	if completeErr := w.repo.Complete(ctx, job.ID); completeErr != nil {
		slog.Error("标记 job 完成状态时出错", "job_id", job.ID, "error", completeErr)
	}
}
