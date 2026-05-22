-- ============================================================
-- polar_video schema — end-state (post-T6-A workspace migration).
--
-- Apply order:
--   1. As cluster superuser:
--        CREATE DATABASE polar_video OWNER ideamesh;
--   2. As `ideamesh` against polar_video:
--        psql -d polar_video -f scripts/migrate/video-schema.sql
--
-- Cross-DB references (no FKs across databases):
--   - owner_user_id            → resolved via dock SDK /internal/v1/users/:id
--   - workspace_id             → resolved via dock SDK /internal/v1/teams/:id
--   - default_llm_config_id    → resolved via dock SDK
--                                /internal/v1/llm-configs/:id?workspace_id=<wid>
--   - llm_config_id (per-shot) → same SDK call
--
-- All four stay TEXT/BIGINT (no FK); the polar_video tables can't
-- reference dock-owned llm_configs cross-database.
--
-- See doc/arch/database-ownership.md.
-- ============================================================

CREATE TABLE IF NOT EXISTS video_projects (
    id BIGSERIAL PRIMARY KEY,
    owner_user_id TEXT NOT NULL,
    workspace_id TEXT,
    title TEXT NOT NULL DEFAULT '',
    default_llm_config_id BIGINT,
    status TEXT NOT NULL DEFAULT 'draft',
    final_video_url TEXT NOT NULL DEFAULT '',
    final_render_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_projects_owner ON video_projects(owner_user_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_video_projects_workspace ON video_projects(workspace_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS video_shots (
    id BIGSERIAL PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES video_projects(id) ON DELETE CASCADE,
    ord INT NOT NULL DEFAULT 0,
    prompt TEXT NOT NULL DEFAULT '',
    ratio TEXT NOT NULL DEFAULT '9:16',
    duration INT NOT NULL DEFAULT 10,
    generate_audio BOOLEAN NOT NULL DEFAULT TRUE,
    watermark BOOLEAN NOT NULL DEFAULT FALSE,
    llm_config_id BIGINT,
    task_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    video_url TEXT NOT NULL DEFAULT '',
    poster_url TEXT NOT NULL DEFAULT '',
    trim_start_ms INT NOT NULL DEFAULT 0,
    trim_end_ms INT NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    submitted_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_shots_project ON video_shots(project_id, ord);
CREATE INDEX IF NOT EXISTS idx_video_shots_status ON video_shots(status);

CREATE TABLE IF NOT EXISTS video_assets (
    id BIGSERIAL PRIMARY KEY,
    project_id BIGINT NOT NULL REFERENCES video_projects(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    url TEXT NOT NULL DEFAULT '',
    file_name TEXT NOT NULL DEFAULT '',
    mime_type TEXT NOT NULL DEFAULT '',
    size BIGINT NOT NULL DEFAULT 0,
    duration_ms INT NOT NULL DEFAULT 0,
    bgm_volume REAL NOT NULL DEFAULT 0.3,
    voice_volume REAL NOT NULL DEFAULT 1.0,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_video_assets_project ON video_assets(project_id, kind);
