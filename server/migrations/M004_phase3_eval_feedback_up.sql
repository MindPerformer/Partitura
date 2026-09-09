-- M004: Phase 3 评测与反馈表结构
-- 创建 evaluation_datasets, evaluation_items, evaluation_results, search_feedback, search_metrics
-- 依据：design/01-SEARCH.md §Evaluation, §Feedback;
--       design/00-MASTER.md §核心数据原则 (evaluation data)

-- ============================================================
-- evaluation_datasets：搜索评测数据集
-- 引入动机：design/01-SEARCH.md §Evaluation 要求建设 Search Evaluation Dataset
-- ============================================================
CREATE TABLE evaluation_datasets (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL UNIQUE,
    description TEXT,
    -- 数据集状态
    status      VARCHAR(20) NOT NULL DEFAULT 'active',
    -- 元数据
    created_by  UUID NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_ed_status CHECK (status IN ('active', 'archived'))
);

-- ============================================================
-- evaluation_items：评测数据条目
-- 引入动机：design/01-SEARCH.md §Evaluation 每条包含 query, expected/relevant documents,
--           relevance grade 0..3, query class
-- ============================================================
CREATE TABLE evaluation_items (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dataset_id      UUID NOT NULL REFERENCES evaluation_datasets(id) ON DELETE CASCADE,
    query           TEXT NOT NULL,
    -- 期望文档列表（JSONB: [{document_id, path, grade}]）
    expected_documents JSONB NOT NULL,
    -- 相关性等级 0..3
    relevance_grade INT NOT NULL DEFAULT 0,
    -- 查询类别
    query_class     VARCHAR(100) NOT NULL DEFAULT 'general',
    -- 元数据
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_ei_grade CHECK (relevance_grade >= 0 AND relevance_grade <= 3)
);

CREATE INDEX idx_ei_dataset ON evaluation_items(dataset_id);
CREATE INDEX idx_ei_query_class ON evaluation_items(query_class);

-- ============================================================
-- search_feedback：搜索反馈记录
-- 引入动机：design/01-SEARCH.md §Feedback 要求记录 verified evaluation,
--           explicit feedback, implicit search→read/patch 信号
-- ============================================================
CREATE TABLE search_feedback (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- 反馈类型
    feedback_type   VARCHAR(50) NOT NULL,
    -- 查询（截断保护隐私，不记录完整 query 如有隐私风险）
    query_hash      VARCHAR(64),
    -- 相关文档
    document_id     UUID REFERENCES documents(id) ON DELETE SET NULL,
    -- 反馈内容（JSONB，不含敏感信息）
    detail          JSONB,
    -- 搜索 ID（关联 search_metrics）
    search_id       VARCHAR(255),
    -- 元数据
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_sf_type CHECK (feedback_type IN ('verified', 'explicit', 'implicit'))
);

CREATE INDEX idx_sf_workspace ON search_feedback(workspace_id);
CREATE INDEX idx_sf_user ON search_feedback(user_id);
CREATE INDEX idx_sf_created ON search_feedback(created_at);

-- ============================================================
-- search_metrics：搜索指标记录
-- 引入动机：design/01-SEARCH.md §Evaluation 要求 P50/P95 latency, reranker cost
--           design/05-OPERATIONS.md §Search Maintenance 要求持续收集 search metrics
-- ============================================================
CREATE TABLE search_metrics (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id         UUID REFERENCES users(id) ON DELETE SET NULL,
    -- 搜索标识
    search_id       VARCHAR(255) NOT NULL,
    -- 使用的 profile
    profile_id      UUID REFERENCES search_profiles(id) ON DELETE SET NULL,
    -- 延迟（毫秒）
    latency_ms      INT NOT NULL,
    -- 是否使用了 reranker
    reranker_used   BOOLEAN NOT NULL DEFAULT FALSE,
    -- reranker 成本估算
    reranker_cost   REAL NOT NULL DEFAULT 0,
    -- 结果数量
    result_count    INT NOT NULL DEFAULT 0,
    -- 是否降级
    degraded        BOOLEAN NOT NULL DEFAULT FALSE,
    -- 降级原因
    degradation_reason VARCHAR(100),
    -- 元数据
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sm_workspace_created ON search_metrics(workspace_id, created_at);
CREATE INDEX idx_sm_profile ON search_metrics(profile_id);
CREATE INDEX idx_sm_search_id ON search_metrics(search_id);

-- ============================================================
-- evaluation_results：评测结果持久化
-- 引入动机：design/01-SEARCH.md §Evaluation 要求指标可查询；
--           design/04-WEB-API.md §Admin 要求 evaluation 管理 API；
--           design/05-OPERATIONS.md §Background Jobs 要求 evaluate_profile job 实际执行并持久化结果。
-- M004 定义了评测输入（datasets/items），此处补充评测输出存储，
-- 使 RunEvaluation API 和 evaluate_profile job 能将计算出的 NDCG/Recall/MRR 等指标
-- 持久化到 PG，使管理员可查询历史评测结果。
-- ============================================================
CREATE TABLE evaluation_results (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- 关联的评测数据集
    dataset_id      UUID NOT NULL REFERENCES evaluation_datasets(id) ON DELETE CASCADE,
    -- 被评测的 search profile
    profile_id      UUID NOT NULL REFERENCES search_profiles(id) ON DELETE CASCADE,
    -- 关联的 job（可追溯执行者）
    job_id          UUID REFERENCES jobs(id) ON DELETE SET NULL,
    -- 评测指标（JSONB: {ndcg_10, recall_5, recall_10, mrr, precision_10, p50_latency_ms, p95_latency_ms, reranker_cost, reranker_used, class_metrics})
    metrics         JSONB NOT NULL,
    -- 评测覆盖的 query 条目数
    item_count      INT NOT NULL DEFAULT 0,
    -- 元数据
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_er_dataset ON evaluation_results(dataset_id);
CREATE INDEX idx_er_profile ON evaluation_results(profile_id);
CREATE INDEX idx_er_created ON evaluation_results(created_at DESC);
