-- M011: pg_trgm 模糊搜索索引
-- 引入动机：ListAllUsersWithSystemInfo / ListMemberCandidates / ListAllWorkspaces
-- 对 users.username/email 与 workspaces.name/display_name 使用 ILIKE '%..%' 前后模糊匹配，
-- B-tree 索引无法加速该模式，数据量大时退化为全表顺序扫描。
-- pg_trgm 的 gin_trgm_ops GIN 索引可加速 ILIKE '%..%' 与 similarity 查询。
--
-- 设计约束：
--   - CREATE EXTENSION / CREATE INDEX 均使用 IF NOT EXISTS，保证重复执行迁移幂等。
--   - 索引创建不使用 CONCURRENTLY：本迁移在事务内执行（runner 每迁移一事务），
--     CREATE INDEX CONCURRENTLY 不能在事务块中运行。

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- users：管理员用户列表与成员候选搜索按 username/email 模糊匹配
CREATE INDEX IF NOT EXISTS idx_users_username_trgm ON users USING gin (username gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_users_email_trgm    ON users USING gin (email gin_trgm_ops);

-- workspaces：管理员 workspace 列表按 name/display_name 模糊匹配
CREATE INDEX IF NOT EXISTS idx_workspaces_name_trgm         ON workspaces USING gin (name gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_workspaces_display_name_trgm ON workspaces USING gin (display_name gin_trgm_ops);
