-- Migration 017: the user turns that are really tool results get their type
-- back, and the ones that stored no text get it recovered from raw_json.
--
-- Issue #185. Claude Code writes a tool result as a message with role "user"
-- on a transcript line of type "user" — the API shape, where a result is
-- something the caller hands back to the model. pkg/parser copied that type
-- verbatim, so every tool result in the author's archive is filed as a message
-- the person typed: 315,985 of them across 52,375 sessions, while
-- `tool_result` — the type the TUI, the exporter and the MCP schema were all
-- written for — had zero rows. 145,798 of those turns came out with no content
-- at all and rendered as a bare `User` header with nothing beneath it.
--
-- Why a backfill at all. Normally the write-path fix is the whole answer and a
-- backfill is an admission: resync the sessions and the rows come out right.
-- Not here. The mislabeled rows run from 2026-02 to now and the oldest
-- transcript left on disk is dated Sep 25 — ~78% of them (114,389, May to
-- August) have no source file to resync from. raw_json is present on 100% of
-- them and is the only surviving record, which is the fidelity purpose that
-- column exists for.
--
-- The rule, stated once: a user message whose content blocks are ALL
-- tool_result blocks is a tool result. "All" and not "any", because a line
-- carrying prose beside a result is a user message with a result attached, and
-- retagging it would file the person's words under a machine's answer. The
-- archive holds no such line — Claude Code writes results into a message of
-- their own — so the predicate below costs nothing today and is what keeps a
-- future producer that batches them from losing prose.
--
-- What is NOT retagged, and why each one is a row this must leave alone:
--
--   * 4,557 user turns with no raw_json. Nothing left to read; the guard is
--     json_valid(), which is NULL and so false for them.
--   * 6 user turns whose oversized line was replaced by the _ccvault_stripped
--     placeholder. The placeholder keeps the turn's identity and drops its
--     message, so nothing says these were results. They have no
--     $.message.content and fall out on the json_type test.
--   * 9 user turns that are a pasted screenshot or PDF — real messages with no
--     text in them, whose only block is an image or a document.
--   * every assistant turn. tool_use blocks live there and are not touched —
--     #184 already made every surface read calls from tool_uses.
--
-- A note for whoever writes 018: splitStatements ends a statement at any line
-- whose text ends in a semicolon, comment lines included. One in this header
-- did, which handed the migrator a comment-only statement to execute.

-- ---------------------------------------------------------------------------
-- Statement 1: recover the text, for the rows that have none.
--
-- turns.content gets a bounded preview and not the body. The whole result text
-- already lives twice over — verbatim in turns.raw_json, and in
-- tool_uses.result_content under issue #28's storage policy with an FTS index
-- of its own. Measured on the author's archive, storing the bodies here as
-- well would add 453 MB to a turns.content holding 296 MB today and duplicate
-- every one of those bytes into turns_fts, where they would compete with the
-- prose that index exists to find. `[Tool Result: ...]` is the shape this
-- column has carried for these turns since the beginning; the type is what was
-- wrong.
--
-- Rows that already carry a preview are not rewritten. 170,187 of them do, and
-- their text is already correct — writing it again would churn turns_fts for
-- nothing. That is also the idempotency predicate: once statement 2 has run,
-- no row here is type 'user' any more, so a second run matches nothing.
--
-- substr() counts characters, which is why pkg/parser's matching cut counts
-- runes: 67,845 of the 145,006 rows filled here carry a non-ASCII character
-- inside the first 200, and a byte cut in Go would disagree with this
-- statement on 47% of them. TestMigration017AgreesWithTheParser is what holds
-- the two together.
--
-- Written as UPDATE ... FROM so the extracted text is named once and read
-- twice (its length, then its prefix) instead of the whole expression being
-- repeated.
UPDATE turns
SET content = '[Tool Result: '
    || CASE WHEN length(src.result_text) > 200
            THEN substr(src.result_text, 1, 197) || '...'
            ELSE src.result_text END
    || ']'
FROM (
    SELECT t.rowid AS rid,
           -- The two shapes the transcripts write: a JSON string (11,000 of
           -- 11,408 blocks in the author's live transcripts) and an array of
           -- content blocks (408). An image-bearing result yields NULL and so
           -- no content: #28 detects an image by shape rather than by tool
           -- name, its base64 must never reach the FTS index, and a preview
           -- that kept a screenshot's caption while dropping the screenshot is
           -- a misleading partial — 191 of the affected rows are exactly that.
           -- A tool_reference block contributes nothing, which leaves 287 rows
           -- legitimately empty.
           CASE json_type(t.raw_json, '$.message.content[0].content')
               WHEN 'text' THEN json_extract(t.raw_json, '$.message.content[0].content')
               WHEN 'array' THEN
                   CASE WHEN EXISTS (
                            SELECT 1 FROM json_each(t.raw_json, '$.message.content[0].content') img
                            WHERE json_extract(img.value, '$.type') = 'image')
                        THEN NULL
                        ELSE (SELECT group_concat(json_extract(txt.value, '$.text'), char(10))
                              FROM json_each(t.raw_json, '$.message.content[0].content') txt
                              WHERE json_extract(txt.value, '$.type') = 'text'
                                AND json_extract(txt.value, '$.text') <> '')
                   END
           END AS result_text
    FROM turns t
    WHERE t.type = 'user'
      AND (t.content IS NULL OR t.content = '')
      AND t.raw_json IS NOT NULL
      AND json_valid(t.raw_json)
      AND json_type(t.raw_json, '$.message.content') = 'array'
      -- Single-block only. Every one of the archive's 315,985 all-tool_result
      -- user turns carries exactly one content block, and a general
      -- N-block join cannot be correlated from inside a FROM subquery in
      -- SQLite. Statement 2 retags whatever shape appears; a row with several
      -- blocks keeps its empty content here and gets it on the next resync,
      -- where pkg/parser reads all of them.
      AND json_array_length(t.raw_json, '$.message.content') = 1
      AND json_extract(t.raw_json, '$.message.content[0].type') = 'tool_result'
) AS src
WHERE turns.rowid = src.rid
  AND src.result_text IS NOT NULL
  AND src.result_text <> '';

-- ---------------------------------------------------------------------------
-- Statement 2: the retag.
--
-- After statement 1, because that one reads type = 'user' to find its rows.
--
-- `IS NOT 'tool_result'` rather than `<> 'tool_result'`, so a block with no
-- type key at all — json_extract returns NULL, and NULL <> 'x' is NULL, not
-- true — counts as "not a tool result" and keeps the row typed `user`. The
-- conservative direction: a shape nobody has seen is left as the transcript
-- labelled it.
--
-- Does not fire turns_fts. The turns_au trigger is AFTER UPDATE OF content,
-- timestamp, so flipping a type costs nothing in the index — which is also why
-- the two statements are not merged: mentioning content in the SET clause
-- fires the trigger for every matched row whether the value changes or not.
UPDATE turns
SET type = 'tool_result'
WHERE type = 'user'
  AND raw_json IS NOT NULL
  AND json_valid(raw_json)
  AND json_type(raw_json, '$.message.content') = 'array'
  AND json_array_length(raw_json, '$.message.content') > 0
  AND NOT EXISTS (
      SELECT 1 FROM json_each(raw_json, '$.message.content') block
      WHERE json_extract(block.value, '$.type') IS NOT 'tool_result'
  );

-- ---------------------------------------------------------------------------
-- The verdict on turns_fts
--
-- Statement 1 writes content, which fires turns_au and so rewrites a document
-- in turns_fts for every row it fills. That makes this check load-bearing
-- rather than decoration: comparing the %_docsize count against the turns
-- count cannot see a stranded entry whose rowid something later reused, and a
-- rebuild that reassigned rowids would leave the counts agreeing exactly while
-- every document described the wrong turn.
--
-- FTS5's strict 'integrity-check' reads the index against the content table
-- and errors if they disagree, which rolls this whole migration back — the
-- right outcome, because an archive whose search index silently describes
-- other people's conversations is worse than one still carrying #185. This is
-- the statement that aborted a bad run of migration 012 and left that archive
-- byte-identical to its backup.
--
-- Note for anyone running this by hand: the sqlite3 CLI exits 0 even when this
-- check fails. Judge it by the error text, never by the exit status.
INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1);
