-- ABOUTME: Adds turns.ordinal, a gapless per-session position, plus sessions.last_entry_uuid.
-- ABOUTME: Backfills both from rowid order, which is insertion order, which is file order.

-- A turn's position in its session, counting from 0 with no gaps, over every
-- turn type the archive holds. Ordering a session used to mean sorting by
-- timestamp, which is wrong in two ways that both show up in real data: turns
-- inside one assistant response tie to the millisecond, and a clock that
-- skewed (a laptop resuming, an NTP step, a session spanning machines) puts
-- them in the wrong order outright.
--
-- One sequence covering all types, not a filtered conversational one: the
-- archive's type mix is assistant/user/system/attachment/progress, and a
-- filtered sequence would force every consumer to know which types take part.
-- Gapless over everything is simpler and is what makes "the turns after
-- position N" a sound cursor.
--
-- Ordinals are strictly per session, and a subagent transcript is its own
-- session row (migration 007), so a subagent numbers from 0 independently of
-- its parent with no special case here. That is load-bearing rather than
-- incidental: subagents run concurrently, so folding them into the parent's
-- sequence would mean interleaving by timestamp — reintroducing the exact
-- tie-and-skew fragility this column exists to remove.
--
-- DEFAULT 0 only so the ADD COLUMN can be NOT NULL on a populated table. The
-- backfill below replaces every one of those zeros, and the unique index
-- created after it would reject them if it did not.
ALTER TABLE turns ADD COLUMN ordinal INTEGER NOT NULL DEFAULT 0;

-- The uuid of the turn sitting at the session's highest ordinal. A re-parse
-- can check it against the transcript it just read and tell "resuming where I
-- left off" from "this file was rewritten under me" — which a row count or an
-- mtime cannot distinguish. NULL for a session with no turns; NULL rather than
-- '' so no read path has to treat the empty string as a third state.
--
-- next_ordinal, the other half of agentsview's pattern, is deliberately not
-- here: MAX(ordinal) against the unique index below is an index seek, and a
-- denormalized counter is one more thing to drift during the per-file replace
-- that sync performs on every re-sync.
ALTER TABLE sessions ADD COLUMN last_entry_uuid TEXT;

-- Narrow the FTS update trigger to the only column turns_fts indexes before
-- the backfill runs, because the backfill is a full-table UPDATE and this
-- trigger does not care which column changed.
--
-- Measured on 965,000 synthetic turns: with the trigger firing on every
-- column the backfill takes 9.2s and grows the database by 377 MB of FTS
-- segments that tombstone and re-add byte-identical content; narrowed to
-- content it takes 2.2s and grows it by 20 MB, the column and the index. The
-- wide form was never needed — an UPDATE that leaves content alone leaves the
-- index correct, and nothing in the tree updates turns.content at all.
DROP TRIGGER IF EXISTS turns_au;
CREATE TRIGGER turns_au AFTER UPDATE OF content ON turns BEGIN
    INSERT INTO turns_fts(turns_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO turns_fts(rowid, content) VALUES (new.rowid, new.content);
END;

-- Backfill, ordered by rowid alone.
--
-- rowid reflects insertion order, insertion order is the order the parser
-- walked the JSONL file, and file order is the ground truth. Leading with
-- timestamp — as this issue first proposed — would reintroduce the ties and
-- let a skewed clock reorder turns that rowid had right.
--
-- Checked rather than assumed, read-only, against the author's 965,061-turn
-- archive: 6,597 adjacent turn pairs carry a timestamp earlier than the turn
-- inserted before them and 19,160 tie exactly, so the two rules genuinely
-- disagree. Of the 1,413 sessions whose source .jsonl was still on disk to
-- compare against, ORDER BY rowid reproduced the file's line order for 1,412
-- and ORDER BY timestamp, rowid for 1,336.
--
-- The one session rowid got wrong had three turns rewritten by a later
-- partial ingest, so the database no longer holds their original position in
-- any column. Nothing recoverable in SQL; the next sync of that file replaces
-- the session's turns wholesale and numbers them correctly.
--
-- The `ordinal <> ord` guard keeps a replay (anything that rewinds
-- schema_version re-runs every migration above the rewind point) from
-- rewriting rows that are already right.
UPDATE turns SET ordinal = seq.ord
FROM (
    SELECT rowid AS rid,
           ROW_NUMBER() OVER (PARTITION BY session_id ORDER BY rowid) - 1 AS ord
    FROM turns
) AS seq
WHERE turns.rowid = seq.rid
  AND turns.ordinal <> seq.ord;

-- UNIQUE is the constraint that makes a position a cursor: ask for ordinal N
-- of a session and at most one turn can answer. A unique index rather than a
-- table constraint because SQLite cannot add one via ALTER TABLE; it enforces
-- the same thing, and doubles as the index MAX(ordinal) seeks.
CREATE UNIQUE INDEX IF NOT EXISTS idx_turns_session_ordinal ON turns(session_id, ordinal);

-- Highest ordinal, not latest timestamp — the same reason the backfill above
-- does not lead with timestamp. `IS NULL` so a replay can only ever fill a
-- blank.
UPDATE sessions
SET last_entry_uuid = (
    SELECT t.id FROM turns t
    WHERE t.session_id = sessions.id
    ORDER BY t.ordinal DESC
    LIMIT 1
)
WHERE last_entry_uuid IS NULL;
