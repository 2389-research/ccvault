-- ABOUTME: Migration to add source columns for multi-tool support
-- ABOUTME: Tags existing data as 'claude-code' and indexes for efficient filtering

ALTER TABLE sessions ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';
ALTER TABLE projects ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';

-- Ensure source_files exists before altering (may be missing in partial bootstraps)
CREATE TABLE IF NOT EXISTS source_files (
    path TEXT PRIMARY KEY,
    mtime DATETIME NOT NULL,
    synced_at DATETIME NOT NULL
);
ALTER TABLE source_files ADD COLUMN source TEXT NOT NULL DEFAULT 'claude-code';

CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source);
CREATE INDEX IF NOT EXISTS idx_projects_source ON projects(source);
