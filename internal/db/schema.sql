-- ABOUTME: Reference copy of the SQLite schema for ccvault database
-- ABOUTME: Migrations in internal/db/migrations/ are the source of truth for schema changes

-- Projects table
CREATE TABLE IF NOT EXISTS projects (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT UNIQUE NOT NULL,
    display_name TEXT NOT NULL,
    first_seen_at DATETIME,
    last_activity_at DATETIME,
    session_count INTEGER DEFAULT 0,
    total_tokens INTEGER DEFAULT 0,
    source TEXT NOT NULL DEFAULT 'claude-code'
);

CREATE INDEX IF NOT EXISTS idx_projects_path ON projects(path);
CREATE INDEX IF NOT EXISTS idx_projects_last_activity ON projects(last_activity_at DESC);
CREATE INDEX IF NOT EXISTS idx_projects_source ON projects(source);

-- Sessions table
CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    project_id INTEGER REFERENCES projects(id),
    started_at DATETIME NOT NULL,
    ended_at DATETIME,
    model TEXT,
    git_branch TEXT,
    turn_count INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    cache_write_tokens INTEGER DEFAULT 0,
    source_file TEXT NOT NULL,
    source_mtime DATETIME,
    has_error INTEGER DEFAULT 0,
    has_subagent INTEGER DEFAULT 0,
    source TEXT NOT NULL DEFAULT 'claude-code',
    parent_session_id TEXT REFERENCES sessions(id),
    -- The uuid of the turn at this session's highest ordinal. Lets a re-parse
    -- tell "resuming where I left off" from "this transcript was rewritten".
    last_entry_uuid TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_source_file ON sessions(source_file);
CREATE INDEX IF NOT EXISTS idx_sessions_source ON sessions(source);
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id)
    WHERE parent_session_id IS NOT NULL;

-- Turns table
CREATE TABLE IF NOT EXISTS turns (
    id TEXT PRIMARY KEY,
    session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
    parent_id TEXT,
    type TEXT NOT NULL,
    timestamp DATETIME NOT NULL,
    content TEXT,
    raw_json TEXT,
    input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    -- Position within the session, from 0, gapless, over every turn type.
    -- Ordering reads off this rather than timestamp, which ties and skews.
    ordinal INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id);
CREATE INDEX IF NOT EXISTS idx_turns_timestamp ON turns(timestamp);
CREATE INDEX IF NOT EXISTS idx_turns_type ON turns(type);

-- UNIQUE is what makes a position a cursor: ordinal N of a session identifies
-- at most one turn. Doubles as the index MAX(ordinal) seeks.
CREATE UNIQUE INDEX IF NOT EXISTS idx_turns_session_ordinal ON turns(session_id, ordinal);

-- Tool uses table
CREATE TABLE IF NOT EXISTS tool_uses (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id TEXT REFERENCES turns(id) ON DELETE CASCADE,
    session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL,
    file_path TEXT,
    timestamp DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tool_uses_session ON tool_uses(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_uses_tool_name ON tool_uses(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_uses_file_path ON tool_uses(file_path);

-- Full-text search virtual table
CREATE VIRTUAL TABLE IF NOT EXISTS turns_fts USING fts5(
    content,
    content='turns',
    content_rowid='rowid'
);

-- Triggers to keep FTS in sync.
--
-- turns_ad depends on PRAGMA recursive_triggers being ON, which ccvault's
-- connection DSN sets and verifyConnectionPragmas asserts. Turns are written
-- with INSERT OR REPLACE, and SQLite fires a REPLACE's implicit DELETE
-- through an AFTER DELETE trigger only when recursive triggers are enabled.
-- With the default setting a replaced turn indexes its new content and leaves
-- the old entry in turns_fts, matching text no turns row holds any more.
CREATE TRIGGER IF NOT EXISTS turns_ai AFTER INSERT ON turns BEGIN
    INSERT INTO turns_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER IF NOT EXISTS turns_ad AFTER DELETE ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;

-- turns_au is scoped to content, the only column turns_fts indexes. An UPDATE
-- that leaves content alone leaves the index correct, so firing on every
-- column only tombstoned and re-added byte-identical documents — which made a
-- full-table UPDATE (migration 008's ordinal backfill) cost 377 MB of dead
-- FTS segments on a million-turn archive.
CREATE TRIGGER IF NOT EXISTS turns_au AFTER UPDATE OF content ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO turns_fts(rowid, content) VALUES (new.rowid, new.content);
END;

-- Sync state table
CREATE TABLE IF NOT EXISTS sync_state (
    key TEXT PRIMARY KEY,
    value TEXT
);

-- Source file tracking for incremental sync (composite PK prevents cross-source collisions)
CREATE TABLE IF NOT EXISTS source_files (
    path TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'claude-code',
    mtime DATETIME NOT NULL,
    synced_at DATETIME NOT NULL,
    PRIMARY KEY (path, source)
);

-- Schema version tracking for migrations
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL,
    applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
