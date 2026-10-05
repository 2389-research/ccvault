-- ABOUTME: Adds the tool payload columns to tool_uses plus an FTS index over the two searchable ones.
-- ABOUTME: Backfills every column from turns.raw_json, so an existing archive needs no re-sync.

-- The id the source gave this call, verbatim. toolu_… for Claude Code and
-- nanoclaw, call_… for codex, jeff's tool_id where it recorded one.
--
-- NULL, not '', when the source recorded no id — the same discipline migration
-- 008 used for last_entry_uuid, so no read path has to treat the empty string
-- as a third state. Jeff needs it: tool_id is empty on 633 of its 742 real
-- requests.
--
-- The provider's id rather than a surrogate, because the point of the column
-- is to be joinable against ids recorded elsewhere. A subagent transcript's
-- meta.json carries a toolUseId naming the exact call that dispatched it (#31,
-- resolving for 87 of 95 real subagents) and had nowhere to land until now.
ALTER TABLE tool_uses ADD COLUMN tool_use_id TEXT;

-- The call's arguments, stored whole and never truncated. Measured across the
-- author's 259,836 calls: 78.6 MB in total, p50 72 bytes, largest 99,792.
--
-- Not guaranteed to parse as JSON, despite the name — which follows the column
-- agentsview established. It holds the arguments as the source recorded them,
-- and codex's custom_tool_call records a plain-text body (that is how
-- apply_patch sends a diff). Check json_valid before json_extract.
ALTER TABLE tool_uses ADD COLUMN input_json TEXT;
ALTER TABLE tool_uses ADD COLUMN input_length INTEGER;

-- The result text, NULL when the content was left out or when the tool
-- returned nothing at all.
ALTER TABLE tool_uses ADD COLUMN result_content TEXT;

-- The result's size as the transcript carries it, recorded whether or not the
-- content was stored. That is what lets a consumer tell a short result from an
-- omitted one without guessing.
--
-- It is also the only thing that says a result exists: result_length IS NULL
-- means nothing ever answered this call, which is a different fact from a
-- result that came back empty (result_length = 0). A separate has_result
-- column would say the same thing twice and could disagree with itself.
ALTER TABLE tool_uses ADD COLUMN result_length INTEGER;

-- Why result_content was left out: 'bulk_read', 'image', 'oversize',
-- 'undecodable'. NULL when the content is stored. See pkg/toolpayload for the
-- policy and the measurements behind it.
ALTER TABLE tool_uses ADD COLUMN result_omitted_reason TEXT;

-- Deliberately NOT unique. tool_uses has no natural key today and this is not
-- one: the same transcript ingested under two sources (a Claude Code session
-- also walked by the nanoclaw adapter) legitimately repeats a provider id, and
-- #29 found that an INSERT OR REPLACE resolves a unique-index conflict by
-- silently deleting the conflicting row. A plain index is what the join needs.
CREATE INDEX IF NOT EXISTS idx_tool_uses_tool_use_id ON tool_uses(tool_use_id)
    WHERE tool_use_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Backfill
--
-- The payloads are already in the archive, inside turns.raw_json — 1,348 MB of
-- it against 109 MB of extracted content. Reading them here rather than asking
-- for a re-sync is the difference between an upgrade and a re-ingest of 44,840
-- sessions.
--
-- Runs before tool_uses_fts and its triggers exist, so these two full-table
-- UPDATEs cost no index work at all. Migration 008 measured what the other
-- order costs: an FTS trigger that fires on a column it does not index turned
-- a backfill of 965,000 rows from 2.2s into 9.2s and added 377 MB of dead
-- segments. Creating the index afterwards and rebuilding it in one pass avoids
-- the question entirely.
--
-- Timed end to end against a copy of the author's archive — 965,061 turns,
-- 261,138 tool uses, 5,057 MB: the whole of this migration takes 2m26s and
-- grows the database by 665 MB. That is 442 MB of payload columns (74 MB of
-- inputs, 368 MB of results), ~130 MB of FTS, and the rest page overhead.
-- 259,836 of the 261,138 rows get an id and an input; see the note on the
-- join below for the 1,302 that do not.
-- ---------------------------------------------------------------------------

-- Every tool_use block in the archive, with its position inside the message.
DROP TABLE IF EXISTS temp.migration_009_calls;

CREATE TEMP TABLE migration_009_calls AS
SELECT t.id                               AS turn_id,
       je.key                             AS block_pos,
       json_extract(je.value, '$.id')     AS tool_use_id,
       json_extract(je.value, '$.name')   AS tool_name,
       json_extract(je.value, '$.input')  AS input_json
FROM turns t,
     json_each(CASE WHEN json_valid(t.raw_json)
                     AND json_type(t.raw_json, '$.message.content') = 'array'
                    THEN json_extract(t.raw_json, '$.message.content')
                    ELSE '[]' END) je
WHERE t.type = 'assistant'
  AND json_extract(je.value, '$.type') = 'tool_use';

-- Match each tool_uses row to the block that produced it.
--
-- There is no stored key to join on — that is what this migration is adding —
-- so the join is per turn by position: the Nth tool_uses row of a turn, by
-- rowid, is the Nth tool_use block of its message, by array index. rowid is
-- insertion order and the parser has always walked the content array in order,
-- so the two sequences are the same sequence.
--
-- tool_name has to agree as well. It is not needed to find the match; it is
-- there so that a row the position rule gets wrong stays NULL instead of being
-- labelled with another call's input. Measured on the author's archive: the
-- guard fired on nothing, so no row was saved from a mislabel — but nothing
-- else would have caught one.
--
-- 1,302 rows came out of this with a NULL id on the author's archive, measured
-- on the migrated copy. The cause is duplicate tool_uses rows left by the
-- recovery import in #30: turns carrying more tool_uses rows than their
-- message has tool_use blocks. The position rule fills the first row of each
-- turn and leaves the extras alone, which is the right outcome — giving two
-- rows the same provider id would make the id useless as a join key.
--
-- How those duplicates are distributed is deliberately not stated here. Three
-- counts of it disagree (two query shapes of mine, and an independent one that
-- found a longer tail), and the difference turns on whether turns with no
-- parseable tool_use block are folded in. Pinning it down belongs to the
-- data-hygiene issue the duplicates need, not to this migration, which behaves
-- the same either way.
UPDATE tool_uses
SET tool_use_id  = src.tool_use_id,
    input_json   = src.input_json,
    input_length = octet_length(src.input_json)
FROM (
    SELECT rows.rid, calls.tool_use_id, calls.input_json
    FROM (SELECT rowid AS rid, turn_id, tool_name,
                 ROW_NUMBER() OVER (PARTITION BY turn_id ORDER BY rowid) AS n
          FROM tool_uses) AS rows
    JOIN (SELECT turn_id, tool_name, tool_use_id, input_json,
                 ROW_NUMBER() OVER (PARTITION BY turn_id ORDER BY block_pos) AS n
          FROM migration_009_calls) AS calls
      ON calls.turn_id = rows.turn_id
     AND calls.n = rows.n
     AND calls.tool_name = rows.tool_name
) AS src
WHERE tool_uses.rowid = src.rid
  AND tool_uses.tool_use_id IS NULL;

-- Every tool_result block, keyed by the session it happened in and the call it
-- answers.
--
-- The session is half the key, not decoration. A provider id is only ever
-- promised to be unique within the conversation that issued it: claude-code's
-- toolu_… are random enough that no id in the author's archive appears in two
-- sessions, but the archive holds no codex sessions at all, and whether
-- codex's call_… are globally unique or numbered per conversation is the
-- provider's choice. Keyed on the id alone, two sessions sharing one would
-- have the GROUP BY collapse them and write one session's output onto the
-- other's row — silently, and into the wrong FTS document, so it would come
-- back as a search hit attributed to the wrong conversation.
--
-- tool_uses already carries session_id, so scoping the join costs nothing.
-- idx_tool_uses_tool_use_id is deliberately non-unique (see above), which is
-- the right call and also means nothing in the schema would catch this.
--
-- A tool result is always recorded in the same transcript as its call, so
-- scoping to the session cannot lose a legitimate match.
--
-- Not filtered to user turns. That is where Claude Code puts them, and keying
-- on the block's own type costs nothing and does not depend on it staying
-- true. GROUP BY keeps one row per call: a transcript that answered the same
-- call twice should not make the join multiply rows.
DROP TABLE IF EXISTS temp.migration_009_results;

CREATE TEMP TABLE migration_009_results AS
SELECT t.session_id                                  AS session_id,
       json_extract(je.value, '$.tool_use_id')       AS tool_use_id,
       json_type(je.value, '$.content')              AS content_type,
       json_extract(je.value, '$.content')           AS content,
       octet_length(json_extract(je.value, '$.content')) AS content_len
FROM turns t,
     json_each(CASE WHEN json_valid(t.raw_json)
                     AND json_type(t.raw_json, '$.message.content') = 'array'
                    THEN json_extract(t.raw_json, '$.message.content')
                    ELSE '[]' END) je
WHERE json_extract(je.value, '$.type') = 'tool_result'
  AND json_extract(je.value, '$.tool_use_id') IS NOT NULL
GROUP BY t.session_id, tool_use_id;

-- The structured half: whether the content holds an image block, and the text
-- blocks concatenated. 130,516 of the archive's results use this shape and
-- 129,750 of those hold exactly one block, but the ORDER BY is there because
-- the 766 that hold more have an order that means something.
DROP TABLE IF EXISTS temp.migration_009_result_blocks;

CREATE TEMP TABLE migration_009_result_blocks AS
SELECT r.session_id,
       r.tool_use_id,
       MAX(CASE WHEN json_extract(e.value, '$.type') = 'image' THEN 1 ELSE 0 END) AS has_image,
       group_concat(CASE WHEN json_extract(e.value, '$.type') = 'text'
                          AND json_extract(e.value, '$.text') <> ''
                         THEN json_extract(e.value, '$.text') END,
                    char(10) ORDER BY e.key) AS text_content
FROM migration_009_results r,
     json_each(CASE WHEN r.content_type = 'array' THEN r.content ELSE '[]' END) e
GROUP BY r.session_id, r.tool_use_id;

-- Apply the storage policy. The order of the CASE arms is the order in
-- pkg/toolpayload.decide, and the two have to stay in step: the same
-- transcript has to produce the same row whether it arrives through a sync or
-- through this backfill.
--
-- Image first because it is the shape-based rule and the most specific thing
-- true about a result carrying one — matching on the tool's name would miss
-- every MCP tool whose name says nothing about images, and 476 image results
-- holding 54.9 MB of base64 is exactly what must not reach the index.
UPDATE tool_uses
SET result_length = COALESCE(src.content_len, 0),
    result_omitted_reason =
        CASE WHEN src.has_image = 1                                  THEN 'image'
             WHEN tool_uses.tool_name IN ('Read', 'NotebookRead')    THEN 'bulk_read'
             WHEN COALESCE(src.content_len, 0) > 131072              THEN 'oversize'
             WHEN src.content_type IS NULL OR src.content_type = 'null' THEN NULL
             WHEN src.content_type NOT IN ('text', 'array')          THEN 'undecodable'
             ELSE NULL END,
    result_content =
        CASE WHEN src.has_image = 1                                  THEN NULL
             WHEN tool_uses.tool_name IN ('Read', 'NotebookRead')    THEN NULL
             WHEN COALESCE(src.content_len, 0) > 131072              THEN NULL
             WHEN src.content_type = 'text'                          THEN NULLIF(src.content, '')
             WHEN src.content_type = 'array'                         THEN NULLIF(src.text_content, '')
             ELSE NULL END
FROM (
    SELECT r.session_id, r.tool_use_id, r.content_type, r.content, r.content_len,
           COALESCE(b.has_image, 0) AS has_image,
           b.text_content
    FROM migration_009_results r
    LEFT JOIN migration_009_result_blocks b USING (session_id, tool_use_id)
) AS src
WHERE tool_uses.tool_use_id = src.tool_use_id
  AND tool_uses.session_id = src.session_id
  AND tool_uses.result_length IS NULL;

DROP TABLE IF EXISTS temp.migration_009_calls;
DROP TABLE IF EXISTS temp.migration_009_results;
DROP TABLE IF EXISTS temp.migration_009_result_blocks;

-- ---------------------------------------------------------------------------
-- Full-text search over the payloads
--
-- Its own index rather than folding into turns_fts, because the material
-- belongs to a tool_uses row: turns_fts is external-content over turns and has
-- one column, and there is no turns column these payloads could live in
-- without duplicating them.
--
-- Only the two columns whose content is stored are indexed. result_content is
-- NULL for every omitted result, so the file dumps and base64 contribute
-- nothing — which is the whole reason the policy omits them. Indexing 205 MB of
-- file contents would put a search for `git commit` in competition with every
-- file ever read.
-- ---------------------------------------------------------------------------
CREATE VIRTUAL TABLE IF NOT EXISTS tool_uses_fts USING fts5(
    input_json,
    result_content,
    content='tool_uses',
    content_rowid='id'
);

CREATE TRIGGER IF NOT EXISTS tool_uses_ai AFTER INSERT ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(rowid, input_json, result_content)
    VALUES (new.id, new.input_json, new.result_content);
END;

-- Depends on PRAGMA recursive_triggers being ON, which ccvault's connection DSN
-- sets and verifyConnectionPragmas asserts — the same dependency turns_ad has.
-- Sync replaces a session's tool uses with DELETE then INSERT today, so this
-- fires directly; it also covers a REPLACE's implicit delete if one ever
-- arrives.
CREATE TRIGGER IF NOT EXISTS tool_uses_ad AFTER DELETE ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(tool_uses_fts, rowid, input_json, result_content)
    VALUES ('delete', old.id, old.input_json, old.result_content);
END;

-- Scoped to exactly the two indexed columns. An UPDATE that leaves them alone
-- leaves the index correct, and firing on every column is what cost migration
-- 008's backfill 377 MB of dead segments before it was narrowed.
CREATE TRIGGER IF NOT EXISTS tool_uses_au AFTER UPDATE OF input_json, result_content ON tool_uses BEGIN
    INSERT INTO tool_uses_fts(tool_uses_fts, rowid, input_json, result_content)
    VALUES ('delete', old.id, old.input_json, old.result_content);
    INSERT INTO tool_uses_fts(rowid, input_json, result_content)
    VALUES (new.id, new.input_json, new.result_content);
END;

-- One pass over the backfilled columns, rather than per-row trigger work
-- during the UPDATEs above. Idempotent, so a replay re-indexes rather than
-- duplicating.
--
-- Measured: 130 MB of index over 261,138 documents, and the docsize shadow
-- table holds exactly 261,138 rows afterwards — one per tool_uses row, no
-- orphans. That count is the check that matters; #42 established that
-- COUNT(*) on an external-content FTS table resolves through the base table
-- and cannot see an orphan at all.
INSERT INTO tool_uses_fts(tool_uses_fts) VALUES('rebuild');
