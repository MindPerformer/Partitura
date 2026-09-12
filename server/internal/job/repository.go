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

	"github.com/google/uuid"
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
//
// 引入动机：admin handler 的 ListJobs/GetByID 直接把本结构体交给 encoding/json
// 序列化作为 API 响应体，而 web 端契约（web/types/api.ts 的 Job）使用 snake_case。
// 结构体原先没有 json tag，encoding/json 会按 Go 字段名输出（ID/AttemptCount/...），
// 前端读到的是 undefined，进而在渲染期抛异常导致索引任务页整体白屏。
// 因此这里为每个字段显式声明 json tag，与前端契约逐字段对齐；
// 注意 attempts / error 并非字段名的简单小写化，前端契约就是这两个 key。
// 不使用 omitempty：前端类型声明这些字段恒存在，缺字段会重新引入 undefined 渲染风险。
type Job struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"`
	Payload      map[string]interface{} `json:"payload"`
	Status       string                 `json:"status"`
	LockedBy     string                 `json:"locked_by"`
	AttemptCount int                    `json:"attempts"`
	MaxAttempts  int                    `json:"max_attempts"`
	LastError    string                 `json:"error"`
	CreatedAt    string                 `json:"created_at"`
	// UpdatedAt 引入动机：前端 Job 契约声明了 updated_at（jobs 表有
	// updated_at TIMESTAMPTZ NOT NULL DEFAULT now()），索引任务页需要展示最近更新时间，
	// 由 List / GetByID 两个查询一并选出。
	UpdatedAt   string `json:"updated_at"`
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
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
	// 保持 string 参数以兼容现有 Repository/Worker 调用方，但在进入事务前
	// 显式校验并规范化 UUID；locked_by 列是 UUID，非法 worker ID 必须立即失败，
	// 避免把契约错误推迟到数据库执行阶段。
	parsedWorkerID, err := uuid.Parse(workerID)
	if err != nil {
		return nil, fmt.Errorf("无效 worker ID（必须是 UUID）: %w", err)
	}
	workerID = parsedWorkerID.String()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启 claim 事务: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && rbErr != sql.ErrTxDone {
			slog.Error("claim 事务回滚失败", "error", rbErr)
		}
	}()

	// 在单条 UPDATE ... FROM 语句中完成选取和状态更新，避免原先 SELECT + UPDATE 的往返。
	row := tx.QueryRowContext(ctx,
		`WITH candidate AS (
			SELECT id
			FROM jobs
			WHERE status = 'pending'
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE jobs AS j
		SET status = 'running', locked_by = $1, locked_at = now(),
		    started_at = now(), attempt_count = j.attempt_count + 1, updated_at = now()
		FROM candidate
		WHERE j.id = candidate.id
		RETURNING j.id, j.type, j.payload, j.attempt_count, j.max_attempts`,
		workerID,
	)

	var j Job
	var payloadBytes []byte
	if err = row.Scan(&j.ID, &j.Type, &payloadBytes, &j.AttemptCount, &j.MaxAttempts); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("claim 待执行 job: %w", err)
	}
	if err = json.Unmarshal(payloadBytes, &j.Payload); err != nil {
		slog.Error("解析 job payload 失败", "job_id", j.ID, "error", err)
		return nil, fmt.Errorf("解析 job payload: %w", err)
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

	listQuery := fmt.Sprintf(
		`SELECT id, type, payload, status, COALESCE(locked_by::text, ''),
			attempt_count, max_attempts, COALESCE(last_error, ''),
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(to_char(completed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COUNT(*) OVER ()
		 FROM jobs%s ORDER BY created_at DESC LIMIT $%d OFFSET $%d`,
		whereClause, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)
	var total int

	rows, err := r.db.QueryContext(ctx, listQuery, args...)
	if err != nil {
		return nil, mapDBError(err, "查询 job 列表")
	}
	defer rows.Close()

	// 修复说明：显式初始化为空 slice（而非 nil），使无任务时序列化为 [] 而不是 null。
	// nil slice 会让 API 返回 "jobs": null，前端对 null 做 .length / v-for 会抛异常，
	// 与"空列表就是空数组"的契约不符。
	jobs := make([]Job, 0)
	for rows.Next() {
		var j Job
		var payloadBytes []byte
		var rowTotal int
		if err := rows.Scan(
			&j.ID, &j.Type, &payloadBytes, &j.Status, &j.LockedBy,
			&j.AttemptCount, &j.MaxAttempts, &j.LastError,
			&j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.CompletedAt,
			&rowTotal,
		); err != nil {
			return nil, fmt.Errorf("扫描 job 行: %w", err)
		}
		if len(jobs) == 0 {
			total = rowTotal
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
	// 仅当分页窗口为空时补做 COUNT，确保 offset 超出末页时仍保持 Total 契约。
	if len(jobs) == 0 {
		countQuery := "SELECT COUNT(*) FROM jobs" + whereClause
		if err := r.db.QueryRowContext(ctx, countQuery, args[:len(args)-2]...).Scan(&total); err != nil {
			return nil, mapDBError(err, "查询 job 总数")
		}
	}

	return &ListResult{Jobs: jobs, Total: total}, nil
}

// GetByID 根据 ID 查询任务。
func (r *PGRepository) GetByID(ctx context.Context, id string) (*Job, error) {
	var j Job
	var payloadBytes []byte
	// updated_at 与 List 保持一致：前端 Job 契约声明了该字段，缺失会重新造成 undefined。
	err := r.db.QueryRowContext(ctx,
		`SELECT id, type, payload, status, COALESCE(locked_by::text, ''),
			attempt_count, max_attempts, COALESCE(last_error, ''),
			to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			COALESCE(to_char(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(to_char(completed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		 FROM jobs WHERE id = $1`,
		id,
	).Scan(
		&j.ID, &j.Type, &payloadBytes, &j.Status, &j.LockedBy,
		&j.AttemptCount, &j.MaxAttempts, &j.LastError,
		&j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.CompletedAt,
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
