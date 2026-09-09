-- M008: Phase6 扩展 device authorization 状态，支持一次性交换后的 completed 终端状态。
-- 引入动机：Phase6 WP4 要求 DeviceAuthPoll 原子交换 token，
-- 将 authorization 转为 completed，防止重复创建 session 或多次颁发 token。

ALTER TABLE device_authorizations
    DROP CONSTRAINT chk_device_auth_status;

ALTER TABLE device_authorizations
    ADD CONSTRAINT chk_device_auth_status
        CHECK (status IN ('pending', 'authorized', 'completed', 'denied', 'expired'));
