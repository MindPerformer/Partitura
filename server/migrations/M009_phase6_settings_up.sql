-- M009: Phase6 业务服务配置持久化表。
-- 引入动机：WP3 要求 system_admin 通过 Web 管理可安全持久化的业务配置，
-- 同时不将基础设施/secret 配置写入应用数据库。

CREATE TABLE system_settings (
    id          BIGSERIAL PRIMARY KEY,
    key         VARCHAR(128) NOT NULL UNIQUE,
    value       TEXT NOT NULL DEFAULT '',
    value_type  VARCHAR(32) NOT NULL DEFAULT 'string',
    category    VARCHAR(64) NOT NULL DEFAULT 'business',
    description TEXT NOT NULL DEFAULT '',
    is_secret   BOOLEAN NOT NULL DEFAULT FALSE,
    updated_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chk_settings_value_type CHECK (value_type IN ('string', 'int', 'bool', 'float', 'duration')),
    CONSTRAINT chk_settings_category CHECK (category IN ('business', 'runtime_status', 'secret'))
);

CREATE INDEX idx_settings_category ON system_settings(category);

-- 预置可安全持久化的业务配置安全默认值。
-- 基础设施/secret 配置（DSN、密码、API key、cookie/CSRF 标识、Argon2、HTTP bind）
-- 不属于此表，仍由部署环境管理。
INSERT INTO system_settings (key, value, value_type, category, description, is_secret)
VALUES
    ('default_revision_retention_days', '7', 'int', 'business', 'workspace 默认 revision 保留天数', FALSE),
    ('default_revision_max_count', '30', 'int', 'business', 'workspace 默认 revision 最大数量', FALSE),
    ('default_max_document_size_bytes', '2097152', 'int', 'business', 'workspace 默认单文档最大字节数（2MB）', FALSE),
    ('default_embedding_provider', 'openai-compatible', 'string', 'business', '默认 embedding provider 标识', FALSE),
    ('default_embedding_model', 'qwen3-embedding', 'string', 'business', '默认 embedding 模型名', FALSE),
    ('default_embedding_dimensions', '1024', 'int', 'business', '默认 embedding 维度', FALSE),
    ('default_embedding_timeout_seconds', '30', 'int', 'business', '默认 embedding 请求超时（秒）', FALSE),
    ('default_embedding_batch_size', '32', 'int', 'business', '默认 embedding 批量大小', FALSE),
    ('default_reranker_provider', 'openai-compatible', 'string', 'business', '默认 reranker provider 标识', FALSE),
    ('default_reranker_model', 'qwen3-reranker', 'string', 'business', '默认 reranker 模型名', FALSE),
    ('default_reranker_timeout_seconds', '10', 'int', 'business', '默认 reranker 请求超时（秒）', FALSE),
    ('default_reranker_max_candidates', '20', 'int', 'business', '默认 reranker 候选数', FALSE),
    ('job_worker_enabled', 'false', 'bool', 'business', '后台 job worker 是否默认启用', FALSE),
    ('job_worker_poll_interval', '5', 'int', 'business', '后台 job worker 轮询间隔（秒）', FALSE),
    ('scheduler_enabled', 'false', 'bool', 'business', '后台 scheduler 是否默认启用', FALSE),
    ('scheduler_tick_interval', '60', 'int', 'business', '后台 scheduler tick 间隔（秒）', FALSE)
ON CONFLICT (key) DO NOTHING;
