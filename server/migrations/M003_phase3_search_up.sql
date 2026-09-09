-- M003: Phase 3 搜索基础表结构
-- 创建 search_profiles, search_profile_candidates, jobs
-- 依据：design/01-SEARCH.md §Search Profile, §Index Version, §Auto Tuning;
--       design/05-OPERATIONS.md §Background Jobs;
--       design/00-MASTER.md §核心数据原则 (jobs, search profiles)

-- ============================================================
-- search_profiles：搜索配置版本化
-- 每个 profile 包含 embedding/chunk/lexical/retrieval/reranker/diversification 全部参数
-- 版本化：同一 name 可有多个版本，通过 status 区分 active/inactive/draft
-- 引入动机：design/01-SEARCH.md §Search Profile 要求版本化，
--           §Index Version 要求 profile 变更时创建新 index + rebuild + alias 切换
-- ============================================================
CREATE TABLE search_profiles (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            VARCHAR(100) NOT NULL,
    version         INT NOT NULL DEFAULT 1,
    status          VARCHAR(20) NOT NULL DEFAULT 'draft',
    -- Embedding 配置
    embedding_provider   VARCHAR(255) NOT NULL,
    embedding_model      VARCHAR(255) NOT NULL,
    embedding_dimensions INT NOT NULL,
    embedding_query_instruction   TEXT NOT NULL DEFAULT '',
    embedding_document_instruction TEXT NOT NULL DEFAULT '',
    -- Chunk 配置
    chunk_algorithm_version VARCHAR(50) NOT NULL DEFAULT 'v1',
    chunk_target_size   INT NOT NULL DEFAULT 512,
    chunk_overlap       INT NOT NULL DEFAULT 64,
    chunk_parent_section_behavior VARCHAR(50) NOT NULL DEFAULT 'include',
    -- Lexical 配置
    lexical_title_boost   REAL NOT NULL DEFAULT 2.0,
    lexical_heading_boost REAL NOT NULL DEFAULT 1.5,
    lexical_path_boost    REAL NOT NULL DEFAULT 1.0,
    lexical_tags_boost    REAL NOT NULL DEFAULT 0.5,
    lexical_body_boost    REAL NOT NULL DEFAULT 1.0,
    lexical_analyzer      VARCHAR(50) NOT NULL DEFAULT 'standard',
    -- Retrieval 配置
    retrieval_lexical_top_k INT NOT NULL DEFAULT 50,
    retrieval_vector_top_k  INT NOT NULL DEFAULT 50,
    retrieval_rrf_k         INT NOT NULL DEFAULT 60,
    -- Reranker 配置
    reranker_provider      VARCHAR(255) NOT NULL DEFAULT '',
    reranker_model         VARCHAR(255) NOT NULL DEFAULT '',
    reranker_candidate_count INT NOT NULL DEFAULT 20,
    reranker_final_count     INT NOT NULL DEFAULT 10,
    -- Diversification 配置
    diversification_max_chunks_per_document INT NOT NULL DEFAULT 3,
    diversification_merge_adjacent_chunks   BOOLEAN NOT NULL DEFAULT TRUE,
    -- ES 索引名称（此 profile 对应的 ES index）
    es_index_name    VARCHAR(255) NOT NULL DEFAULT '',
    -- 预算与阈值
    max_p95_latency_ms INT NOT NULL DEFAULT 2000,
    max_reranker_cost_per_query REAL NOT NULL DEFAULT 0.01,
    -- 元数据
    created_by       UUID NOT NULL REFERENCES users(id),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at     TIMESTAMPTZ,
    -- 约束
    CONSTRAINT chk_sp_status CHECK (status IN ('draft', 'active', 'inactive', 'archived')),
    CONSTRAINT chk_sp_dimensions CHECK (embedding_dimensions > 0),
    CONSTRAINT chk_sp_chunk_target CHECK (chunk_target_size > 0),
    CONSTRAINT chk_sp_chunk_overlap CHECK (chunk_overlap >= 0),
    CONSTRAINT chk_sp_topk CHECK (retrieval_lexical_top_k > 0 AND retrieval_vector_top_k > 0),
    CONSTRAINT chk_sp_rrf_k CHECK (retrieval_rrf_k > 0),
    CONSTRAINT chk_sp_reranker_candidate CHECK (reranker_candidate_count > 0),
    CONSTRAINT chk_sp_reranker_final CHECK (reranker_final_count > 0),
    CONSTRAINT chk_sp_diversity CHECK (diversification_max_chunks_per_document > 0),
    CONSTRAINT uq_search_profiles_name_version UNIQUE (name, version)
);

CREATE INDEX idx_search_profiles_status ON search_profiles(status);
CREATE INDEX idx_search_profiles_name ON search_profiles(name);

-- ============================================================
-- search_profile_candidates：自动调优候选 profile
-- 引入动机：design/01-SEARCH.md §Auto Tuning 要求 Level 1 自动创建候选
--           并在通过 regression gate 后自动激活
-- ============================================================
CREATE TABLE search_profile_candidates (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    base_profile_id UUID NOT NULL REFERENCES search_profiles(id) ON DELETE CASCADE,
    candidate_profile_id UUID NOT NULL REFERENCES search_profiles(id) ON DELETE CASCADE,
    tuning_level    INT NOT NULL,
    -- 调优参数变更描述（JSONB）
    parameter_changes JSONB NOT NULL,
    -- 评测结果（JSONB）
    evaluation_result JSONB,
    -- gate 结果
    gate_passed     BOOLEAN,
    gate_details    JSONB,
    -- 状态
    status          VARCHAR(20) NOT NULL DEFAULT 'pending',
    -- 管理员确认（Level 2/3 需要）
    admin_confirmed BOOLEAN NOT NULL DEFAULT FALSE,
    admin_confirmed_by UUID REFERENCES users(id),
    admin_confirmed_at TIMESTAMPTZ,
    -- 元数据
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_spc_status CHECK (status IN ('pending', 'testing', 'passed', 'failed', 'activated', 'rejected')),
    CONSTRAINT chk_spc_level CHECK (tuning_level IN (1, 2, 3))
);

CREATE INDEX idx_spc_base_profile ON search_profile_candidates(base_profile_id);
CREATE INDEX idx_spc_status ON search_profile_candidates(status);

-- ============================================================
-- jobs：PG-backed 后台任务队列
-- 引入动机：design/05-OPERATIONS.md §Background Jobs 要求 PG-backed queue,
--           worker 使用 SKIP LOCKED claim, 支持多 worker, 可重试
-- 任务类型：index_document, rebuild_index, repair_index,
--           evaluate_profile, optimize_profile, cleanup_old_indexes, cleanup_revisions
-- 引入动机：design/05-OPERATIONS.md §Background Jobs 列出全部任务类型，
--           §Revision Cleanup 要求 cleanup_revisions 任务；
--           design/03-DOCUMENTS.md §Revision 要求定时清理历史 snapshot。
-- ============================================================
CREATE TABLE jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type            VARCHAR(50) NOT NULL,
    -- 任务参数（JSONB）
    payload         JSONB NOT NULL,
    -- 状态
    status          VARCHAR(20) NOT NULL DEFAULT 'pending',
    -- 锁定信息（多 worker SKIP LOCKED）
    locked_by       UUID,
    locked_at       TIMESTAMPTZ,
    -- 重试
    attempt_count   INT NOT NULL DEFAULT 0,
    max_attempts    INT NOT NULL DEFAULT 3,
    last_error      TEXT,
    -- 完成时间
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    -- 元数据
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- 约束
    CONSTRAINT chk_jobs_status CHECK (status IN ('pending', 'running', 'completed', 'failed', 'dead')),
    CONSTRAINT chk_jobs_type CHECK (type IN (
        'index_document', 'rebuild_index', 'repair_index',
        'evaluate_profile', 'optimize_profile', 'cleanup_old_indexes',
        'cleanup_revisions'
    )),
    CONSTRAINT chk_jobs_attempts CHECK (max_attempts > 0)
);

-- SKIP LOCKED 查询索引：按状态+创建时间排序获取待执行任务
CREATE INDEX idx_jobs_status_created ON jobs(status, created_at);
CREATE INDEX idx_jobs_type ON jobs(type);
