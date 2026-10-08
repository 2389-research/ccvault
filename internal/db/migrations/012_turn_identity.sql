-- ABOUTME: Gives turns its real identity — (session_id, id), not the uuid alone (#92).
-- ABOUTME: Rebuilds the table, re-keys tool_uses to match, and forgets the mtimes of the sessions that lost turns.

-- ---------------------------------------------------------------------------
-- What this fixes
--
-- A resumed Claude Code transcript copies the earlier session's lines
-- verbatim, uuids included. turns was keyed on the uuid alone, so when the
-- newer session was parsed its write **deleted the earlier session's rows**.
-- The earlier session kept the turn_count its own parse recorded, and the
-- difference is the overlap.
--
-- Measured on the author's archive at migration 010, 46,240 sessions and
-- 996,680 turns:
--
--   201 sessions whose turn_count disagrees with their rows
--   31,864 turns claimed and not present
--   197 of those 201 hold exactly one turn, at ordinal 0
--
-- That last line looks like a collapse and is not: the survivor is whatever
-- the newer transcript did not copy, so a session resumed from its very first
-- message keeps only its first turn. Session 873d77ef reported 1,462 turns and
-- held one; f3806a3c, the session that resumed it, holds turns from ordinal
-- 1,098 up — and 1,098 is exactly where the newer transcript stops duplicating
-- the older one. f3806a3c was then resumed in its turn and has lost its own
-- first 1,098 turns to a third session.
--
-- A turn's identity is the session it belongs to together with its uuid. One
-- uuid in two sessions is two turns, and the schema has to say so, because a
-- write path cannot be careful about a collision the key insists is the same
-- row.
--
-- Three things happen here, in this order and no other:
--
--   1. turns is rebuilt on PRIMARY KEY (session_id, id).
--   2. tool_uses is re-keyed the same way, because its key names a turn.
--   3. The sessions that lost turns have their stored mtime forgotten, so the
--      next sync re-parses exactly those transcripts and the rows come back.
--
-- What does *not* happen here is reconciling sessions.turn_count. The counter
-- is the only visible evidence anything is wrong, and a schema change cannot
-- bring back a deleted row — only a re-parse of the source can. Making the
-- counter agree with a database that is still missing the turns would erase
-- the evidence and leave the archive quietly short. Reconciliation belongs
-- after the re-parse: internal/db's ReconcileSessionTurnCounts, called at the
-- end of a sync that re-read every transcript on disk.
--
-- ---------------------------------------------------------------------------
-- What it costs
--
-- **This migration needs free disk space roughly equal to the size of the
-- turns table**, because the rebuild below holds the new table alongside the
-- old one until the swap. On the author's archive turns is 4,869 MB of a
-- 6,473 MB file, so the migration wants ~4.9 GB free. It runs in a
-- transaction, so running out of space rolls the whole thing back and leaves
-- the archive exactly as it was — but it will not apply until there is room.
--
-- Timed on a slice of the author's archive: 211 sessions, 202,245 turns, a
-- 1,373 MB turns table, carrying all 201 damaged sessions. 63 seconds
-- (67.5s for the whole `ccvault stats` invocation, against 4.3s for the same
-- command once the migration has been applied). The cost is dominated by
-- copying the turns table's bytes, so the author's full archive — 3.5x the
-- turns bytes, 4.9x the rows — should land around four to five minutes. That
-- is an extrapolation, not a measurement: the machine did not have the
-- ~6.5 GB free that copying the archive to time it would have taken.
--
-- turns_fts came through sound: FTS5's strict integrity-check passes on the
-- slice after the rebuild, and on tool_uses_fts too. The cheap %_docsize
-- comparison cannot establish that — see the note on the check at the bottom
-- of this file.
--
-- Recovery, measured over the same 201 sessions with their real transcripts:
-- 2,547 rows before, 26,609 after one ordinary `ccvault sync` — **24,062
-- turns restored** — and 1,098 turn uuids ended up held by two sessions
-- apiece, which is 1,098 rows the old key would have deleted. Four sessions
-- stayed drifted: the three whose transcripts are gone from disk, and one
-- subagent transcript that repeats 22 of its own uuids (125 lines, 103
-- distinct uuids), which this migration cannot help — within one session,
-- (session_id, id) is still one row.
-- ---------------------------------------------------------------------------

-- ---------------------------------------------------------------------------
-- A turn that names no session
--
-- PRIMARY KEY (session_id, id) makes session_id NOT NULL, which is what a
-- turn's identity requires: a turn with no session is not a turn of anything.
-- There are none on the author's archive, and the sync and merge write paths
-- cannot make one — but a hand-repaired archive or one written by some older
-- tool could, and the rebuild below would fail on it rather than skip it.
--
-- Such a row is already unreachable from every path the application exposes.
-- GetTurns, the export, the TUI and MCP all select `WHERE session_id = ?`,
-- which never matches NULL; internal/search joins sessions. The one query that
-- would return it, SearchTurns, has no callers. And there is nothing to repair
-- it from — no column on turns says which transcript the row came from.
--
-- Deleted with a DELETE rather than left out of the rebuild's SELECT, so
-- turns_ad fires and takes the row's turns_fts entry with it. Its tool uses go
-- first, so the deletion leaves no row pointing at a turn that is gone — #87
-- is what that costs.
DELETE FROM tool_uses WHERE turn_id IN (SELECT id FROM turns WHERE session_id IS NULL);

DELETE FROM turns WHERE session_id IS NULL;

-- ---------------------------------------------------------------------------
-- The rebuild
--
-- Changing a primary key means rebuilding the table: SQLite's ALTER TABLE
-- cannot add or alter one. So this is the standard recipe — create the table
-- with the key it should have had, copy, drop, rename, re-create the indexes
-- and triggers the drop took with it.
--
-- **rowid is copied explicitly, and that is the load-bearing detail.**
-- turns_fts is an FTS5 external-content index keyed on turns.rowid: every
-- document in it is identified by the rowid of the turn whose text it
-- describes. A rebuild that let SQLite assign fresh rowids would leave every
-- one of those documents describing a different turn, or none — a million
-- stranded entries, and the cheap orphan check could not even see it, because
-- it compares counts and the counts would still agree. Carrying the rowids
-- across means the index still describes exactly the rows it described before,
-- and costs nothing. The alternative, `INSERT INTO turns_fts(turns_fts)
-- VALUES('rebuild')`, re-reads and re-tokenises every turn in the archive; it
-- is what internal/sync's rebuildSearchIndex does after a --full and takes 18
-- seconds on a million turns, and there is no reason to spend it here.
--
-- DROP TABLE does not fire delete triggers, so dropping the old table leaves
-- turns_fts alone rather than tombstoning the whole index first.
--
-- The migration asserts the result rather than assuming it: the last statement
-- in this file is FTS5's strict integrity-check, which is the only thing that
-- can tell a sound external-content index from a stranded one.
CREATE TABLE IF NOT EXISTS turns_new (
    id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_id TEXT,
    type TEXT NOT NULL,
    timestamp DATETIME NOT NULL,
    content TEXT,
    raw_json TEXT,
    input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    ordinal INTEGER NOT NULL DEFAULT 0,
    search_period TEXT GENERATED ALWAYS AS (
        CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'
             THEN 'ccvd' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2) || substr(timestamp, 9, 2)
                  || ' ccvym' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2)
             ELSE 'ccvymx' END
    ) VIRTUAL,
    -- The whole point of this migration. A rowid table implements this as a
    -- unique index over the two columns plus NOT NULL on both, which is
    -- exactly the constraint #92 asks for, and it leads with session_id so
    -- "the turns of this session" seeks on it.
    PRIMARY KEY (session_id, id)
);

INSERT INTO turns_new (rowid, id, session_id, parent_id, type, timestamp, content,
    raw_json, input_tokens, output_tokens, ordinal)
SELECT rowid, id, session_id, parent_id, type, timestamp, content,
    raw_json, input_tokens, output_tokens, ordinal
FROM turns;

DROP TABLE turns;

ALTER TABLE turns_new RENAME TO turns;

-- Re-created exactly as migration 001, 008 and 010 left them.
--
-- idx_turns_session is now covered twice over — by the primary key's index and
-- by idx_turns_session_ordinal, both of which lead with session_id — and it
-- was already redundant against the second of those before this migration.
-- Kept anyway: dropping an index is a separate, measurable decision and does
-- not belong in a migration whose job is a correctness fix.
CREATE INDEX IF NOT EXISTS idx_turns_session ON turns(session_id);
CREATE INDEX IF NOT EXISTS idx_turns_timestamp ON turns(timestamp);
CREATE INDEX IF NOT EXISTS idx_turns_type ON turns(type);

-- UNIQUE is what makes a position a cursor: ordinal N of a session identifies
-- at most one turn (#29). Doubles as the index MAX(ordinal) seeks.
CREATE UNIQUE INDEX IF NOT EXISTS idx_turns_session_ordinal ON turns(session_id, ordinal);

CREATE TRIGGER IF NOT EXISTS turns_ai AFTER INSERT ON turns BEGIN
    INSERT INTO turns_fts(rowid, content, search_period)
    VALUES (new.rowid, new.content, new.search_period);
END;

CREATE TRIGGER IF NOT EXISTS turns_ad AFTER DELETE ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content, search_period)
    VALUES('delete', old.rowid, old.content, old.search_period);
END;

-- Scoped to content and timestamp for the reason migration 010 scoped it:
-- firing on every column only tombstoned and re-added byte-identical
-- documents, which cost migration 008's full-table UPDATE 377 MB of dead FTS
-- segments.
CREATE TRIGGER IF NOT EXISTS turns_au AFTER UPDATE OF content, timestamp ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content, search_period)
    VALUES('delete', old.rowid, old.content, old.search_period);
    INSERT INTO turns_fts(rowid, content, search_period)
    VALUES (new.rowid, new.content, new.search_period);
END;

-- ---------------------------------------------------------------------------
-- tool_uses follows the turn
--
-- Migration 011 keyed tool_uses on (turn_id, turn_ordinal): a call is
-- identified by the turn that issued it and its position in that turn. That
-- key assumed what this migration has just stopped being true — that a turn id
-- names one turn. Left as it was, it would say a shared turn may hold only one
-- call at each position *across the whole archive*, so writing one session's
-- copy of the turn would delete the other session's calls. #113 is the story
-- of a unique index turning a legitimate second row into a silent deletion,
-- and InsertToolUsesTx's delete-then-insert has the same effect as the REPLACE
-- it replaced if the key is wrong.
--
-- So the key gains the session, under the same index name: the columns
-- changed, what the index is for did not. turn_id stays in the lead so the
-- seek internal/search makes twice per returned row — "the tool uses of this
-- turn" — is the same seek it was, and so migration 011's reason for dropping
-- the standalone idx_tool_uses_turn_id still holds.
--
-- Creating it unique is safe without a dedup pass: the rows are already unique
-- on (turn_id, turn_ordinal), so they are unique on any superset of it.
--
-- tool_uses.turn_id still declares `REFERENCES turns(id)`, which no longer
-- names a unique key. That clause has never done anything — foreign key
-- enforcement is off by default in SQLite and ccvault's DSN does not turn it
-- on, which is what #87's 6,378 orphans cost — and internal/db deletes a
-- turn's tool uses explicitly in every path that deletes a turn. It is left in
-- place because correcting it means rebuilding tool_uses for a clause that
-- enforces nothing. Anything that ever does enable enforcement has to rebuild
-- the table and point the reference at (session_id, id).
DROP INDEX IF EXISTS idx_tool_uses_turn_ordinal;

CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_uses_turn_ordinal
    ON tool_uses(turn_id, session_id, turn_ordinal);

-- ---------------------------------------------------------------------------
-- Scheduling the re-parse
--
-- The schema fix stops more turns being deleted; it cannot bring back the ones
-- already gone. Those come back only from re-reading the transcript, and 198
-- of the author's 201 affected transcripts are still on disk.
--
-- Rather than wait for someone to think of `sync --full` — which re-reads all
-- 46,000 files to repair 201 of them — this forgets the stored mtime of
-- exactly the sessions whose counter disagrees with their rows. needsSync
-- treats a file with no stored mtime as needing a parse, so the next ordinary
-- sync re-reads precisely those transcripts. With the key fixed, re-parsing
-- the session that lost turns restores its rows and takes nothing from the
-- session that resumed it.
--
-- A session whose transcript is gone from disk keeps its drift, and should:
-- those turns are genuinely lost, and `ccvault stats` now says so rather than
-- reporting turns the archive does not hold (#91).
--
-- Deleting the source_files row rather than zeroing its mtime, because mtime
-- is NOT NULL and a zero date is a value some other reader would have to know
-- to distrust. A row's absence already means "never synced", which is exactly
-- the state wanted.
--
-- Matched on both halves of source_files' key, (path, source): two sources can
-- hold the same path, and an unqualified match on path alone would unmark the
-- other source's file too.
DELETE FROM source_files
WHERE EXISTS (
    SELECT 1 FROM sessions s
    WHERE s.source_file = source_files.path
      AND s.source = source_files.source
      AND COALESCE(s.turn_count, 0) <> (SELECT COUNT(*) FROM turns t WHERE t.session_id = s.id)
);

-- ---------------------------------------------------------------------------
-- The verdict on turns_fts
--
-- Not decoration, and not interchangeable with the cheap check. Comparing the
-- %_docsize count against the turns count cannot see a stranded entry whose
-- rowid something later reused, and a rebuild that reassigned rowids would
-- leave the counts agreeing exactly while every document described the wrong
-- turn. FTS5's strict 'integrity-check' reads the index against the content
-- table and errors if they disagree, which rolls this whole migration back —
-- the right outcome, because an archive whose search index silently describes
-- other people's conversations is worse than one still carrying #92.
INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1);
