-- ABOUTME: Normalize projects.display_name to basename(path) for existing rows.
-- ABOUTME: Backfill for PR #22 — GetDisplayName was refactored to filepath.Base.

-- Before PR #22, parser.GetDisplayName returned a "last 2–3 path components
-- with ~home substitution" heuristic. New syncs write basename(path). Without
-- this migration, projects last synced under the old binary keep the stale
-- multi-segment display_name forever unless the user runs `ccvault sync --full`.
--
-- The recipe below is the SQLite equivalent of Go's filepath.Base for the
-- POSIX-shaped paths ccvault stores (always absolute, no trailing slash):
--   substr(path, length(rtrim(path, replace(path, '/', ''))) + 1)
--
-- Trick: replace(path, '/', '') gives the path with slashes removed; rtrim
-- then strips those trailing characters from the original path, leaving
-- everything up to and including the final '/'. Its length is the offset
-- to substr from, so substr returns everything after the last '/'.
--
-- Edge cases: paths with no '/' (unlikely, but safe) get their whole value
-- as the basename. Paths with trailing '/' or empty paths are skipped —
-- no valid ccvault row stores those shapes, and skipping avoids corrupting
-- anything unusual we haven't anticipated.

UPDATE projects
SET display_name =
    CASE
        WHEN instr(path, '/') = 0 THEN path
        ELSE substr(path, length(rtrim(path, replace(path, '/', ''))) + 1)
    END
WHERE path IS NOT NULL
  AND path != ''
  AND path NOT LIKE '%/';
