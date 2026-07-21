-- ABOUTME: Add pushed_by column to sessions for per-user attribution on remote vaults
-- ABOUTME: Populated by ccvaultd from the SSH pubkey identity; empty string on local vaults

ALTER TABLE sessions ADD COLUMN pushed_by TEXT NOT NULL DEFAULT '';
