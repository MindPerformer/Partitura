-- M012: job/document 查询路径索引优化
-- 覆盖 job claim、stale recovery、status 过滤，以及文档列表/搜索的实际过滤条件。

-- Claim 按 created_at 选取 pending job；部分索引避免扫描其他状态。
CREATE INDEX idx_jobs_pending_created_at ON jobs (created_at)
WHERE status = 'pending';

-- RecoverStaleJobs 仅扫描 running 且按 locked_at 判断超时的 job。
CREATE INDEX idx_jobs_running_locked_at ON jobs (locked_at)
WHERE status = 'running';

-- status 过滤的列表/计数路径，created_at 与分页排序保持现有访问顺序。
CREATE INDEX idx_jobs_status_created_id ON jobs (status, created_at DESC, id);

-- 文档列表按 workspace/status 过滤并按 path 排序。
CREATE INDEX idx_documents_workspace_status_path
ON documents (workspace_id, status, path);

-- integrity/search 查询排除 archived 与 special 文档；仅索引实际候选行。
CREATE INDEX idx_documents_active_regular_workspace_path
ON documents (workspace_id, path)
WHERE status <> 'archived' AND is_special = FALSE;

-- rebuild 分页按 workspace、普通/非 archived 谓词筛选并按 id 排序；
-- 以 id 作为第二列使候选读取与 ORDER BY id 对齐。
CREATE INDEX idx_documents_rebuild_workspace_id
ON documents (workspace_id, id)
WHERE is_special = FALSE AND status <> 'archived';

-- workspace 内特殊文档按 status 的检查/计数路径。
CREATE INDEX idx_documents_workspace_status_special
ON documents (workspace_id, status, is_special);
