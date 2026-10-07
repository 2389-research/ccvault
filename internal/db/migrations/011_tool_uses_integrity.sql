-- ABOUTME: One cleanup pass over tool_uses — orphans, stale session ids, duplicate rows (#82, #87).
-- ABOUTME: Then gives the table the key it never had: (turn_id, turn_ordinal), a call's position in its turn.

-- ---------------------------------------------------------------------------
-- Why one pass
--
-- tool_uses had no key, so nothing in the schema said how many rows a call is
-- allowed to have or that a row must belong to a turn that exists. Two
-- defects came out of that, both from the era of the destructive `--full`
-- syncs before #30 and the recovery import that followed, and both measured
-- against the author's archive:
--
--   #82, duplicates. 1,298 (turn_id, tool_name) groups hold more than one
--   row — 1,288 at ×2, 8 at ×4, 2 at ×15, which reproduces the issue's table
--   exactly. But (turn_id, tool_name) is not a key and never was: a turn may
--   legitimately issue the same tool many times in one message. Checked
--   against the transcripts themselves, by counting the tool_use blocks in
--   each turn's raw_json, 1,288 of those groups really are duplicated and 10
--   are not — including both of the "×15" turns, which each genuinely made
--   fifteen Bash calls. The surplus is 1,302 rows: 1,281 turns carrying two
--   rows for one call, and 7 carrying four.
--
--   #87, orphans. 6,378 rows whose turn_id named no row in turns when the
--   issue was filed. Zero remain on the author's archive today, re-synced
--   away since, but an archive that has not been re-synced still holds them
--   and they are invisible to search (every query joins through turns) while
--   still being counted by GetToolUsageStats and carried in every backup.
--
-- One pass because a dedup keyed on (turn_id, tool_name) could not touch the
-- orphans — their turn_id is absent, not duplicated — and both defects want
-- the same structural fix, which is the second half of this file.
--
-- There is a third finding the cleanup cannot avoid, because the dedup has to
-- decide which of two rows to keep: 1,281 rows name a session their turn does
-- not belong to. A resumed Claude Code transcript copies the earlier
-- session's lines verbatim, so the same turn uuid arrives in two files. turns
-- has a primary key, so one row survives under whichever session was parsed
-- last; tool_uses had none, so both files' rows survived, the older one still
-- filed under the older session. That is also how the orphans were made: sync
-- deletes a session's tool uses by session_id, and a row filed under another
-- session is out of that DELETE's reach, so deleting the turn left the row
-- behind.
--
-- Timed end to end against a copy of the author's archive — 46,061 sessions,
-- 1,014,942 turns, 278,353 tool uses, 6,473 MB: 31.6s, and the file does not
-- grow at all. The new column and index fit in free pages, and dropping the
-- redundant index at the bottom gives back about as much as the new one takes.
-- 278,353 rows in, 277,051 out; 1,281 session ids repaired; 0 orphans (this
-- archive has already been re-synced past them); the 10 turns that really did
-- issue two, four and fifteen calls of one tool keep every row, each matching
-- its own transcript's block count. A replay costs 32.9s and changes nothing.
-- ---------------------------------------------------------------------------

-- Orphans first, so the repair and the dedup below work on rows that have a
-- turn to be judged against.
DELETE FROM tool_uses
WHERE turn_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM turns t WHERE t.id = tool_uses.turn_id);

-- Make every row name the session its turn belongs to.
--
-- turns is the authority: its primary key is the turn uuid, so a turn belongs
-- to exactly one session and a tool use of that turn cannot honestly belong to
-- another. Done before the dedup because it decides which row is worth
-- keeping — without it, the row carrying the payloads migration 009
-- backfilled is also the row with the stale session_id, and no choice between
-- the two is right. Repairing first makes the pair differ only in payload.
--
-- `IS NOT` rather than `<>` so a NULL session_id counts as disagreeing, and
-- joined rather than written as two correlated subqueries so the turn is
-- looked up once per row instead of twice. Measured at 10.4s over the author's
-- 278,353 rows, which is the bulk of this migration's running time.
UPDATE tool_uses
SET session_id = src.session_id
FROM (SELECT t.id AS turn_id, t.session_id FROM turns t) AS src
WHERE src.turn_id = tool_uses.turn_id
  AND tool_uses.session_id IS NOT src.session_id;

-- ---------------------------------------------------------------------------
-- The duplicates
--
-- Two rows of one turn are the same call when they carry the same provider id.
-- That covers 268 of the author's 1,302 surplus rows: 247 turns whose pair
-- repeats one id, and 7 turns whose four rows do.
--
-- The remaining 1,034 carry no id at all, and the reason is migration 009's
-- backfill: it matched the Nth tool_uses row of a turn to the Nth tool_use
-- block of its message, so a turn holding more rows than its message holds
-- blocks filled its first row and left the extras NULL. An extra like that is
-- strictly less informative than its sibling — same turn, same tool, no id,
-- no input, no result — which is what the second arm matches on.
--
-- The payload conditions are not decoration. jeff records no tool_id on 633
-- of its 742 calls, so a jeff turn can hold two same-named calls where only
-- one has an id, and matching on a missing id alone would delete the other
-- one. A call the sync write path recorded always has its input_json, so
-- requiring the row to be empty of payload is what separates "an extra left by
-- an import" from "a real call whose source minted no id". On the author's
-- archive this arm matches 1,034 rows and every one of them is empty.
--
-- Checked against the independent authority before being trusted: counting
-- each turn's tool_use blocks in its raw_json and taking every row beyond that
-- count as surplus selects exactly the same 1,302 rows. That method cannot be
-- the one in the migration — 216,978 turns hold raw_json that does not parse
-- (#101), and a turn whose blocks cannot be counted would have every one of
-- its rows taken as surplus — but it is what makes the rule above more than an
-- assertion.
--
-- Staged into a temp table rather than deleted by a subquery over the table
-- being deleted from, so the rows to remove are decided once, against the
-- table as it stands now.
--
-- Expressed with window functions rather than the correlated EXISTS that says
-- the same thing more readably, and the reason is measured. The EXISTS form
-- needs an index on tool_uses(turn_id) to seek the sibling rows, and this
-- migration drops the standalone one at the bottom as redundant. Without it
-- SQLite picks idx_tool_uses_tool_name instead — and one tool name accounts
-- for 133,044 of the author's rows, so the subquery degrades into a scan of
-- that group per row: the EXISTS form ran in 0.8s on the first pass, where
-- migration 010's index still existed, and had not finished a replay after ten
-- minutes without it. Two sorted passes cost 8.9s either way and depend on no
-- index at all. Checked against the EXISTS form on the live archive: both
-- select the same 1,302 rows, symmetric difference zero.
--
--   pos_in_id_group  position among the rows of this turn and tool carrying
--                    this same provider id. Greater than 1 means an earlier
--                    row already recorded this call. PARTITION BY treats
--                    NULLs as equal, hence the IS NOT NULL guard.
--   pos_in_group     position among the rows of this turn and tool whatever
--                    their id. Greater than 1 means the turn already has a
--                    row for this tool, which is what makes an id-less,
--                    payload-less extra an extra.
-- ---------------------------------------------------------------------------
DROP TABLE IF EXISTS temp.migration_011_surplus;

CREATE TEMP TABLE migration_011_surplus AS
SELECT id FROM (
    SELECT id, tool_use_id, input_json, result_length,
           ROW_NUMBER() OVER (PARTITION BY turn_id, tool_name ORDER BY id) AS pos_in_group,
           ROW_NUMBER() OVER (PARTITION BY turn_id, tool_name, tool_use_id ORDER BY id) AS pos_in_id_group
    FROM tool_uses
)
WHERE (tool_use_id IS NOT NULL AND pos_in_id_group > 1)
   OR (tool_use_id IS NULL
       AND input_json IS NULL
       AND result_length IS NULL
       AND pos_in_group > 1);

DELETE FROM tool_uses WHERE id IN (SELECT id FROM temp.migration_011_surplus);

DROP TABLE IF EXISTS temp.migration_011_surplus;

-- ---------------------------------------------------------------------------
-- The key
--
-- A call's position within the turn that issued it, from 0. This is what
-- tool_uses has been missing, and it is positional rather than the provider's
-- id for one reason: tool_use_id is not available everywhere. jeff leaves it
-- empty on 633 of 742 calls, so (turn_id, tool_use_id) would leave most jeff
-- calls unkeyed — SQLite treats NULLs in a unique index as distinct, so the
-- constraint would simply not apply to them. An ordinal covers every row of
-- every source. hex needs no coverage at all; it records no tool calls.
--
-- Stable across a re-sync, which is the property that makes it a key rather
-- than a serial number: the parser walks a message's content array in order,
-- so the same transcript yields the same call at the same position every time.
-- That is the same ground truth migration 008 used for turns.ordinal and
-- migration 009 used to match rows to blocks.
--
-- NOT NULL DEFAULT 0 matches turns.ordinal. The default is a trap in the same
-- way turns.ordinal's is — a writer that omits the column lands every row of a
-- turn on 0 — which is why internal/db/merge.go computes the position for an
-- incoming archive that predates this column instead of letting the default
-- apply.
-- ---------------------------------------------------------------------------
ALTER TABLE tool_uses ADD COLUMN turn_ordinal INTEGER NOT NULL DEFAULT 0;

-- Numbered by id within the turn, because id is an AUTOINCREMENT rowid and so
-- is insertion order, insertion order is the order the parser read the
-- message's blocks in, and that is the order a re-sync will number them in
-- too. The dedup above has already run, so the survivors come out gapless from
-- 0 — which is what a fresh parse of the same transcript would write, and
-- therefore what the write path's delete-then-insert will land on.
--
-- The `<>` guard makes a replay read-only. It also keeps the UPDATE off the
-- 277,051 surviving rows of the author's archive on a second pass, and costs nothing
-- on the first. tool_uses_au fires only on input_json, result_content and
-- timestamp, so neither pass touches tool_uses_fts — migration 008's 377 MB of
-- dead segments came from exactly this shape of UPDATE with a wider trigger.
UPDATE tool_uses
SET turn_ordinal = src.n
FROM (
    SELECT id, ROW_NUMBER() OVER (PARTITION BY turn_id ORDER BY id) - 1 AS n
    FROM tool_uses
) AS src
WHERE tool_uses.id = src.id
  AND tool_uses.turn_ordinal <> src.n;

-- The constraint itself.
--
-- Unique, which the table has never had, and which is only safe because the
-- write path no longer uses INSERT OR REPLACE: SQLite resolves a REPLACE's
-- unique conflict by deleting the conflicting row, so a unique index plus
-- REPLACE converts a duplicate into a silent deletion. #29 found that; #93
-- rewrote the turns write path away from REPLACE for it; InsertToolUsesTx
-- deletes the row holding the position with a statement of its own and then
-- plainly inserts.
--
-- Deliberately not also unique on (turn_id, tool_use_id). The ordinal already
-- bounds a turn's rows, and a unique index on the provider id would reintroduce
-- the hazard migration 009 documented: the same transcript ingested under two
-- sources repeats a provider id legitimately. idx_tool_uses_tool_use_id stays
-- a plain index, which is what the join needs.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tool_uses_turn_ordinal
    ON tool_uses(turn_id, turn_ordinal);

-- idx_tool_uses_turn_id is now redundant. "The tool uses of this turn" — which
-- internal/search asks twice per returned row — seeks on the leading column of
-- the index above, so the standalone index buys nothing and costs an extra
-- b-tree write per insert. Migration 010 measured it at 12 MB over 264,199
-- rows when it added it.
DROP INDEX IF EXISTS idx_tool_uses_turn_id;
