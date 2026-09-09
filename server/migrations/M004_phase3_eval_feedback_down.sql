-- M004 down: 回退 Phase 3 评测与反馈表
-- 删除顺序：先删依赖表（evaluation_results → search_metrics/search_feedback → evaluation_items → evaluation_datasets）
DROP TABLE IF EXISTS evaluation_results;
DROP TABLE IF EXISTS search_metrics;
DROP TABLE IF EXISTS search_feedback;
DROP TABLE IF EXISTS evaluation_items;
DROP TABLE IF EXISTS evaluation_datasets;
