-- Query-time search overrides. Index-affecting settings remain in search_profiles
-- and must use the existing rebuild/evaluation workflow.
CREATE TABLE search_profile_bindings (
    scope_key TEXT PRIMARY KEY,
    workspace_id UUID UNIQUE REFERENCES workspaces(id) ON DELETE CASCADE,
    revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0),
    draft_overrides JSONB,
    published_revision BIGINT,
    updated_by UUID REFERENCES users(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_tuning_scope CHECK (
        (workspace_id IS NULL AND scope_key = 'global') OR
        (workspace_id IS NOT NULL AND scope_key = workspace_id::text)
    ),
    CONSTRAINT chk_tuning_draft CHECK (draft_overrides IS NULL OR jsonb_typeof(draft_overrides) = 'object')
);
CREATE TABLE search_profile_overrides (
    scope_key TEXT NOT NULL REFERENCES search_profile_bindings(scope_key) ON DELETE CASCADE,
    revision BIGINT NOT NULL CHECK (revision > 0),
    base_profile_id UUID NOT NULL REFERENCES search_profiles(id),
    parameters JSONB NOT NULL CHECK (jsonb_typeof(parameters) = 'object'),
    action TEXT NOT NULL CHECK (action IN ('publish', 'rollback', 'clear')),
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope_key, revision)
);
ALTER TABLE search_profile_bindings ADD CONSTRAINT fk_tuning_published_revision
    FOREIGN KEY (scope_key, published_revision)
    REFERENCES search_profile_overrides(scope_key, revision) DEFERRABLE INITIALLY DEFERRED;
CREATE INDEX idx_search_profile_overrides_base ON search_profile_overrides(base_profile_id);
