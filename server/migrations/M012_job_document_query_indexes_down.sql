-- M012 回退：删除 job/document 查询路径索引。
DROP INDEX IF EXISTS idx_documents_rebuild_workspace_id;
DROP INDEX IF EXISTS idx_documents_workspace_status_special;
DROP INDEX IF EXISTS idx_documents_active_regular_workspace_path;
DROP INDEX IF EXISTS idx_documents_workspace_status_path;
DROP INDEX IF EXISTS idx_jobs_status_created_id;
DROP INDEX IF EXISTS idx_jobs_running_locked_at;
DROP INDEX IF EXISTS idx_jobs_pending_created_at;
