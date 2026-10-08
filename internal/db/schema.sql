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
--
-- Keyed on (session_id, id), which is a turn's identity: the session it
-- belongs to together with its uuid. The uuid alone is not unique across the
-- archive — a resumed Claude Code transcript copies the earlier session's
-- lines verbatim, uuids included — so keying on it alone made the
-- later-parsed session's write delete the earlier session's rows. On the
-- author's archive that cost 31,864 turns across 201 sessions, 197 of which
-- were left holding exactly one turn apiece. See migration 012 and #92.
CREATE TABLE IF NOT EXISTS turns (
    id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
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
    ) VIRTUAL,
    PRIMARY KEY (session_id, id)
);

CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id);
CREATE INDEX IF NOT EXISTS idx_turns_timestamp ON turns(timestamp);
CREATE INDEX IF NOT EXISTS idx_turns_type ON turns(type);

-- UNIQUE is what makes a position a cursor: ordinal N of a session identifies
-- at most one turn. Doubles as the index MAX(ordinal) seeks.
CREATE UNIQUE INDEX IF NOT EXISTS idx_turns_session_ordinal ON turns(session_id, ordinal);

-- Tool uses table
--
-- Every ON DELETE CASCADE in this file, here and on turns, is inert. Foreign
-- key enforcement is off by default in SQLite and ccvault's DSN does not turn
-- it on, so these clauses document intent and enforce nothing. #87's 6,378
-- orphaned tool_uses rows are what that cost. Enabling enforcement is a
-- schema-wide decision on an archive that has never had it — PRAGMA
-- foreign_key_check on the author's 46,061-session archive reports three
-- violations already, all sessions.parent_session_id naming a parent
-- transcript the archive does not hold, which is a legitimate shape for a
-- subagent session whose parent was never synced. So internal/db deletes a
-- turn's tool uses explicitly instead, in every path that deletes a turn: a
-- database file cannot require a connection setting of whatever opens it, and
-- #93 is the story of what happens when correctness rests on one.
--
-- tool_uses.turn_id's reference is doubly inert since migration 012: turns.id
-- alone is not a unique key any more, so the clause does not even name one.
-- Correcting it means rebuilding tool_uses for a declaration that enforces
-- nothing; anything that enables enforcement has to do that and point the
-- reference at (session_id, id).
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
    -- This call's position within the turn that issued it, from 0. Half of the
    -- table's key: see idx_tool_uses_turn_ordinal below and migration 011.
    turn_ordinal INTEGER NOT NULL DEFAULT 0,
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

-- The table's key, added by migration 011 after the cleanup that made it
-- possible: a call is identified by the turn that issued it and its position in
-- that turn. Before it, tool_uses had no key at all, and two defects came out
-- of that — 1,302 surplus rows from the recovery import (#82) and 6,378 rows
-- naming a turn that no longer existed (#87).
--
-- session_id joined the key in migration 012, when a turn's identity became
-- (session_id, id). Without it, the index says a shared turn may hold only one
-- call at each position across the whole archive, so writing one session's copy
-- of the turn deletes the other session's calls.
--
-- Positional rather than keyed on the provider's id, because tool_use_id is not
-- available everywhere: jeff leaves it empty on 633 of 742 calls, and SQLite
-- treats NULLs in a unique index as distinct, so the constraint would simply
-- not apply to them. An ordinal covers every row of every source.
--
-- Also "the tool uses of this turn", which search asks twice per returned row
-- to label a payload hit with its tool and its snippet. turn_id leads the
-- index, so the seek is the same one the standalone index on turn_id used to
-- answer; migration 011 dropped that index as redundant.
--
-- Safe only because the write path does not use INSERT OR REPLACE: SQLite
-- resolves a REPLACE's unique conflict by deleting the conflicting row, so a
-- unique index plus REPLACE converts a duplicate into a silent deletion.
-- InsertToolUsesTx deletes the row holding the position itself, then plainly
-- inserts — the shape #93 moved the turns write path to.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_uses_turn_ordinal
    ON tool_uses(turn_id, session_id, turn_ordinal);

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
-- turns_ad fires on a DELETE statement under SQLite's own rules, with no
-- pragma involved, and that is what the index's correctness rests on.
--
-- It did not always. Turns used to be written with INSERT OR REPLACE, and
-- SQLite routes a REPLACE's *implicit* delete through an AFTER DELETE trigger
-- only when PRAGMA recursive_triggers is on — off by default. So a replaced
-- turn indexed its new content and left the old entry in turns_fts, matching
-- text no turns row holds any more, unless the connection had opted in. #42
-- set the pragma in ccvault's DSN; #93 established that this was not enough,
-- because the file cannot require the setting of anything else that opens it,
-- and a ccvault built before #42 was still first on the author's PATH. One
-- `sync --full` through that binary left 19,023 orphaned documents.
-- internal/db's deleteTurnConflictsSQL is the fix: an explicit DELETE of the
-- rows a turn is about to replace, then a plain INSERT. tool_uses was always
-- written that way, which is why tool_uses_fts came through the same sync
-- clean.
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

-- Triggers are scoped to exactly the columns the index reads, counting
-- timestamp for the one search_period derives from: an UPDATE that leaves
-- them alone leaves the index correct, and a wider scope is what cost
-- migration 008's backfill 377 MB of dead segments before it was narrowed.
--
-- tool_uses never needed recursive_triggers, because it is written as an
-- explicit DELETE followed by a plain INSERT rather than with INSERT OR
-- REPLACE. That is the whole reason tool_uses_fts survived the `sync --full`
-- of issue #93 that left 19,023 orphans in turns_fts, and the shape turns is
-- now written in too.
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
