-- ABOUTME: Add remote_push_state to track per-remote incremental push watermarks
-- ABOUTME: Composite PK (remote_name, session_id); enables "push what's changed" semantics

CREATE TABLE remote_push_state (
    remote_name       TEXT NOT NULL,
    session_id        TEXT NOT NULL,
    session_ended_at  DATETIME NOT NULL,
    pushed_at         DATETIME NOT NULL,
    PRIMARY KEY (remote_name, session_id)
);

CREATE INDEX idx_remote_push_state_pushed_at
    ON remote_push_state(remote_name, pushed_at);
