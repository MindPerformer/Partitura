-- M001: Phase 1 基础表结构
-- 创建 users, sessions, device_sessions, workspaces, workspace_members, audit_logs
-- 依据：design/00-MASTER.md §核心数据原则, design/04-WEB-API.md §RBAC, §Security

-- 启用 UUID 生成扩展
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ============================================================
-- users：用户表
-- password_hash 存储 Argon2id 格式的密码哈希，绝不存明文
-- system_role: system_admin / user
-- workspace_create_perm: 独立的 workspace 创建权限，默认 false
-- ============================================================
CREATE TABLE users (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username              VARCHAR(100) NOT NULL UNIQUE,
    email                 VARCHAR(255) NOT NULL UNIQUE,
    password_hash         TEXT NOT NULL,
    system_role           VARCHAR(20) NOT NULL DEFAULT 'user',
    workspace_create_perm BOOLEAN NOT NULL DEFAULT FALSE,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_users_system_role CHECK (system_role IN ('system_admin', 'user'))
);

-- ============================================================
-- sessions：Web cookie session
-- token_hash 存储 session token 的哈希，绝不存明文
-- csrf_token_hash 存储 CSRF token 的哈希
-- revoked_at 非空时表示已撤销
-- ============================================================
CREATE TABLE sessions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash      VARCHAR(255) NOT NULL UNIQUE,
    csrf_token_hash VARCHAR(255) NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at      TIMESTAMPTZ
);

CREATE INDEX idx_sessions_token_hash ON sessions(token_hash);
CREATE INDEX idx_sessions_user_id ON sessions(user_id);

-- ============================================================
-- device_sessions：MCP/API token session
-- access_token_hash 和 refresh_token_hash 只存哈希，绝不存明文
-- ============================================================
CREATE TABLE device_sessions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id              UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name          VARCHAR(255) NOT NULL,
    access_token_hash    VARCHAR(255) NOT NULL UNIQUE,
    refresh_token_hash   VARCHAR(255) NOT NULL UNIQUE,
    expires_at           TIMESTAMPTZ NOT NULL,
    refresh_expires_at   TIMESTAMPTZ NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at           TIMESTAMPTZ
);

CREATE INDEX idx_device_sessions_access_hash ON device_sessions(access_token_hash);
CREATE INDEX idx_device_sessions_refresh_hash ON device_sessions(refresh_token_hash);

-- ============================================================
-- workspaces：工作空间表
-- 一个 Workspace = 一个独立项目
-- revision_retention_days: revision 保留天数，默认 7
-- revision_max_count: revision 最大数量，默认 30
-- max_document_size_bytes: 单文档最大字节数，默认 2MB (2097152)
-- status: active / archived
-- ============================================================
CREATE TABLE workspaces (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                     VARCHAR(100) NOT NULL UNIQUE,
    display_name             VARCHAR(255) NOT NULL,
    description              TEXT,
    status                   VARCHAR(20) NOT NULL DEFAULT 'active',
    revision_retention_days  INT NOT NULL DEFAULT 7,
    revision_max_count       INT NOT NULL DEFAULT 30,
    max_document_size_bytes  INT NOT NULL DEFAULT 2097152,
    created_by               UUID NOT NULL REFERENCES users(id),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_workspaces_status CHECK (status IN ('active', 'archived')),
    CONSTRAINT chk_workspaces_retention_days CHECK (revision_retention_days > 0),
    CONSTRAINT chk_workspaces_max_count CHECK (revision_max_count > 0),
    CONSTRAINT chk_workspaces_max_doc_size CHECK (max_document_size_bytes > 0)
);

-- ============================================================
-- workspace_members：工作空间成员关系
-- role: owner / admin / editor / viewer
-- UNIQUE(workspace_id, user_id): 同一用户在同一 workspace 中只能有一条成员记录
-- ============================================================
CREATE TABLE workspace_members (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role         VARCHAR(20) NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_wm_role CHECK (role IN ('owner', 'admin', 'editor', 'viewer')),
    CONSTRAINT uq_wm_workspace_user UNIQUE (workspace_id, user_id)
);

CREATE INDEX idx_wm_workspace_id ON workspace_members(workspace_id);
CREATE INDEX idx_wm_user_id ON workspace_members(user_id);

-- ============================================================
-- audit_logs：审计日志
-- 使用 BIGSERIAL 自增主键，适合大量日志写入
-- user_id 和 workspace_id 可为空（如登录失败的审计）
-- detail 使用 JSONB 存储结构化详情
-- ============================================================
CREATE TABLE audit_logs (
    id            BIGSERIAL PRIMARY KEY,
    user_id       UUID,
    workspace_id  UUID,
    action        VARCHAR(100) NOT NULL,
    resource_type VARCHAR(100),
    resource_id   UUID,
    detail        JSONB,
    request_id    VARCHAR(255),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_workspace_created ON audit_logs(workspace_id, created_at);
CREATE INDEX idx_audit_user_created ON audit_logs(user_id, created_at);
