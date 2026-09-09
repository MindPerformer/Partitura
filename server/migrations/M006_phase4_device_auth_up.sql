-- M006: Device Authorization Flow
-- 为 MCP CLI 提供不依赖浏览器 cookie 的 device authorization 流程。
-- 引入动机：design/02-MCP.md §登录 要求 device/browser authorization 风格登录。
-- 现有 /api/auth/device/authorize 需要已认证的 cookie session + CSRF，
-- 不适合 CLI 工具直接使用。新增 start/poll 端点支持 OAuth2 Device Flow。

CREATE TABLE device_authorizations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_code     VARCHAR(255) NOT NULL UNIQUE,
    user_code       VARCHAR(32) NOT NULL UNIQUE,
    device_name     VARCHAR(255) NOT NULL,
    -- user_id 在用户批准授权后设置
    user_id         UUID REFERENCES users(id) ON DELETE CASCADE,
    -- status: pending / authorized / denied / expired
    status          VARCHAR(20) NOT NULL DEFAULT 'pending',
    expires_at      TIMESTAMPTZ NOT NULL,
    -- poll_interval_seconds: 客户端轮询间隔
    poll_interval_seconds INT NOT NULL DEFAULT 5,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_device_auth_status CHECK (status IN ('pending', 'authorized', 'denied', 'expired'))
);

CREATE INDEX idx_device_auth_device_code ON device_authorizations(device_code);
CREATE INDEX idx_device_auth_user_code ON device_authorizations(user_code);
CREATE INDEX idx_device_auth_status ON device_authorizations(status);
