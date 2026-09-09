-- M008 down：回退 device authorization 状态约束。
-- 如果存在已完成（completed）的授权，先降级为 expired 再恢复旧约束。

UPDATE device_authorizations
SET status = 'expired', updated_at = now()
WHERE status = 'completed';

ALTER TABLE device_authorizations
    DROP CONSTRAINT chk_device_auth_status;

ALTER TABLE device_authorizations
    ADD CONSTRAINT chk_device_auth_status
        CHECK (status IN ('pending', 'authorized', 'denied', 'expired'));
