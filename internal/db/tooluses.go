// ABOUTME: Reads over tool_uses that the turn and session readers have no business carrying — today, the failed calls (#83).
// ABOUTME: Serves the MCP tool-use surface and the TUI conversation view from the stored is_error column, not from raw_json.

package db

import "fmt"

// FailedToolCall names one tool call whose result reported a failure. Enough
// to find it and to say what it was, and nothing more: both callers go on to
// read the call out of material they already hold — the TUI from the turn's
// raw_json, MCP from the turn it is already emitting — so returning the
// payloads here would copy a quarter of a megabyte per session to be thrown
// away.
type FailedToolCall struct {
	// TurnID is the turn that issued the call, which is what a reader
	// navigates to.
	TurnID string

	// TurnOrdinal is the call's position within that turn, from 0. Half of
	// the table's key since migration 011, and the only way to point at one
	// call of a turn that made fifteen.
	TurnOrdinal int

	// ToolUseID is the provider's id, empty for a source that minted none —
	// jeff leaves it empty on 633 of 742 calls. It is what the TUI matches a
	// rendered tool_use block against, so a row without one simply does not
	// get marked rather than marking the wrong block.
	ToolUseID string

	// ToolName is what failed, which is the first thing a reader wants.
	ToolName string
}

// GetFailedToolCalls returns the calls in a session whose result reported a
// failure, in the order they were issued.
//
// Reads the stored column rather than re-deriving the flag from raw_json, and
// that is the point of the column existing. 216,978 turns in the author's
// archive hold raw_json that does not parse (#101) and 1,808 of those carry an
// is_error: true the backfill could not reach; a surface that re-parsed per
// read would lose the rows the backfill *did* recover as well, since it would
// hit the same unparseable bytes. What is stored is what is known.
//
// Returns an empty slice, not an error, for a session that broke nothing. The
// callers branch on length, and an error for an uneventful session would make
// it indistinguishable from a failed read.
//
// Seeks idx_tool_uses_is_error, which leads on session_id and is partial on
// is_error = 1 — so the cost is proportional to this session's failures rather
// than to the archive's 279,383 tool uses.
func (db *DB) GetFailedToolCalls(sessionID string) ([]FailedToolCall, error) {
	rows, err := db.Query(`
		SELECT turn_id, turn_ordinal, COALESCE(tool_use_id, ''), tool_name
		FROM tool_uses
		WHERE session_id = ? AND is_error = 1
		ORDER BY turn_id, turn_ordinal`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query failed tool calls for session %s: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	failures := make([]FailedToolCall, 0)
	for rows.Next() {
		var f FailedToolCall
		if err := rows.Scan(&f.TurnID, &f.TurnOrdinal, &f.ToolUseID, &f.ToolName); err != nil {
			return nil, fmt.Errorf("scan failed tool call: %w", err)
		}
		failures = append(failures, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read failed tool calls: %w", err)
	}

	return failures, nil
}
