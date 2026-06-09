-- ABOUTME: Adds error and subagent tracking columns to sessions
-- ABOUTME: Supports has:error and has:subagent search filters

ALTER TABLE sessions ADD COLUMN has_error BOOLEAN DEFAULT 0;

ALTER TABLE sessions ADD COLUMN has_subagent BOOLEAN DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_sessions_has_error ON sessions(has_error) WHERE has_error = 1;

CREATE INDEX IF NOT EXISTS idx_sessions_has_subagent ON sessions(has_subagent) WHERE has_subagent = 1;
