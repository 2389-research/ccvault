-- ABOUTME: Adds sessions.parent_session_id so a subagent transcript can be its own row.
-- ABOUTME: Backfills the link for subagent rows that only ever carried it inside their composite id.

-- A subagent transcript (<project>/<session-uuid>/subagents/agent-*.jsonl) is
-- its own session row, identified by a minted composite id:
--
--   <source>:<parent-uuid>:agent-<agentId>
--
-- The relationship lives in this column rather than only inside that string,
-- so listings can filter on it and a parent can count its children. NULL means
-- top-level, which is what every pre-existing row is.
ALTER TABLE sessions ADD COLUMN parent_session_id TEXT REFERENCES sessions(id);

-- Both directions are hot: "is this row top-level?" on every default listing,
-- and "how many children does this parent have?" for subagent_count.
CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id)
    WHERE parent_session_id IS NOT NULL;

-- Backfill. The nanoclaw adapter has always minted composite ids for the
-- sidechain files it ingests (133 rows on the author's archive) and has always
-- computed a parent_session_id into its Metadata — but there was no column, so
-- sync dropped it and every one of those rows was orphaned: reachable by id,
-- unlinked to its parent. The id is the only surviving record of the link, and
-- it is enough: the parent id is the composite id minus its last ':' segment.
--
-- substr/rtrim/replace is the SQLite spelling of "index of the last colon":
-- replace(id, ':', '') is the id with colons removed, so rtrim(id, <that>)
-- strips trailing non-colon characters and leaves a string ending at the last
-- colon. Its length is that colon's position.
--
-- Scope, deliberately narrow:
--   * source_file under a subagents/ directory — the on-disk fact that makes a
--     row a sidechain, independent of how its id happens to be spelled.
--   * id of the <prefix>:<parent>:agent-<id> shape, so a two-segment id never
--     gets truncated to its bare prefix.
--   * parent_session_id IS NULL, so this can only ever fill a blank.
--   * NOT 'claude-code:%' — claude-code subagent rows are minted by the code
--     that ships with this migration and always write the column directly, and
--     their parent id is the bare uuid (no source prefix), so the
--     strip-last-segment rule would produce a parent that does not exist.
--     No such row can predate this migration: the scanner skipped them all.
--
-- Rows whose computed parent is not itself in the archive still get the link.
-- It is the truth about where the transcript came from, and the default
-- listing deliberately keeps showing a subagent whose parent is absent so that
-- hiding never turns into losing.
UPDATE sessions
SET parent_session_id = substr(id, 1, length(rtrim(id, replace(id, ':', ''))) - 1)
WHERE parent_session_id IS NULL
  AND source_file LIKE '%/subagents/%'
  AND id LIKE '%:%:agent-%'
  AND id NOT LIKE 'claude-code:%';
