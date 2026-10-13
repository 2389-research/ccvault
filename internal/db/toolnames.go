// ABOUTME: Reads what each turn called out of tool_uses, either for a whole session or for the turns a page is rendering (#141, #157).
// ABOUTME: Lives here rather than in internal/mcp because the SQL over tool_uses belongs to the storage layer, not to a handler.

package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// SessionToolNamesByTurn groups a session's recorded tool calls by the turn
// that issued them.
//
// This is where a caller learns what a turn called. Reading raw_json for it
// only ever worked for claude-code and nanoclaw, whose entries carry a
// top-level `message` object; codex and jeff record a turn's calls on their
// own transcript lines, so a codex turn's calls are not in that turn's
// raw_json at all and no amount of re-parsing recovers them (#100). The
// adapters already extract every source's calls into tool_uses.
//
// Measured over the author's 47k-session archive: of the 287,557 assistant
// turns whose raw_json the old parse could read, not one names a tool the
// stored rows do not, and 501 of them name fewer. The column dominates
// wherever both have an answer.
//
// Duplicates are kept: a turn that made three Bash calls says Bash three
// times, because the count is the fact.
//
// Names within a turn come back in no particular order, and a caller that
// renders them wants ToolNamesForTurns instead. The one caller here —
// get_session_summary — counts them, and the ORDER BY it did not need cost a
// temp B-tree over every row of the session, because idx_tool_uses_session
// carries no ordering (#157). That sort was about half of what the read
// cost, measured by BenchmarkSessionToolNameReadSort over the same rows:
//
//	1,871 calls    1.5ms -> 0.8ms
//	133,039 calls  109ms -> 53ms
//
// The first is the archive's largest session; the second is the synthetic
// scale #141 quoted.
//
// The SQL is a named constant so a test can put EXPLAIN QUERY PLAN in front of
// the query this actually runs rather than in front of a copy of it.
func (db *DB) SessionToolNamesByTurn(sessionID string) (map[string][]string, error) {
	rows, err := db.Query(sessionToolNamesSQL, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query tool calls for session %s: %w", sessionID, err)
	}
	return scanToolNamesByTurn(rows)
}

// ToolNamesForTurns is SessionToolNamesByTurn scoped to named turns, in the
// order the calls were issued — for a caller rendering a page rather than
// summarising a session (#141).
//
// Naming the turns changes which index answers the query, and that is where
// the cost goes. Session-wide, SQLite seeks idx_tool_uses_session and then
// sorts every row it found in a temp B-tree, because that index does not
// carry the ORDER BY. Per turn it seeks idx_tool_uses_turn_ordinal — which
// leads on turn_id and already holds the rows in the order asked for — once
// per turn, and sorts nothing.
//
// Measured by BenchmarkToolNameReadForOnePage on a session of 7,034 turns,
// for one 50-turn page, session-wide -> page-bounded:
//
//	1,871 calls     1,871 rows, 1.3ms  ->   50 rows, 0.07ms
//	133,039 calls  133,039 rows, 205ms ->  950 rows, 0.81ms
//
// The first is the largest real session in the author's archive; the second
// is the scale #141 quoted. Either way the point is the shape: a per-page
// cost proportional to the session is what undoes paging a long one (#79).
func (db *DB) ToolNamesForTurns(sessionID string, turnIDs []string) (map[string][]string, error) {
	if len(turnIDs) == 0 {
		// `turn_id IN ()` is not valid SQL, and there is nothing to ask about.
		return map[string][]string{}, nil
	}

	byTurn := make(map[string][]string, len(turnIDs))

	for _, batch := range chunkTurnIDs(turnIDs) {
		args := turnIDArgs(sessionID, batch)
		rows, err := db.Query(toolNamesForTurnsSQL(len(batch)), args...)
		if err != nil {
			return nil, fmt.Errorf("query tool calls for %d turns of session %s: %w", len(turnIDs), sessionID, err)
		}
		part, err := scanToolNamesByTurn(rows)
		if err != nil {
			return nil, err
		}
		for turnID, names := range part {
			byTurn[turnID] = append(byTurn[turnID], names...)
		}
	}

	return byTurn, nil
}

// sessionToolNamesSQL reads every call of a session, deliberately unordered:
// see SessionToolNamesByTurn on why the ORDER BY came off (#157).
const sessionToolNamesSQL = `
	SELECT turn_id, tool_name
	FROM tool_uses
	WHERE session_id = ? AND turn_id IS NOT NULL`

// toolNamesForTurnsSQL reads the calls of n named turns, in the order they
// were issued. A function rather than a constant because the IN list's width
// is the caller's page size.
func toolNamesForTurnsSQL(n int) string {
	return `
	SELECT turn_id, tool_name
	FROM tool_uses
	WHERE session_id = ? AND turn_id IN (` + inPlaceholders(n) + `)
	ORDER BY turn_id, turn_ordinal`
}

// scanToolNamesByTurn groups the rows of a (turn_id, tool_name) query.
func scanToolNamesByTurn(rows *sql.Rows) (map[string][]string, error) {
	defer func() { _ = rows.Close() }()

	byTurn := make(map[string][]string)
	for rows.Next() {
		var turnID, toolName string
		if err := rows.Scan(&turnID, &toolName); err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}
		if toolName == "" {
			continue
		}
		byTurn[turnID] = append(byTurn[turnID], toolName)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tool calls: %w", err)
	}

	return byTurn, nil
}

// turnIDChunk is how many turn ids go into one `turn_id IN (...)`, so a caller
// asking about more turns than get_turns' 50-row page cannot exceed SQLite's
// bound-parameter ceiling.
const turnIDChunk = 500

// chunkTurnIDs splits turn ids into batches of at most turnIDChunk. Every id
// of one turn therefore lands in a single batch, which is what lets a caller
// append each batch's names without reordering a turn's calls.
func chunkTurnIDs(turnIDs []string) [][]string {
	batches := make([][]string, 0, (len(turnIDs)+turnIDChunk-1)/turnIDChunk)
	for start := 0; start < len(turnIDs); start += turnIDChunk {
		batches = append(batches, turnIDs[start:min(start+turnIDChunk, len(turnIDs))])
	}
	return batches
}

// turnIDArgs is the argument list for a `session_id = ? AND turn_id IN (...)`
// query over one batch.
func turnIDArgs(sessionID string, batch []string) []interface{} {
	args := make([]interface{}, 0, len(batch)+1)
	args = append(args, sessionID)
	for _, id := range batch {
		args = append(args, id)
	}
	return args
}

// inPlaceholders renders n bound-parameter placeholders for an IN list. Named
// so it does not shadow the local `placeholders` slices the rest of the
// package builds for the same job.
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
