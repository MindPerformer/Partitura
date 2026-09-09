-- M010 down: 删除 provider_secrets 表。
-- 引入动机：回退 M010 迁移时清理 Provider 密文存储表。

DROP TABLE IF EXISTS provider_secrets;
