-- M002 down: 回退 Phase 2 文档表结构
-- 按依赖逆序删除

DROP TABLE IF EXISTS sources;
DROP TABLE IF EXISTS revisions;
DROP TABLE IF EXISTS documents;
