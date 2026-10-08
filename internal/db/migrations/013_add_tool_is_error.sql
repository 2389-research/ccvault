-- ABOUTME: Adds tool_uses.is_error — whether a call's result reported a failure — plus the partial index that makes it worth filtering on.
-- ABOUTME: Backfills from turns.raw_json in migration 009's style, so an existing archive needs no re-sync.

-- Whether the result answering this call reported a failure. Three states, all
-- of them real (#83):
--
--   NULL  nothing is known. Either no result ever answered the call, or the
--         source records no error flag at all — codex's function_call_output
--         and jeff's tool_result carry the output and say nothing about its
--         success, so a 0 there would be an assertion nobody made.
--   0     a result arrived and did not report a failure.
--   1     the result reported a failure.
--
-- This is exactly the convention migration 009 chose for result_length — NULL
-- means nothing answered the call, 0 means it answered with nothing — rather
-- than a second convention sitting beside the first in the same table.
--
-- Not derivable from result_content, which is why it has to be stored. A tool
-- that fails having printed nothing leaves no text to read a failure out of,
-- and a failing Read has its body omitted by the storage policy entirely. The
-- flag is the only thing those rows carry, and 10,909 of the author's calls
-- failed with their text already stored and only findable by guessing at the
-- wording.
--
-- Per-call, where sessions.has_error is per-session. The two answer different
-- questions — "this session held a failure" and "this call failed" — and the
-- second is the granularity that lets a reader jump to what broke rather than
-- to the conversation it broke in.
ALTER TABLE tool_uses ADD COLUMN is_error INTEGER;

-- ---------------------------------------------------------------------------
-- Backfill
--
-- The flags are already in the archive, inside the same turns.raw_json that
-- migration 009 walked for the payloads. Reading them here rather than asking
-- for a re-sync is the difference between an upgrade and a re-ingest of 46,061
-- sessions.
--
-- What it covers, measured against the author's archive before running:
-- 273,023 tool_result blocks are reachable in parseable raw_json, 10,909 of
-- them flagged is_error: true, and 10,893 of those join to a tool_uses row.
--
-- What it cannot cover: 216,978 turns hold raw_json that does not parse
-- (#101), and 1,808 of those carry the text "is_error":true. Those calls stay
-- NULL — correctly, since NULL means "nothing is known" and nothing is. The
-- gap is about 14% of the archive's real failures and it is not closeable from
-- here; closing it means fixing #101's ingestion, after which a re-sync fills
-- these rows through the write path.
--
-- The shapes it reads are claude-code's and nanoclaw's, which write a
-- tool_result block under message.content. codex and jeff record a tool result
-- with no error flag in it at all, so there is nothing for this to find in
-- their transcripts and nothing it can honestly write — see #100 for why that
-- has to be said out loud rather than inferred from an empty result.
--
-- Runs before the index below exists, so the UPDATE costs no index work.
-- Migration 008 measured what the other order costs: a trigger firing on a
-- column it does not index turned a 965,000-row backfill from 2.2s into 9.2s
-- and left 377 MB of dead segments. No FTS column is added here at all — a
-- one-bit flag has nothing to tokenize, and a partial index answers the only
-- query it exists to serve.
--
-- Timed end to end against a copy of the author's archive — 46,061 sessions,
-- 996,680 turns, 279,383 tool uses, 6,473 MB: see the PR for the figures.
-- ---------------------------------------------------------------------------

-- Every tool_result block in the archive, keyed by the session it happened in
-- and the call it answers.
--
-- The session is half the key, not decoration, for the reason migration 009
-- records: a provider id is only promised to be unique within the conversation
-- that issued it, idx_tool_uses_tool_use_id is deliberately non-unique, and
-- keyed on the id alone a GROUP BY would collapse two sessions' calls and
-- write one session's flag onto the other's row.
--
-- That matters more here than it did for the payloads. A wrong result_content
-- is visibly another session's text; a wrong is_error is one bit, and
-- indistinguishable from a right one.
--
-- MAX of the flag within the group, which is what "answered twice, once with a
-- failure" should come out as, and which also keeps the GROUP BY from picking
-- an arbitrary row's value.
--
-- json_extract gives 1 and 0 for a JSON true and false, and NULL when the key
-- is absent. Absent is the common case — 217,581 of the author's 273,023
-- blocks omit it — and it means success: Claude Code writes the key only when
-- there is something to report. Reading absent as unknown would leave 80% of
-- the archive unqueryable and make the column close to useless, so COALESCE
-- resolves it to 0. The honest NULLs are the rows this query never sees at
-- all, which is the result that never arrived.
--
-- Not filtered to user turns, for migration 009's reason: that is where Claude
-- Code puts them, and keying on the block's own type does not depend on it
-- staying true.
DROP TABLE IF EXISTS temp.migration_013_results;

CREATE TEMP TABLE migration_013_results AS
SELECT t.session_id                                     AS session_id,
       json_extract(je.value, '$.tool_use_id')          AS tool_use_id,
       MAX(COALESCE(json_extract(je.value, '$.is_error'), 0)) AS is_error
FROM turns t,
     json_each(CASE WHEN json_valid(t.raw_json)
                     AND json_type(t.raw_json, '$.message.content') = 'array'
                    THEN json_extract(t.raw_json, '$.message.content')
                    ELSE '[]' END) je
WHERE json_extract(je.value, '$.type') = 'tool_result'
  AND json_extract(je.value, '$.tool_use_id') IS NOT NULL
GROUP BY t.session_id, tool_use_id;

-- Joined on the provider id scoped to the session, which is the join migration
-- 009 established and the only one available: the id is the sole link between
-- a call's row and the block answering it, and a positional rule cannot work
-- here because results arrive in whatever order they finish.
--
-- The `IS NULL` guard makes a replay read-only, the discipline migration 011
-- used for turn_ordinal. It also means a row the write path has already filled
-- is never overwritten by the backfill, so a partially re-synced archive keeps
-- the value its transcript produced.
UPDATE tool_uses
SET is_error = src.is_error
FROM migration_013_results AS src
WHERE tool_uses.tool_use_id = src.tool_use_id
  AND tool_uses.session_id = src.session_id
  AND tool_uses.is_error IS NULL;

DROP TABLE IF EXISTS temp.migration_013_results;

-- ---------------------------------------------------------------------------
-- The index
--
-- Partial, on the one value anyone filters for. "Which calls failed" selects
-- 10,909 rows out of 279,383 on the author's archive — 3.9% — so a full index
-- would carry the other 96% for nothing, and a flag that cannot be filtered
-- cheaply is a flag nobody filters.
--
-- Covering (session_id, turn_id) because the surfaces that read it all ask the
-- same follow-up: GetFailedToolCalls scopes by session, and internal/search
-- asks "does this turn hold a failed call" through an EXISTS on turn_id.
-- Keeping both on the index means neither has to seek back into the table to
-- answer it.
--
-- WHERE is_error = 1 rather than IS NOT NULL: the 0s are the bulk of the
-- column and nothing searches for them. A query for successful calls is a
-- scan, deliberately — it returns a quarter of a million rows and has no
-- selective form.
-- ---------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_tool_uses_is_error
    ON tool_uses(session_id, turn_id)
    WHERE is_error = 1;
