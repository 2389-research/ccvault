// ABOUTME: Reads a session's tool calls out of tool_uses grouped by the turn that issued them, for a view that renders them (#179).
// ABOUTME: The one place every adapter agrees, which is what makes a renderer reading from here right for a source nobody has written yet.

package db

import (
	"database/sql"
	"fmt"
)

// ToolCall is one recorded call as a view renders it: what was called, with
// what, and whether the result said it failed.
//
// Smaller than models.ToolUse on purpose. The result text is the largest thing
// a row carries and no caller here renders it, so reading it would copy a
// quarter of a megabyte per session to be thrown away — the same reasoning that
// keeps FailedToolCall narrow.
//
// No turn id or ordinal, because the grouping carries both: the map key is the
// turn, and the slice is in the order the calls were issued.
type ToolCall struct {
	// ToolName is what was called.
	ToolName string

	// InputJSON is the arguments exactly as the source recorded them. Not
	// guaranteed to parse as JSON despite the name — codex's custom_tool_call
	// carries a plain-text body, which is how apply_patch sends a diff. A
	// renderer has to cope with text here.
	InputJSON string

	// Failed is true only when the result reported a failure. is_error's third
	// state — nothing is known, because no result arrived or because the source
	// records no error flag at all — collapses into false here, since a view
	// either marks a call or does not. Marking an unanswered call would send a
	// reader looking for an error nobody reported.
	Failed bool
}

// SessionToolCallsByTurn groups a session's tool calls by the turn that issued
// them, each turn's calls in the order it made them.
//
// This is what the conversation view renders an assistant turn's calls from,
// and reading them here rather than out of the turn's raw_json is the whole of
// #179. raw_json only ever named a turn's calls for claude-code and nanoclaw:
// codex records each call on a transcript line of its own, so a codex turn's
// calls are not in that turn's raw_json at all, and pi writes a block type the
// Claude Code shape does not have. tool_uses is filled identically by every
// adapter, which makes a renderer reading from here correct for a source that
// does not exist yet — where a switch over block types has to be extended per
// source, and renders a blank line when it is not.
//
// Nothing here is derived from raw_json at read time, including the failure
// flag: 216,978 turns in the author's archive hold raw_json that does not parse
// (#101), and what is stored is what is known. See GetFailedToolCalls.
//
// Session-wide and unpaged, matching GetTurns beside it — the view loads a whole
// conversation, so a call read narrower than the turn read would not bound
// anything. The archive's largest session holds 1,871 calls; ordering them
// costs a temp B-tree because idx_tool_uses_session carries no ordering, which
// #157 measured at 0.7ms for that session.
//
// Returns an empty map, not an error, for a session that called nothing: the
// caller indexes it per turn, and an error for an uneventful session would make
// it indistinguishable from a failed read.
func (db *DB) SessionToolCallsByTurn(sessionID string) (map[string][]ToolCall, error) {
	rows, err := db.Query(sessionToolCallsSQL, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query tool calls for session %s: %w", sessionID, err)
	}
	return scanToolCallsByTurn(rows)
}

// sessionToolCallsSQL reads every call of a session that names a turn, ordered
// so each turn's calls arrive in the order they were issued.
//
// turn_id IS NOT NULL because a call naming no turn has no turn to render under,
// and NULL would otherwise gather every such call of a session into one phantom
// group. Same filter SessionToolNamesByTurn uses, for the same reason.
const sessionToolCallsSQL = `
	SELECT turn_id, tool_name, COALESCE(input_json, ''), COALESCE(is_error, 0)
	FROM tool_uses
	WHERE session_id = ? AND turn_id IS NOT NULL
	ORDER BY turn_id, turn_ordinal`

// scanToolCallsByTurn groups the rows of the query above. The ORDER BY puts a
// turn's calls together and in issue order, so appending preserves both.
func scanToolCallsByTurn(rows *sql.Rows) (map[string][]ToolCall, error) {
	defer func() { _ = rows.Close() }()

	byTurn := make(map[string][]ToolCall)
	for rows.Next() {
		var turnID string
		var call ToolCall
		var isError int
		if err := rows.Scan(&turnID, &call.ToolName, &call.InputJSON, &isError); err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}
		call.Failed = isError == 1
		byTurn[turnID] = append(byTurn[turnID], call)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tool calls: %w", err)
	}

	return byTurn, nil
}
