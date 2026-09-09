-- M002: Phase 2 文档表结构
-- 创建 documents, revisions, sources
-- 依据：design/03-DOCUMENTS.md §Metadata, §Source, §Revision; design/00-MASTER.md §特殊文件, §Workspace默认目录

-- ============================================================
-- documents：项目知识文档
-- 每个 document 属于一个 workspace，通过 path 唯一标识
-- content_markdown 存储当前 Markdown 正文，是业务真相源
-- content_hash 是 content_markdown 的 SHA-256 十六进制摘要，用于乐观并发控制
-- revision_number 每次变更递增，与 content_hash 共同构成并发校验对
-- status: active / draft / archived
-- is_special: 标记 PROJECT.md / AGENTS.md 特殊文件，Phase 3 搜索排除
-- ============================================================
CREATE TABLE documents (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    path            VARCHAR(512) NOT NULL,
    title           VARCHAR(255) NOT NULL,
    type            VARCHAR(50),
    status          VARCHAR(20) NOT NULL DEFAULT 'active',
    content_markdown TEXT NOT NULL,
    content_hash    VARCHAR(64) NOT NULL,
    revision_number INT NOT NULL DEFAULT 1,
    is_special      BOOLEAN NOT NULL DEFAULT FALSE,
    created_by      UUID NOT NULL REFERENCES users(id),
    updated_by      UUID NOT NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_documents_status CHECK (status IN ('active', 'draft', 'archived')),
    CONSTRAINT chk_documents_type CHECK (
        type IS NULL OR type IN (
            'architecture', 'codebase', 'development', 'decision',
            'issue', 'roadmap', 'research', 'reference',
            'operation', 'standard', 'guide', 'other'
        )
    ),
    CONSTRAINT chk_documents_revision CHECK (revision_number >= 1),
    CONSTRAINT uq_documents_workspace_path UNIQUE (workspace_id, path)
);

CREATE INDEX idx_documents_workspace_id ON documents(workspace_id);
CREATE INDEX idx_documents_workspace_status ON documents(workspace_id, status);
CREATE INDEX idx_documents_workspace_type ON documents(workspace_id, type);
CREATE INDEX idx_documents_is_special ON documents(is_special) WHERE is_special = TRUE;

-- ============================================================
-- revisions：文档版本全量快照
-- 每次创建/更新/移动/归档/恢复产生一条 revision，保存完整 Markdown snapshot
-- 不使用长 diff chain，直接存储完整内容
-- revision_number 与 documents.revision_number 对应
-- ============================================================
CREATE TABLE revisions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id     UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    revision_number INT NOT NULL,
    path            VARCHAR(512) NOT NULL,
    title           VARCHAR(255) NOT NULL,
    content_markdown TEXT NOT NULL,
    content_hash    VARCHAR(64) NOT NULL,
    status          VARCHAR(20) NOT NULL,
    created_by      UUID NOT NULL REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_revisions_status CHECK (status IN ('active', 'draft', 'archived')),
    CONSTRAINT uq_revisions_document_revision UNIQUE (document_id, revision_number)
);

CREATE INDEX idx_revisions_document_id ON revisions(document_id);
CREATE INDEX idx_revisions_workspace_id ON revisions(workspace_id);
CREATE INDEX idx_revisions_document_created ON revisions(document_id, created_at DESC);

-- ============================================================
-- sources：文档来源元数据
-- V1 做 Document-level provenance
-- source_type: web / code / file / issue / commit / conversation / manual / other
-- source_document_id: 来源必须是同一 workspace 内的 document（可为空表示外部来源）
-- ============================================================
CREATE TABLE sources (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id         UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    source_type         VARCHAR(50) NOT NULL,
    value               VARCHAR(1024) NOT NULL,
    title               VARCHAR(255),
    retrieved_at        TIMESTAMPTZ,
    content_hash        VARCHAR(64),
    refresh_interval_days INT,
    source_document_id  UUID REFERENCES documents(id) ON DELETE SET NULL,
    created_by          UUID NOT NULL REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_sources_type CHECK (
        source_type IN ('web', 'code', 'file', 'issue', 'commit', 'conversation', 'manual', 'other')
    ),
    CONSTRAINT chk_sources_refresh_interval CHECK (refresh_interval_days IS NULL OR refresh_interval_days > 0)
);

CREATE INDEX idx_sources_document_id ON sources(document_id);
CREATE INDEX idx_sources_workspace_id ON sources(workspace_id);
