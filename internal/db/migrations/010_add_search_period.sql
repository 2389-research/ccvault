-- ABOUTME: Indexes a coarse period token beside the searchable text in both FTS tables.
-- ABOUTME: Lets a date filter prune inside the index rather than after it (issue #80).

-- ---------------------------------------------------------------------------
-- Why
--
-- Search applies its date filter in the outer WHERE, after a CTE has UNIONed
-- every all-time match from both indexes into a temp B-tree. So the cost of
-- "what did I do last week" tracks the size of the archive rather than the
-- size of the week. Measured on the author's 951,548-turn archive: `git`
-- all-time 1.90s, `git after:2026-10-01` 1.17s — the narrow query pays 62% of
-- the wide one for 0.6% of the rows.
--
-- FTS5 intersects postings lists, so an additional selective MATCH term prunes
-- inside the index, before any row reaches the UNION. This migration stores
-- the terms to intersect with: two period tokens per row, one naming its day
-- and one naming its month. internal/search compiles a date filter into the
-- set of tokens that covers the range — days at the partial edges, months
-- through the whole interior — so a week is seven terms and a year is a few
-- dozen.
--
-- The outer WHERE stays. The tokens bound the scan; the predicate is what
-- trims the partial first and last day to the hour. Correctness is the
-- predicate's job, and the tokens are chosen so they can only ever admit more
-- rows than the predicate keeps, never fewer.
-- ---------------------------------------------------------------------------

-- The period tokens for a row, derived rather than stored.
--
-- A VIRTUAL generated column costs no bytes in the table and cannot drift from
-- the timestamp it describes — there is no backfill UPDATE here at all, which
-- is what #28 measured as the expensive half of a migration like this one
-- (9.2s and 377 MB of dead FTS segments for a full-table update with triggers
-- live, against 2.2s and 20 MB for filling the index afterwards). FTS5 reads
-- the column out of the content table like any other, so `rebuild` and
-- `integrity-check` both see it.
--
-- Sliced out of the stored text instead of read with strftime, because the
-- first ten characters of a timestamp are the calendar date in every spelling
-- this archive holds or could hold — Go's `2026-10-05 13:34:02.179 +0000 UTC`,
-- which is what the driver writes today, and SQLite's own
-- `2026-10-05T13:34:02Z`. That matters beyond convenience: the date filter is
-- a lexicographic comparison of that same text, so deriving the token from the
-- text ties the token to the comparison. A row the predicate admits always
-- carries a token in the compiled set, because if its date prefix sorted below
-- the filter's the whole string would too.
--
-- GLOB guards the slice. A timestamp with an unpadded day would slice into
-- "5 " and tokenize as a term in no compiled set, which would make the row
-- vanish from every date-filtered search — a wrong answer, where this
-- migration is only meant to buy a faster one. Anything that is not a padded
-- date gets 'ccvymx' instead, which every compiled term set includes, so such
-- a row stays a candidate and the predicate decides it.
ALTER TABLE turns ADD COLUMN search_period TEXT GENERATED ALWAYS AS (
    CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'
         THEN 'ccvd' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2) || substr(timestamp, 9, 2)
              || ' ccvym' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2)
         ELSE 'ccvymx' END
) VIRTUAL;

-- The same column on tool_uses, from its own timestamp.
--
-- Its own rather than its turn's, which a generated column could not reach
-- anyway — and does not need to: adapter.ToolUseFromParsed stamps every tool
-- use with the timestamp of the turn that issued it, and that is the only way
-- a tool_uses row is ever written. Checked on the author's archive: of 257,821
-- tool uses whose turn is still present, zero disagree with it.
--
-- The search query filters on the turn's timestamp, so a tool use that ever
-- did disagree could be pruned out of a window its turn belongs to. The
-- equality above is what rules that out, and it is structural rather than
-- enforced, so it is worth stating here: a writer that stamps a tool use with
-- its own clock instead of the turn's would make payload hits go missing from
-- date-filtered searches.
ALTER TABLE tool_uses ADD COLUMN search_period TEXT GENERATED ALWAYS AS (
    CASE WHEN timestamp GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*'
         THEN 'ccvd' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2) || substr(timestamp, 9, 2)
              || ' ccvym' || substr(timestamp, 1, 4) || substr(timestamp, 6, 2)
         ELSE 'ccvymx' END
) VIRTUAL;

-- ---------------------------------------------------------------------------
-- Both indexes get the column
--
-- An FTS5 table's column list cannot be altered, so each index is dropped and
-- recreated. That is the whole cost of this migration: 951,548 turns and
-- 264,199 tool uses retokenized once.
--
-- turns_fts as well as tool_uses_fts, even though the payload index is the one
-- that showed up in the measurements. The UNION has two branches and leaving
-- one unbounded would leave the cost of a date-filtered search growing with
-- the archive — which is the property being fixed, not the absolute number.
--
-- Dropping turns_fts also has a side effect on an archive that already
-- drifted. Issue #81 reports 29,268 entries in the author's turns_fts with no
-- turns row behind them, left by the era before the turns_ad trigger's
-- dependency on recursive_triggers was understood, and a strict
-- integrity-check on that index reports "database disk image is malformed".
-- Recreating the index from the content table discards them. The repair is
-- announced rather than quiet: it changes what #81 is about.
-- ---------------------------------------------------------------------------

DROP TRIGGER IF EXISTS turns_ai;
DROP TRIGGER IF EXISTS turns_ad;
DROP TRIGGER IF EXISTS turns_au;
DROP TABLE IF EXISTS turns_fts;

CREATE VIRTUAL TABLE turns_fts USING fts5(
    content,
    search_period,
    content='turns',
    content_rowid='rowid'
);

-- turns_ad depends on PRAGMA recursive_triggers being ON, which ccvault's
-- connection DSN sets and verifyConnectionPragmas asserts. Turns are written
-- with INSERT OR REPLACE, and SQLite fires a REPLACE's implicit DELETE through
-- an AFTER DELETE trigger only when recursive triggers are enabled.
CREATE TRIGGER turns_ai AFTER INSERT ON turns BEGIN
    INSERT INTO turns_fts(rowid, content, search_period)
    VALUES (new.rowid, new.content, new.search_period);
END;

CREATE TRIGGER turns_ad AFTER DELETE ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content, search_period)
    VALUES('delete', old.rowid, old.content, old.search_period);
END;

-- Scoped to content and timestamp: exactly the columns the index now depends
-- on. timestamp is new to this list and has to be here — the period token
-- derives from it, so an UPDATE that moves a turn in time and leaves its text
-- alone would otherwise leave the index claiming the wrong month. Listing a
-- column the index does not read is what cost migration 008's backfill 377 MB
-- of dead segments, so the list stays exact in both directions.
CREATE TRIGGER turns_au AFTER UPDATE OF content, timestamp ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content, search_period)
    VALUES('delete', old.rowid, old.content, old.search_period);
    INSERT INTO turns_fts(rowid, content, search_period)
    VALUES (new.rowid, new.content, new.search_period);
END;

DROP TRIGGER IF EXISTS tool_uses_ai;
DROP TRIGGER IF EXISTS tool_uses_ad;
DROP TRIGGER IF EXISTS tool_uses_au;
DROP TABLE IF EXISTS tool_uses_fts;

CREATE VIRTUAL TABLE tool_uses_fts USING fts5(
    input_json,
    result_content,
    search_period,
    content='tool_uses',
    content_rowid='id'
);

CREATE TRIGGER tool_uses_ai AFTER INSERT ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(rowid, input_json, result_content, search_period)
    VALUES (new.id, new.input_json, new.result_content, new.search_period);
END;

CREATE TRIGGER tool_uses_ad AFTER DELETE ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(tool_uses_fts, rowid, input_json, result_content, search_period)
    VALUES ('delete', old.id, old.input_json, old.result_content, old.search_period);
END;

CREATE TRIGGER tool_uses_au AFTER UPDATE OF input_json, result_content, timestamp ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(tool_uses_fts, rowid, input_json, result_content, search_period)
    VALUES ('delete', old.id, old.input_json, old.result_content, old.search_period);
    INSERT INTO tool_uses_fts(rowid, input_json, result_content, search_period)
    VALUES (new.id, new.input_json, new.result_content, new.search_period);
END;

-- One pass per index, after the triggers exist, so a replay re-derives both
-- rather than duplicating either. Everything above this point is schema, so
-- this is where the migration's time goes.
INSERT INTO turns_fts(turns_fts) VALUES('rebuild');
INSERT INTO tool_uses_fts(tool_uses_fts) VALUES('rebuild');
