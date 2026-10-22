-- ABOUTME: Points tool_uses.turn_id's foreign key at turns' real key, (id, session_id), which migration 012 left it naming nothing (#122).
-- ABOUTME: Rebuilds the table carrying every id across, because tool_uses_fts is external-content keyed on tool_uses.id.

-- ---------------------------------------------------------------------------
-- What this fixes
--
-- `turn_id TEXT REFERENCES turns(id)` named a unique key until migration 012
-- re-keyed turns on (session_id, id). Since then it has named something that
-- is not unique, which SQLite will not accept as a foreign key target at all.
--
-- Nothing is broken today, and that is worth stating precisely. The clause has
-- never enforced anything: foreign key enforcement is off by default in SQLite
-- and ccvault's DSN does not turn it on, which is exactly what #87's 6,378
-- orphaned tool_uses rows cost. internal/db deletes a turn's tool uses
-- explicitly in every path that deletes a turn — see
-- deleteToolUsesOfSessionTurnsSQL — because a database file cannot require a
-- connection setting of whatever opens it, and #93 is the story of what
-- happens when correctness rests on one.
--
-- So this is a trap, not a live bug. Anything that ever enables enforcement —
-- a PRAGMA foreign_keys=ON, a tool that reads the schema and believes it, a
-- migration that assumes the reference is real — has to rebuild this table
-- first. Migration 012 declined to pay a table rebuild for a clause that does
-- nothing, and said so. This pays it, so the next person does not have to
-- discover the problem by hitting it.
--
-- The declaration becomes a table-level constraint over both columns, because
-- a call belongs to the turn with its uuid *in its own session*. One uuid in
-- two sessions is two turns (#92), and a reference naming only turn_id would
-- be ambiguous in precisely the case migration 012 made legal.
--
-- ON DELETE CASCADE is carried over as it was, and is just as inert as before.
-- It documents intent; the explicit deletes are what act.
--
-- ---------------------------------------------------------------------------
-- What it costs
--
-- **This migration needs free disk space roughly equal to the size of the
-- tool_uses table**, because the rebuild holds the new table alongside the old
-- one until the swap. On the author's archive tool_uses carries about 544 MB
-- across 333,245 rows, a third of the rows migration 012 rebuilt and about a
-- ninth of its bytes. It runs in a transaction, so running out of space rolls
-- the whole thing back and leaves the archive exactly as it was.
--
-- For calibration from the migrations that have run on that archive: 012
-- rebuilt a 1,098,229-row turns table in 20m45s with a 5.4 GB WAL peak, and
-- 014 rewrote a million timestamps in 3m26s. Measured here: see the rehearsal
-- figures in the pull request for #122.
--
-- ---------------------------------------------------------------------------
-- The rebuild
--
-- Changing a foreign key means rebuilding the table: SQLite's ALTER TABLE
-- cannot add or alter one. So this is the same recipe migration 012 used —
-- create the table with the key it should have had, copy, drop, rename,
-- re-create the indexes and triggers the drop took with it.
--
-- **id is copied explicitly, and that is the load-bearing detail.**
-- tool_uses_fts is an FTS5 external-content index declared
-- content_rowid='id': every document in it is identified by the id of the call
-- whose input and result it describes. A rebuild that let SQLite assign fresh
-- ids would leave every one of those documents describing a different call, or
-- none — and the cheap orphan check could not even see it, because it compares
-- counts and the counts would still agree.
--
-- The author's archive makes that risk concrete rather than theoretical: it
-- holds 333,245 rows whose ids run to 961,068, so the ids are sparse and a
-- re-numbering would move almost every row. Carrying them across means the
-- index still describes exactly the rows it described before, and costs
-- nothing. The alternative, INSERT INTO tool_uses_fts(tool_uses_fts)
-- VALUES('rebuild'), re-reads and re-tokenises every call in the archive, and
-- there is no reason to spend it here.
--
-- DROP TABLE does not fire delete triggers, so dropping the old table leaves
-- tool_uses_fts alone rather than tombstoning the whole index first. And the
-- triggers name tool_uses, so none of them fire while tool_uses_new is being
-- filled — the index is simply not touched by this migration.
--
-- Orphaned rows are carried across untouched. #87's rows are the only evidence
-- that defect happened, and a migration whose job is a declaration must not
-- delete data on the way past. Enabling enforcement is the decision that has
-- to deal with them, and it is a separate one.
CREATE TABLE IF NOT EXISTS tool_uses_new (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    turn_id TEXT,
    session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name TEXT NOT NULL,
    file_path TEXT,
    timestamp DATETIME NOT NULL,
    tool_use_id TEXT,
    input_json TEXT,
    input_length INTEGER,
    result_content TEXT,
    result_length INTEGER,
    result_omitted_reason TEXT,
    is_error INTEGER,
    turn_ordinal INTEGER NOT NULL DEFAULT 0,
    search_period TEXT GENERATED ALWAYS AS (
        CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'
             THEN 'ccvd' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2) || substr(timestamp, 9, 2)
                  || ' ccvym' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2)
             ELSE 'ccvymx' END
    ) VIRTUAL,
    -- The whole point of this migration: a reference that names a key which
    -- exists. turns' primary key is (session_id, id); SQLite resolves a
    -- reference's parent columns against a unique index by set rather than by
    -- position, so naming them (id, session_id) here matches that key.
    -- Verified rather than assumed: with enforcement on, an insert whose pair
    -- exists in turns is accepted and one whose pair does not is rejected, and
    -- PRAGMA foreign_key_check names exactly the rows that do not satisfy it
    -- even while enforcement is off. The column order is written to read as
    -- "the turn, in its session", which is what the pair means.
    FOREIGN KEY (turn_id, session_id) REFERENCES turns(id, session_id) ON DELETE CASCADE
);

INSERT INTO tool_uses_new (id, turn_id, session_id, tool_name, file_path, timestamp,
    tool_use_id, input_json, input_length, result_content, result_length,
    result_omitted_reason, is_error, turn_ordinal)
SELECT id, turn_id, session_id, tool_name, file_path, timestamp,
    tool_use_id, input_json, input_length, result_content, result_length,
    result_omitted_reason, is_error, turn_ordinal
FROM tool_uses;

DROP TABLE tool_uses;

ALTER TABLE tool_uses_new RENAME TO tool_uses;

-- ---------------------------------------------------------------------------
-- Re-created exactly as migrations 001, 009, 011, 012 and 013 left them.
CREATE INDEX IF NOT EXISTS idx_tool_uses_session ON tool_uses(session_id);
CREATE INDEX IF NOT EXISTS idx_tool_uses_tool_name ON tool_uses(tool_name);
CREATE INDEX IF NOT EXISTS idx_tool_uses_file_path ON tool_uses(file_path);

-- The table's key: a call is identified by the turn that issued it, that
-- turn's session, and the call's position in the turn. turn_id leads so the
-- seek internal/search makes twice per returned row — "the tool uses of this
-- turn" — and the one #141's page-bounded tool-name read makes are the seeks
-- they were.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_uses_turn_ordinal
    ON tool_uses(turn_id, session_id, turn_ordinal);

-- Not unique: the same transcript ingested under two sources legitimately
-- repeats a provider id.
CREATE INDEX IF NOT EXISTS idx_tool_uses_tool_use_id ON tool_uses(tool_use_id)
    WHERE tool_use_id IS NOT NULL;

-- The failed calls and only those, carrying the columns every reader of the
-- flag asks about next.
CREATE INDEX IF NOT EXISTS idx_tool_uses_is_error
    ON tool_uses(session_id, turn_id)
    WHERE is_error = 1;

-- Scoped to exactly the columns tool_uses_fts reads, counting timestamp for
-- the one search_period derives from: an UPDATE that leaves them alone leaves
-- the index correct, and a wider scope is what cost migration 008's backfill
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

-- ---------------------------------------------------------------------------
-- The verdict on tool_uses_fts
--
-- Not decoration, and not interchangeable with the cheap check. Comparing the
-- %_docsize count against the tool_uses count cannot see a stranded entry
-- whose id something later reused, and a rebuild that reassigned ids would
-- leave the counts agreeing exactly while every document described the wrong
-- call. internal/db says the same thing in CheckFTSIntegrity's own comment,
-- which is why `ccvault stats --strict` exists beside it.
--
-- FTS5's strict 'integrity-check' reads the index against the content table
-- and errors if they disagree, which rolls this whole migration back — the
-- right outcome, because an archive whose search index silently describes
-- other people's tool calls is worse than one still carrying a decorative
-- foreign key. This is the statement that aborted a bad run of migration 012
-- and left that archive byte-identical to its backup.
INSERT INTO tool_uses_fts(tool_uses_fts, rank) VALUES('integrity-check', 1);
