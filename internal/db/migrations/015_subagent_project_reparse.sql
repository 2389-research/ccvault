-- ABOUTME: Forgets the stored mtime of every subagent transcript filed under a project its parent is not in (#156).
-- ABOUTME: Schedules a re-parse rather than rewriting project_id in SQL, so the repair cannot disagree with the code.

-- ---------------------------------------------------------------------------
-- What this fixes
--
-- A subagent session was attributed to the directory the agent ran in. An
-- agent given worktree isolation runs in `<repo>/.claude/worktrees/
-- agent-<hex>`, a directory named after the agent and deleted when it exits,
-- so every dispatch minted a project row of its own. Measured on the author's
-- archive at migration 014, 48,745 sessions in 163 projects:
--
--   42 subagent sessions, 11,997 turns, filed under 37 project rows
--   35 of those rows are `<repo>/.claude/worktrees/agent-<hex>`
--   2 are a hand-made `<repo>/.worktrees/ws-<name>`
--   27 of the 37 belong to the repo ccvault itself
--
-- which is why `ccvault list-sessions --project ccvault` answered
-- "ambiguous" with 28 candidates instead of resolving to one.
--
-- The parse-time rule is fixed in pkg/parser's SubagentProjectPath: a
-- subagent's project is the project of the session that dispatched it, read
-- from the parent transcript's own cwd. That fix is inert on rows already in
-- the archive, because sync skips a file whose stored mtime matches the one on
-- disk and all 42 of these transcripts are untouched since they were written.
--
-- ---------------------------------------------------------------------------
-- Why a re-parse and not an UPDATE
--
-- Repointing project_id here would mean writing the attribution rule a second
-- time, in SQL, over a path string — and SQL cannot read the parent
-- transcript's cwd, which is where the answer actually lives. The two spellings
-- would then be free to disagree, and the one in the migration would be the
-- one nobody runs again. So this forgets the mtime and lets the ordinary sync
-- re-read exactly these files through the corrected code, the same trick
-- migration 012 used to recover 24,062 deleted turns.
--
-- A re-parse is also the non-destructive option: the session rows, their turns
-- and their tool uses all stay exactly where they are until a sync replaces
-- them from the transcript.
--
-- ---------------------------------------------------------------------------
-- The predicate
--
-- "A subagent whose project is not its parent's project", expressed over the
-- two session rows rather than over path text:
--
--   * s.parent_session_id IS NOT NULL is what makes a row a subagent. It is
--     set only by the adapters' subagent branches.
--   * the self-join requires the parent row to be present, so a subagent
--     whose dispatcher is not in the archive (3 on the author's) is left
--     alone — there is nothing to inherit from and a re-parse would not
--     change its filing.
--   * `IS NOT` rather than `<>` so a NULL project_id on either side compares
--     rather than swallowing the row.
--
-- This matches the parse-time rule exactly instead of approximating it, which
-- matters because a looser predicate has something to catch: the author has a
-- hand-made `.worktrees/evals` directory holding 9 **top-level** sessions, a
-- human's own `claude` runs inside a worktree. Those are correctly filed where
-- #72's CanonicalProjectPath put them, they carry no parent_session_id, and
-- any pattern match on "/.worktrees/" would have swept them up.
--
-- Source-agnostic by construction. nanoclaw also ingests subagents and files
-- them by group directory rather than cwd, so its rows already agree with
-- their parents and none of them match.
--
-- Matched on both halves of source_files' key, (path, source): two sources can
-- hold the same path, and an unqualified match on path alone would unmark the
-- other source's file too.
--
-- Deleting the row rather than zeroing its mtime, because mtime is NOT NULL
-- and a zero date is a value some other reader would have to know to distrust.
-- A row's absence already means "never synced", which is exactly the state
-- wanted.
DELETE FROM source_files
WHERE EXISTS (
    SELECT 1
    FROM sessions s
    JOIN sessions parent ON parent.id = s.parent_session_id
    WHERE s.source_file = source_files.path
      AND s.source = source_files.source
      AND s.parent_session_id IS NOT NULL
      AND s.project_id IS NOT parent.project_id
);

-- ---------------------------------------------------------------------------
-- What this migration deliberately does not do
--
-- The project rows the subagents are about to leave keep their session_count
-- until the re-parse has happened, and are then empty. They cannot be dropped
-- here: at this moment the sessions still point at them, and deleting a
-- project row whose sessions are still filed under it would orphan the
-- sessions instead of moving them.
--
-- internal/sync's reconcileProjects prunes a project row that no session
-- references, at the end of the same sync that re-parses these files. That is
-- where the 37 rows go.
