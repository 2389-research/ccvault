// ABOUTME: Reads over tool_uses that the turn and session readers have no business carrying — today, the failed calls (#83).
// ABOUTME: Serves the MCP tool-use surface from the stored is_error column, not from raw_json.

package db

import (
	"database/sql"
	"fmt"
)

// FailedToolCall names one tool call whose result reported a failure. Enough
// to find it and to say what it was, and nothing more: the caller goes on to
// read the call out of material it already holds — MCP from the turn it is
// already emitting — so returning the payloads here would copy a quarter of a
// megabyte per session to be thrown away. ToolCall in toolcalls.go is the shape
// for a caller that has no such material and must render the call itself.
type FailedToolCall struct {
	// TurnID is the turn that issued the call, which is what a reader
	// navigates to.
	TurnID string

	// TurnOrdinal is the call's position within that turn, from 0. Half of
	// the table's key since migration 011, and the only way to point at one
	// call of a turn that made fifteen.
	TurnOrdinal int

	// ToolUseID is the provider's id, empty for a source that minted none —
	// jeff leaves it empty on 633 of 742 calls. It is what a consumer joins
	// this row against an id recorded elsewhere with; a row without one can
	// only be identified by TurnID and TurnOrdinal above.
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
//
// For a caller labelling one page of a session, GetFailedToolCallsForTurns
// narrows that further; this one is for a caller that means the session, which
// today is get_session_summary counting its failures. The TUI conversation view
// used to read it too, and stopped when it began reading whole calls from
// SessionToolCallsByTurn — a call read from its own row carries its own
// is_error, so nothing is left to match up (#179).
func (db *DB) GetFailedToolCalls(sessionID string) ([]FailedToolCall, error) {
	rows, err := db.Query(failedToolCallsSQL, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query failed tool calls for session %s: %w", sessionID, err)
	}
	return scanFailedToolCalls(rows)
}

// GetFailedToolCallsForTurns is GetFailedToolCalls scoped to named turns, for
// a caller labelling a page rather than summarising a session (#157).
//
// get_turns calls this once per invocation, and the session-wide form read
// every failure in the session however few turns the page was labelling —
// the shape #141 fixed for the tool names, left behind here because bounding
// it needed this signature to exist.
//
// Naming the turns narrows the seek on idx_tool_uses_is_error from
// (session_id=?) to (session_id=? AND turn_id=?), once per turn on the page.
// Measured by BenchmarkFailedToolCallReadForOnePage, failures at the
// archive's rate of one call in ten, for one 50-turn page:
//
//	1,871 calls    187 rows, 0.18ms ->  5 rows, 0.05ms
//	133,039 calls  13,303 rows, 15ms -> 95 rows, 0.13ms
//
// Cheap either way on today's archive — 11,511 failed calls across it, 196 in
// the worst session — which is why #157 called this the same shape rather
// than the same cost.
//
// The ORDER BY's last term still sorts in a temp B-tree, because that index
// carries session_id and turn_id but not turn_ordinal — left as it is on
// purpose: the sort now spans one page's failures instead of the session's,
// and the archive's worst session holds 196 failures in total, so sorting
// them in Go would move that work rather than remove it. The seek was the
// half that scaled with the session.
//
// Returns an empty slice for a page whose turns broke nothing, like the
// session-wide reader and for the same reason: the callers branch on length.
func (db *DB) GetFailedToolCallsForTurns(sessionID string, turnIDs []string) ([]FailedToolCall, error) {
	failures := make([]FailedToolCall, 0)
	if len(turnIDs) == 0 {
		// `turn_id IN ()` is not valid SQL, and there is nothing to ask about.
		return failures, nil
	}

	for _, batch := range chunkTurnIDs(turnIDs) {
		args := turnIDArgs(sessionID, batch)
		rows, err := db.Query(failedToolCallsForTurnsSQL(len(batch)), args...)
		if err != nil {
			return nil, fmt.Errorf("query failed tool calls for %d turns of session %s: %w",
				len(turnIDs), sessionID, err)
		}
		part, err := scanFailedToolCalls(rows)
		if err != nil {
			return nil, err
		}
		failures = append(failures, part...)
	}

	return failures, nil
}

// failedToolCallsSQL reads every failure of a session.
const failedToolCallsSQL = `
	SELECT turn_id, turn_ordinal, COALESCE(tool_use_id, ''), tool_name
	FROM tool_uses
	WHERE session_id = ? AND is_error = 1
	ORDER BY turn_id, turn_ordinal`

// failedToolCallsForTurnsSQL reads the failures of n named turns. A function
// rather than a constant because the IN list's width is the caller's page
// size.
//
// is_error = 1 stays written out rather than parameterised so SQLite can match
// the partial index's WHERE clause; a bound parameter there would not.
func failedToolCallsForTurnsSQL(n int) string {
	return `
	SELECT turn_id, turn_ordinal, COALESCE(tool_use_id, ''), tool_name
	FROM tool_uses
	WHERE session_id = ? AND is_error = 1 AND turn_id IN (` + inPlaceholders(n) + `)
	ORDER BY turn_id, turn_ordinal`
}

// scanFailedToolCalls reads the rows of a failed-call query, which both
// readers above produce in the order the calls were issued.
func scanFailedToolCalls(rows *sql.Rows) ([]FailedToolCall, error) {
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
