-- M007: Phase 6 Operations — 周期性维护调度去重表
-- 引入动机：design/05-OPERATIONS.md §Search Maintenance 要求：
--   - 每日 integrity validation、stale/missing/orphan repair、failed-job retry
--   - 每周 Level 1 profile tuning 与 evaluation replay
--   - 按管理员 retention 设置触发 revision cleanup 与 old-index cleanup
--
-- 调度的去重和执行记录必须落在 PostgreSQL，多个 server replica 不得重复执行同一周期性工作。
-- 此表记录每次调度任务的执行事实，通过唯一约束和事务实现分布式去重。

CREATE TABLE scheduler_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- schedule_key 是调度的唯一标识，格式为 "{task_type}:{period_key}"
    -- 引入动机：多实例通过同一 schedule_key 竞争执行权，唯一约束确保仅一方成功插入。
    -- 例如 "daily_integrity:2024-01-15" 表示 2024-01-15 的每日完整性检查
    schedule_key    VARCHAR(200) NOT NULL UNIQUE,
    -- task_type 标识调度类型
    -- 引入动机：区分不同周期性任务，便于查询和监控
    task_type       VARCHAR(100) NOT NULL,
    -- period_key 标识调度周期
    -- 引入动机：同一 task_type 在不同周期可执行一次，period_key 唯一标识周期
    period_key      VARCHAR(100) NOT NULL,
    -- enqueued_job_id 记录入队的 job ID（可为 NULL 如果该周期无需入队）
    enqueued_job_id UUID,
    -- 执行元数据
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 索引：按 task_type + period_key 查询
CREATE INDEX idx_scheduler_runs_task_period ON scheduler_runs (task_type, period_key);

-- 索引：按创建时间清理旧记录
CREATE INDEX idx_scheduler_runs_created_at ON scheduler_runs (created_at);
