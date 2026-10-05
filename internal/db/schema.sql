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
    ordinal INTEGER NOT NULL DEFAULT 0,
    -- The day and month this turn belongs to, as FTS5 terms, so a date filter
    -- can prune inside turns_fts instead of after it. Derived, not stored, and
    -- sliced out of the stored text rather than read with strftime because the
    -- date filter compares that same text. 'ccvymx' for a timestamp that is
    -- not a padded date — every compiled term set includes it, so such a row
    -- stays a candidate and the timestamp predicate decides it. See migration
    -- 010 and internal/search/period.go.
    search_period TEXT GENERATED ALWAYS AS (
        CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'
             THEN 'ccvd' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2) || substr(timestamp, 9, 2)
                  || ' ccvym' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2)
             ELSE 'ccvymx' END
    ) VIRTUAL
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
    timestamp DATETIME NOT NULL,
    -- The id the source gave this call, verbatim, so it can be joined against
    -- ids recorded elsewhere. NULL when the source recorded none.
    tool_use_id TEXT,
    -- The call's arguments, stored whole and indexed.
    input_json TEXT,
    input_length INTEGER,
    -- The result text, NULL when omitted by policy or when the tool returned
    -- nothing. result_length is the size as the transcript carried it and is
    -- recorded either way; result_length IS NULL means nothing answered the
    -- call at all. See pkg/toolpayload.
    result_content TEXT,
    result_length INTEGER,
    result_omitted_reason TEXT,
    -- Same tokens as turns.search_period, from this row's own timestamp —
    -- which adapter.ToolUseFromParsed stamps with the timestamp of the turn
    -- that issued the call, the timestamp the search query filters on.
    search_period TEXT GENERATED ALWAYS AS (
        CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'
             THEN 'ccvd' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2) || substr(timestamp, 9, 2)
                  || ' ccvym' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2)
             ELSE 'ccvymx' END
    ) VIRTUAL
);

CREATE INDEX IF NOT EXISTS idx_tool_uses_session ON tool_uses(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_uses_tool_name ON tool_uses(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_uses_file_path ON tool_uses(file_path);

-- Not unique: the same transcript ingested under two sources legitimately
-- repeats a provider id, and INSERT OR REPLACE resolves a unique conflict by
-- silently deleting the conflicting row.
CREATE INDEX IF NOT EXISTS idx_tool_uses_tool_use_id ON tool_uses(tool_use_id)
    WHERE tool_use_id IS NOT NULL;

-- Full-text search virtual table.
--
-- search_period is indexed beside the content so a date filter prunes inside
-- the postings-list intersection rather than after it. Every MATCH this
-- codebase builds is column-scoped — the caller's text against content, period
-- terms against search_period — which is what keeps a turn whose text happens
-- to hold a period token out of a date filter, and a caller searching for that
-- token from matching a whole month.
CREATE VIRTUAL TABLE IF NOT EXISTS turns_fts USING fts5(
    content,
    search_period,
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
    INSERT INTO turns_fts(rowid, content, search_period)
    VALUES (new.rowid, new.content, new.search_period);
END;

CREATE TRIGGER IF NOT EXISTS turns_ad AFTER DELETE ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content, search_period)
    VALUES('delete', old.rowid, old.content, old.search_period);
END;

-- turns_au is scoped to content and timestamp: exactly the columns turns_fts
-- reads, counting the one search_period derives from. An UPDATE that leaves
-- both alone leaves the index correct, so firing on every column only
-- tombstoned and re-added byte-identical documents — which made a full-table
-- UPDATE (migration 008's ordinal backfill) cost 377 MB of dead FTS segments
-- on a million-turn archive. timestamp has to be in the list, though: the
-- period token derives from it, and an UPDATE that moved a turn in time
-- without touching its text would leave the index claiming the wrong month.
CREATE TRIGGER IF NOT EXISTS turns_au AFTER UPDATE OF content, timestamp ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content, search_period)
    VALUES('delete', old.rowid, old.content, old.search_period);
    INSERT INTO turns_fts(rowid, content, search_period)
    VALUES (new.rowid, new.content, new.search_period);
END;

-- Full-text search over the tool payloads.
--
-- Its own index rather than folded into turns_fts, because the material
-- belongs to a tool_uses row and there is no turns column it could live in
-- without duplicating it. Only the two stored columns are indexed;
-- result_content is NULL for every omitted result, so file dumps and base64
-- contribute nothing.
CREATE VIRTUAL TABLE IF NOT EXISTS tool_uses_fts USING fts5(
    input_json,
    result_content,
    search_period,
    content='tool_uses',
    content_rowid='id'
);

-- Same recursive_triggers dependency as turns_ad. Triggers are scoped to
-- exactly the columns the index reads, counting timestamp for the one
-- search_period derives from: an UPDATE that leaves them alone leaves the
-- index correct, and a wider scope is what cost migration 008's backfill
-- 377 MB of dead segments before it was narrowed.
CREATE TRIGGER IF NOT EXISTS tool_uses_ai AFTER INSERT ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(rowid, input_json, result_content, search_period)
    VALUES (new.id, new.input_json, new.result_content, new.search_period);
END;

CREATE TRIGGER IF NOT EXISTS tool_uses_ad AFTER DELETE ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(tool_uses_fts, rowid, input_json, result_content, search_period)
    VALUES ('delete', old.id, old.input_json, old.result_content, old.search_period);
END;

CREATE TRIGGER IF NOT EXISTS tool_uses_au AFTER UPDATE OF input_json, result_content, timestamp ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(tool_uses_fts, rowid, input_json, result_content, search_period)
    VALUES ('delete', old.id, old.input_json, old.result_content, old.search_period);
    INSERT INTO tool_uses_fts(rowid, input_json, result_content, search_period)
    VALUES (new.id, new.input_json, new.result_content, new.search_period);
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
