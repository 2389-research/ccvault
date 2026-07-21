// ABOUTME: CRUD helpers for remote_push_state — the client's incremental push watermark
// ABOUTME: SessionsPendingPush returns sessions never pushed or updated since last push

package db

import (
	"fmt"
	"time"
)

// SessionsPendingPush returns session IDs that need to be pushed to the given remote.
// A session is pending if either (a) it has no row in remote_push_state, or
// (b) its current ended_at is later than the recorded session_ended_at.
func (db *DB) SessionsPendingPush(remoteName string) ([]string, error) {
	rows, err := db.Query(`
        SELECT s.id
        FROM sessions s
        LEFT JOIN remote_push_state r
          ON r.remote_name = ? AND r.session_id = s.id
        WHERE r.session_id IS NULL
           OR s.ended_at > r.session_ended_at
        ORDER BY s.started_at
    `, remoteName)
	if err != nil {
		return nil, fmt.Errorf("query pending: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RecordPush upserts a remote_push_state row after a successful push.
// The (remote_name, session_id) pair identifies the watermark; session_ended_at
// is the value observed at push time, used to detect subsequent updates.
func (db *DB) RecordPush(remoteName, sessionID string, sessionEndedAt time.Time) error {
	_, err := db.Exec(`
        INSERT INTO remote_push_state (remote_name, session_id, session_ended_at, pushed_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(remote_name, session_id) DO UPDATE SET
            session_ended_at = excluded.session_ended_at,
            pushed_at        = excluded.pushed_at
    `, remoteName, sessionID, sessionEndedAt, time.Now().UTC())
	return err
}
