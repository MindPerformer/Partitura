-- M010: Provider 密文存储表。
-- 引入动机：计划要求 Provider API Key 以 AES-256-GCM 密文形式存储于 PostgreSQL，
-- 部署方通过 MASTER_ENCRYPTION_KEY 环境变量提供根密钥。
-- 此表仅保存密文 key 和非敏感 JSON 配置，不保存明文 API Key。

CREATE TABLE provider_secrets (
    id              BIGSERIAL PRIMARY KEY,
    provider_type   VARCHAR(32) NOT NULL UNIQUE,
    encrypted_key   TEXT NOT NULL,
    config_json     JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT chk_provider_type CHECK (provider_type IN ('embedding', 'reranker'))
);

CREATE INDEX idx_provider_secrets_type ON provider_secrets(provider_type);
