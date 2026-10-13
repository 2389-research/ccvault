-- ABOUTME: Rewrites every DATETIME column from Go's time.Time.String() to RFC3339 in UTC (#33).
-- ABOUTME: A storage change the two FTS indexes do not notice, because the calendar date does not move.

-- ---------------------------------------------------------------------------
-- What this fixes
--
-- Columns declared DATETIME held Go's time.Time.String():
--
--   2026-01-14 20:15:00.134 +0000 UTC
--
-- SQLite's date parser gets as far as the seconds and then meets " +0000 UTC",
-- which is not a timezone it knows, so it gives up and returns NULL rather
-- than erroring. Measured on the author's archive on 2026-10-13:
--
--   date(timestamp)  IS NULL for 1,049,544 of 1,049,544 turns
--   date(started_at) IS NULL for    47,409 of    47,409 sessions
--   date(timestamp)  IS NULL for   287,631 of   287,631 tool_uses
--
-- So every SQLite date function was dead across the whole archive. Nothing in
-- ccvault noticed, because nothing in ccvault called one — the product
-- compares and orders these columns as text, and the format happens to sort.
-- Everything *else* that touches the archive did notice, silently: this was
-- found when `GROUP BY strftime('%Y-%m', started_at)` returned a single NULL
-- bucket for a million rows.
--
-- The new rendering is internal/db's timestampLayout,
-- "2006-01-02T15:04:05.000000000Z": RFC3339, in UTC, with the nanosecond field
-- padded rather than trimmed. The comment on that constant says why each of
-- those three choices is load-bearing; the one that matters *here* is the
-- third, because time.RFC3339Nano — which issue #33 proposed — trims trailing
-- zeros, and '.' sorts before 'Z', so a whole second would sort after the
-- tenth of a second that follows it. ORDER BY t.timestamp, MIN()/MAX() over
-- the column and the date-filter predicate are all lexicographic comparisons
-- of this text.
--
-- ---------------------------------------------------------------------------
-- The coupling that shaped it
--
-- turns.search_period and tool_uses.search_period are VIRTUAL generated
-- columns that slice the calendar date out of the timestamp *text*:
--
--   CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'
--        THEN 'ccvd' || substr(timestamp,1,4) || substr(timestamp,6,2) || ...
--
-- and turns_fts and tool_uses_fts both *index* that column. A rendering that
-- moved the year, month or day out from under those substr() calls would turn
-- this into a rebuild of two indexes totalling 1.3 million documents.
--
-- RFC3339 does not move them. Characters 1-10 are "YYYY-MM-DD" in both
-- renderings and the GLOB matches both, so for every row whose UTC calendar
-- date is unchanged the generated column is byte-identical and the index
-- entries describing it stay correct. The author's archive is uniformly
-- "+0000 UTC" — verified, 1,049,544 of 1,049,544 turns and 47,409 of 47,409
-- sessions — so on that archive no date moves at all.
--
-- An archive written by a machine at another offset is a different matter: a
-- turn stamped 22:30 at -05:00 is 03:30 the *next day* in UTC, its
-- search_period changes, and the index has to be told. That is the whole
-- reason the turns and tool_uses conversions below happen in two passes rather
-- than one. The first pass handles exactly the rows whose date moves and runs
-- with the update triggers in place, so FTS5 is kept honest. The second runs
-- with turns_au and tool_uses_au dropped, because by then every remaining row
-- provably keeps its date and firing the trigger would tombstone and re-add a
-- million byte-identical documents — the churn that cost migration 008's
-- full-table UPDATE 377 MB of dead FTS segments and is why migration 010
-- scoped these triggers by column in the first place.
--
-- ---------------------------------------------------------------------------
-- What it costs
--
-- An in-place UPDATE rather than migration 012's table rebuild, because the
-- value gets *shorter* (33 characters to 30) and the primary key does not
-- change, so there is nothing a rebuild would buy and it would want a second
-- copy of the turns table on disk.
--
-- Measured, not extrapolated: on a copy of the author's 6.79 GB archive —
-- 47,409 sessions, 1,049,544 turns, 287,631 tool uses — **202 seconds**, with
-- the WAL peaking at **6.15 GB**. For comparison, migration 012 rebuilt turns
-- on the same archive in 20m45s at a 5.4 GB WAL; this is six times quicker
-- because it rewrites rows in place instead of copying a table and re-creating
-- its indexes. It wants free disk roughly equal to the size of the tables it
-- touches, which is almost the whole archive.
--
-- It runs in a transaction like every migration, so running out of disk rolls
-- the whole thing back and leaves the archive exactly as it was. On that same
-- copy, afterwards: date(timestamp) IS NULL for 0 of 1,049,544 turns, both
-- FTS indexes pass FTS5's strict integrity-check, every search_period value
-- is byte-identical to its pre-migration value, and no row of turns sorts out
-- of chronological order by its text.
--
-- ---------------------------------------------------------------------------
-- The conversion, read once
--
-- Spelled out at every use below, because SQLite has no way to name an
-- expression a migration can reuse. It is the same five lines every time, with
-- the column substituted. Reading it once here:
--
--   I = instr(substr(c, 20), ' ')
--
-- Character 20 is where the stored rendering stops being fixed-width: it is
-- '.' when there are fractional seconds and ' ' when there are not. So the
-- first space at or after it is the one separating the datetime from the
-- offset, and I locates it relative to position 20.
--
--   * chars 1-10 and 12-19 are the date and the time, always.
--   * chars I+20 .. I+24 are the offset, "-0500" — reassembled as "-05:00"
--     and handed to SQLite, which applies it and hands back UTC.
--   * chars 21 .. I+18 are the fractional digits, right-padded to nine.
--   * anything after the offset — the zone abbreviation, and the
--     " m=+48.861813543" monotonic reading that time.Now().String() left in
--     source_files.synced_at and sessions.source_mtime — is ignored, which is
--     #33's third bullet.
--
-- The guard beside it is deliberately narrow: a value SQLite can already read
-- (date(c) IS NOT NULL) is left alone, which is what makes this migration
-- idempotent on replay, and a value that is not the shape described above is
-- left alone rather than fed through substr arithmetic that would only
-- scramble it. Four clauses, and the fourth is the one that is easy to leave
-- out — `instr(substr(c, 20), ' ') > 0`, i.e. there really is a space before
-- the offset. Without it, a value like "2026-01-14 20:15:00+0000" (no space,
-- no colon — unreadable to SQLite, so it passes the first three clauses)
-- would have I = 0, the offset would be read as "+00:0", strftime would
-- return NULL, and the whole concatenation with it.
--
-- COALESCE(…, c) is the belt to that braces. No statement in this file can
-- write a NULL over a value, whatever shape it meets: a conversion that fails
-- leaves the column exactly as it found it, and the archive is left still
-- carrying #33 for that row rather than having lost it. Worth having in a
-- migration that rewrites every timestamp in a million-turn archive, where a
-- NOT NULL column would abort the run and a nullable one would not.

-- ---------------------------------------------------------------------------
-- Pass 1: the rows whose calendar date moves, with the triggers in place.
--
-- Zero rows on the author's archive and on any archive written entirely in
-- UTC; the statement still costs one scan of turns, which is the price of not
-- assuming every archive looks like the author's. The predicate names the
-- condition it is actually about — "the first ten characters change" — rather
-- than the offset, so a non-UTC row whose date happens not to move takes the
-- cheap path with the rest.
UPDATE turns
SET timestamp = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(timestamp, 1, 10) || 'T' || substr(timestamp, 12, 8)
        || substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 3)
        || ':' || substr(timestamp, instr(substr(timestamp, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(timestamp, 20, 1) = '.'
                   THEN substr(timestamp, 21, instr(substr(timestamp, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', timestamp)
WHERE timestamp IS NOT NULL
  AND date(timestamp) IS NULL
  AND instr(substr(timestamp, 20), ' ') > 0
  AND timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]'
  AND substr(timestamp, 1, 10) <> strftime('%Y-%m-%d',
        substr(timestamp, 1, 10) || 'T' || substr(timestamp, 12, 8)
        || substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 3)
        || ':' || substr(timestamp, instr(substr(timestamp, 20), ' ') + 23, 2));

UPDATE tool_uses
SET timestamp = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(timestamp, 1, 10) || 'T' || substr(timestamp, 12, 8)
        || substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 3)
        || ':' || substr(timestamp, instr(substr(timestamp, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(timestamp, 20, 1) = '.'
                   THEN substr(timestamp, 21, instr(substr(timestamp, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', timestamp)
WHERE timestamp IS NOT NULL
  AND date(timestamp) IS NULL
  AND instr(substr(timestamp, 20), ' ') > 0
  AND timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]'
  AND substr(timestamp, 1, 10) <> strftime('%Y-%m-%d',
        substr(timestamp, 1, 10) || 'T' || substr(timestamp, 12, 8)
        || substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 3)
        || ':' || substr(timestamp, instr(substr(timestamp, 20), ' ') + 23, 2));

-- ---------------------------------------------------------------------------
-- Pass 2: everything else, with the update triggers out of the way.
--
-- Safe only because pass 1 has already dealt with every row whose
-- search_period would change. What is left is a rewrite the generated column
-- cannot see, so the documents in turns_fts and tool_uses_fts stay exactly as
-- correct as they were — and the strict integrity-check at the bottom of this
-- file is what proves that rather than assuming it.
--
-- The drop is inside the migration's transaction, so a failure anywhere below
-- rolls the triggers back along with the data.
DROP TRIGGER IF EXISTS turns_au;

DROP TRIGGER IF EXISTS tool_uses_au;

UPDATE turns
SET timestamp = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(timestamp, 1, 10) || 'T' || substr(timestamp, 12, 8)
        || substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 3)
        || ':' || substr(timestamp, instr(substr(timestamp, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(timestamp, 20, 1) = '.'
                   THEN substr(timestamp, 21, instr(substr(timestamp, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', timestamp)
WHERE timestamp IS NOT NULL
  AND date(timestamp) IS NULL
  AND instr(substr(timestamp, 20), ' ') > 0
  AND timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

UPDATE tool_uses
SET timestamp = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(timestamp, 1, 10) || 'T' || substr(timestamp, 12, 8)
        || substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 3)
        || ':' || substr(timestamp, instr(substr(timestamp, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(timestamp, 20, 1) = '.'
                   THEN substr(timestamp, 21, instr(substr(timestamp, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', timestamp)
WHERE timestamp IS NOT NULL
  AND date(timestamp) IS NULL
  AND instr(substr(timestamp, 20), ' ') > 0
  AND timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(timestamp, instr(substr(timestamp, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

-- Re-created verbatim as migration 010 wrote them and migration 012 re-created
-- turns_au. Scoped to the columns the index holds, for the reason above.
CREATE TRIGGER IF NOT EXISTS turns_au AFTER UPDATE OF content, timestamp ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content, search_period)
    VALUES('delete', old.rowid, old.content, old.search_period);
    INSERT INTO turns_fts(rowid, content, search_period)
    VALUES (new.rowid, new.content, new.search_period);
END;

CREATE TRIGGER IF NOT EXISTS tool_uses_au AFTER UPDATE OF input_json, result_content, timestamp ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(tool_uses_fts, rowid, input_json, result_content, search_period)
    VALUES ('delete', old.id, old.input_json, old.result_content, old.search_period);
    INSERT INTO tool_uses_fts(rowid, input_json, result_content, search_period)
    VALUES (new.id, new.input_json, new.result_content, new.search_period);
END;

-- ---------------------------------------------------------------------------
-- The columns no index is built on.
--
-- sessions.started_at and sessions.ended_at are uniformly "+0000 UTC" on the
-- author's archive. sessions.source_mtime, source_files.mtime and
-- source_files.synced_at are not: they come from time.Now() and os.FileInfo on
-- the syncing machine, so they carry its local offset — "-0500 CDT" on all
-- 30,025 source_files rows — and two of the three carry the monotonic reading
-- as well. The same expression handles all of it.
--
-- Converting an mtime to UTC does not disturb incremental sync: needsSync
-- compares instants in Go (sf.ModTime.After(storedMtime)), not text, and the
-- nine fractional digits are preserved exactly, so no file looks modified
-- that was not.
UPDATE sessions
SET started_at = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(started_at, 1, 10) || 'T' || substr(started_at, 12, 8)
        || substr(started_at, instr(substr(started_at, 20), ' ') + 20, 3)
        || ':' || substr(started_at, instr(substr(started_at, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(started_at, 20, 1) = '.'
                   THEN substr(started_at, 21, instr(substr(started_at, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', started_at)
WHERE started_at IS NOT NULL
  AND date(started_at) IS NULL
  AND instr(substr(started_at, 20), ' ') > 0
  AND started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(started_at, instr(substr(started_at, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

UPDATE sessions
SET ended_at = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(ended_at, 1, 10) || 'T' || substr(ended_at, 12, 8)
        || substr(ended_at, instr(substr(ended_at, 20), ' ') + 20, 3)
        || ':' || substr(ended_at, instr(substr(ended_at, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(ended_at, 20, 1) = '.'
                   THEN substr(ended_at, 21, instr(substr(ended_at, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', ended_at)
WHERE ended_at IS NOT NULL
  AND date(ended_at) IS NULL
  AND instr(substr(ended_at, 20), ' ') > 0
  AND ended_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(ended_at, instr(substr(ended_at, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

UPDATE sessions
SET source_mtime = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(source_mtime, 1, 10) || 'T' || substr(source_mtime, 12, 8)
        || substr(source_mtime, instr(substr(source_mtime, 20), ' ') + 20, 3)
        || ':' || substr(source_mtime, instr(substr(source_mtime, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(source_mtime, 20, 1) = '.'
                   THEN substr(source_mtime, 21, instr(substr(source_mtime, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', source_mtime)
WHERE source_mtime IS NOT NULL
  AND date(source_mtime) IS NULL
  AND instr(substr(source_mtime, 20), ' ') > 0
  AND source_mtime GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(source_mtime, instr(substr(source_mtime, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

-- projects.last_activity_at is compared against itself in SQL — UpsertProject
-- keeps the later of the stored and the incoming value with a `>` — so it is
-- one of the few columns where the text comparison was always doing real work.
-- It keeps doing it: the new rendering sorts chronologically, and after this
-- statement every row is in it.
UPDATE projects
SET first_seen_at = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(first_seen_at, 1, 10) || 'T' || substr(first_seen_at, 12, 8)
        || substr(first_seen_at, instr(substr(first_seen_at, 20), ' ') + 20, 3)
        || ':' || substr(first_seen_at, instr(substr(first_seen_at, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(first_seen_at, 20, 1) = '.'
                   THEN substr(first_seen_at, 21, instr(substr(first_seen_at, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', first_seen_at)
WHERE first_seen_at IS NOT NULL
  AND date(first_seen_at) IS NULL
  AND instr(substr(first_seen_at, 20), ' ') > 0
  AND first_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(first_seen_at, instr(substr(first_seen_at, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

UPDATE projects
SET last_activity_at = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(last_activity_at, 1, 10) || 'T' || substr(last_activity_at, 12, 8)
        || substr(last_activity_at, instr(substr(last_activity_at, 20), ' ') + 20, 3)
        || ':' || substr(last_activity_at, instr(substr(last_activity_at, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(last_activity_at, 20, 1) = '.'
                   THEN substr(last_activity_at, 21, instr(substr(last_activity_at, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', last_activity_at)
WHERE last_activity_at IS NOT NULL
  AND date(last_activity_at) IS NULL
  AND instr(substr(last_activity_at, 20), ' ') > 0
  AND last_activity_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(last_activity_at, instr(substr(last_activity_at, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

UPDATE source_files
SET mtime = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(mtime, 1, 10) || 'T' || substr(mtime, 12, 8)
        || substr(mtime, instr(substr(mtime, 20), ' ') + 20, 3)
        || ':' || substr(mtime, instr(substr(mtime, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(mtime, 20, 1) = '.'
                   THEN substr(mtime, 21, instr(substr(mtime, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', mtime)
WHERE mtime IS NOT NULL
  AND date(mtime) IS NULL
  AND instr(substr(mtime, 20), ' ') > 0
  AND mtime GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(mtime, instr(substr(mtime, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

UPDATE source_files
SET synced_at = COALESCE(
    strftime('%Y-%m-%dT%H:%M:%S',
        substr(synced_at, 1, 10) || 'T' || substr(synced_at, 12, 8)
        || substr(synced_at, instr(substr(synced_at, 20), ' ') + 20, 3)
        || ':' || substr(synced_at, instr(substr(synced_at, 20), ' ') + 23, 2))
    || '.'
    || substr(CASE WHEN substr(synced_at, 20, 1) = '.'
                   THEN substr(synced_at, 21, instr(substr(synced_at, 20), ' ') - 2)
                   ELSE '' END || '000000000', 1, 9)
    || 'Z', synced_at)
WHERE synced_at IS NOT NULL
  AND date(synced_at) IS NULL
  AND instr(substr(synced_at, 20), ' ') > 0
  AND synced_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]:[0-9][0-9]*'
  AND substr(synced_at, instr(substr(synced_at, 20), ' ') + 20, 5) GLOB '[+-][0-9][0-9][0-9][0-9]';

-- ---------------------------------------------------------------------------
-- The verdict
--
-- Same last statement as migration 012, for the same reason, and it is not
-- decoration. turns_fts and tool_uses_fts are FTS5 external-content indexes:
-- the documents in them hold the search_period this migration has just
-- rewritten the source of. Comparing %_docsize counts against the base tables
-- cannot see a document that still describes the wrong day — the counts agree
-- however far the index has drifted. FTS5's strict 'integrity-check' reads
-- every indexed document against the content table and errors if they
-- disagree, which rolls this whole migration back.
--
-- That is the right outcome. An archive still carrying #33 is a nuisance; an
-- archive whose search index answers date-filtered queries with other days'
-- conversations is worse than the bug.
INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1);

INSERT INTO tool_uses_fts(tool_uses_fts, rank) VALUES('integrity-check', 1);
