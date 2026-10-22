// ABOUTME: Reads one page of a session's turns, and counts a session's turns, without materialising the rest (#169).
// ABOUTME: Lives beside GetTurns rather than replacing it — export, analytics and the TUI legitimately want every turn.

package db

import (
	"fmt"
	"strings"

	"github.com/2389-research/ccvault/pkg/models"
)

// TurnPage names which turns of a session a reader wants.
//
// The zero value is "every turn of the session, unbounded", which is
// GetTurns' behaviour — so a caller that forgets to set Limit gets a correct
// answer rather than an empty one.
type TurnPage struct {
	// Type keeps only turns of one type. Empty means every type.
	Type string

	// AfterOrdinal is an exclusive lower bound on the turn's position in its
	// session, and applies only when FromOrdinal is set. Two fields rather
	// than a sentinel because -1 is a legitimate value: it is what a caller
	// starting at the beginning passes, and a session's first turn sits at
	// ordinal 0.
	AfterOrdinal int
	FromOrdinal  bool

	// Offset skips rows of the result. A position (AfterOrdinal) names one
	// turn of the session forever where an offset counts rows of whatever
	// this call returned, so MCP's get_turns prefers the position and sets
	// Offset to 0 when both arrive — see internal/mcp.
	Offset int

	// Limit bounds how many turns come back. Zero or negative means
	// unbounded, which is what makes Offset usable on its own.
	Limit int
}

// turnColumns is the select list GetTurns and GetTurnsPage share, so the two
// readers cannot come to disagree about what a turn is.
const turnColumns = `id, session_id, parent_id, type, timestamp, ordinal, content, raw_json, input_tokens, output_tokens`

// GetTurnsPage reads one page of a session's turns, in session order.
//
// This is the paged half of GetTurns, and issue #169 is why it exists.
// get_turns' only reader of a session's turns was GetTurns, which loads every
// one of them and leaves the handler to filter and slice in Go. The cost of a
// page therefore followed the session rather than the page, in both
// dimensions and at different sessions: on the author's archive the worst case
// by rows is a session of 56,030 turns, and the worst by bytes is one of 4,025
// turns carrying 56.8 MB of content and raw_json. Either was paid in full per
// call, to return fifty rows — and that is what undid #79's resumable
// pagination, whose whole purpose is walking a long session in pages.
//
// GetTurns keeps its signature and its callers: the export, the analytics
// exporter and the TUI's conversation view all legitimately want every turn.
// Same shape as GetFailedToolCallsForTurns beside GetFailedToolCalls (#157).
//
// Ordered by ordinal for the reason GetTurns is: ordinal is unique within a
// session by construction, so the sort is total and the same every time,
// which is what a caller paging the result depends on. Timestamp ties to the
// millisecond inside one assistant response and goes backwards outright on a
// skewed clock.
//
// The ordering is also what keeps the read bounded. idx_turns_session_ordinal
// leads with session_id and then holds the rows in ordinal order, so SQLite
// seeks straight to the page and stops at the limit. A plan that seeks the
// right index and then sorts what it found in a temp B-tree cannot stop early
// — it was half the cost of #157's read, and TestGetTurnsPage_PlanCarriesNoSort
// asserts the absence of that sort over the SQL this function runs.
func (db *DB) GetTurnsPage(sessionID string, page TurnPage) ([]models.Turn, error) {
	query, args := turnsPageQuery(sessionID, page)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query turn page of session %s: %w", sessionID, err)
	}
	return scanTurnRows(rows)
}

// CountTurns is how many turns a session holds, optionally of one type.
//
// A page reports the session's total so a caller knows how far it has to
// walk, and before #169 that total was len() over every turn the handler had
// just loaded. COUNT(*) answers it off idx_turns_session_ordinal, which
// covers the unfiltered case outright — one index seek instead of a million
// bytes of content and raw_json.
//
// A type filter still seeks the same index and tests type per row, because
// there is no index leading with (session_id, type) and adding one to serve a
// count would be a worse trade than reading the rows' type bytes.
func (db *DB) CountTurns(sessionID, typeFilter string) (int, error) {
	query, args := turnCountQuery(sessionID, typeFilter)

	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count turns of session %s: %w", sessionID, err)
	}
	return n, nil
}

// turnsPageQuery builds the page read's SQL and arguments.
//
// A function rather than a constant because which bounds apply is the
// caller's, and named so a test can put EXPLAIN QUERY PLAN in front of the
// query this actually runs rather than in front of a copy of it — the
// discipline sessionToolNamesSQL established (#157).
func turnsPageQuery(sessionID string, page TurnPage) (string, []interface{}) {
	var where strings.Builder
	where.WriteString("session_id = ?")
	args := []interface{}{sessionID}

	if page.Type != "" {
		where.WriteString(" AND type = ?")
		args = append(args, page.Type)
	}
	if page.FromOrdinal {
		where.WriteString(" AND ordinal > ?")
		args = append(args, page.AfterOrdinal)
	}

	// LIMIT is always present, because SQLite rejects an OFFSET without one.
	// -1 is its own "no limit", so an unbounded read with an offset is still
	// one statement rather than a special case here.
	limit := page.Limit
	if limit <= 0 {
		limit = -1
	}
	offset := page.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)

	query := `
		SELECT ` + turnColumns + `
		FROM turns WHERE ` + where.String() + `
		ORDER BY ordinal ASC
		LIMIT ? OFFSET ?`

	return query, args
}

// turnCountQuery builds the total read's SQL and arguments, for the same
// reason turnsPageQuery is a function: so a test can explain the real one.
func turnCountQuery(sessionID, typeFilter string) (string, []interface{}) {
	if typeFilter == "" {
		return `SELECT COUNT(*) FROM turns WHERE session_id = ?`, []interface{}{sessionID}
	}
	return `SELECT COUNT(*) FROM turns WHERE session_id = ? AND type = ?`,
		[]interface{}{sessionID, typeFilter}
}
