-- M001: Phase 1 基础表结构 — 回退
-- 按依赖逆序删除所有表和扩展

DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS workspace_members;
DROP TABLE IF EXISTS workspaces;
DROP TABLE IF EXISTS device_sessions;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP EXTENSION IF EXISTS "pgcrypto";
