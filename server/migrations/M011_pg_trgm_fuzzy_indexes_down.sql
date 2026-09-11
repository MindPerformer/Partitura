-- M011 down: 删除 pg_trgm 模糊搜索索引。
-- 引入动机：回退 M011 时清理为 ILIKE '%..%' 创建的 GIN trgm 索引。
-- 设计约束：不 DROP EXTENSION pg_trgm——扩展可能被其它对象依赖，
-- 且 DROP EXTENSION 不带 CASCADE 在仍有依赖时会失败；保留已安装扩展无副作用。

DROP INDEX IF EXISTS idx_workspaces_display_name_trgm;
DROP INDEX IF EXISTS idx_workspaces_name_trgm;
DROP INDEX IF EXISTS idx_users_email_trgm;
DROP INDEX IF EXISTS idx_users_username_trgm;
