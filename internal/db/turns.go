// ABOUTME: Database operations for turns and full-text search
// ABOUTME: Provides CRUD operations for turn records and FTS5 search

package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/2389-research/ccvault/pkg/models"
)

// toolUsesTurnOrdinalIndex is the unique index that gives tool_uses a key:
// (turn_id, turn_ordinal), a call's position inside the turn that issued it.
// Named here because migration 011 creates it and the tests probe for it.
const toolUsesTurnOrdinalIndex = "idx_tool_uses_turn_ordinal"

// InsertTurns inserts multiple turns in a batch
func (db *DB) InsertTurns(turns []models.Turn) error {
	return db.WithTx(func(tx *sql.Tx) error {
		return db.InsertTurnsTx(tx, turns)
	})
}

// InsertTurnsTx inserts multiple turns within a transaction
func (db *DB) InsertTurnsTx(tx *sql.Tx, turns []models.Turn) error {
	if err := checkDistinctOrdinals(turns); err != nil {
		return err
	}

	// A turn can collide with a row already in the table two ways: by id,
	// which is the transcript uuid, and by position, which UNIQUE(session_id,
	// ordinal) enforces. Both collisions are resolved by deleting the row that
	// holds the spot — the same thing INSERT OR REPLACE did — but with a
	// DELETE statement of our own rather than REPLACE's implicit one. See
	// deleteTurnConflictsSQL for why that distinction is the whole fix.
	del, err := tx.Prepare(deleteTurnConflictsSQL)
	if err != nil {
		return fmt.Errorf("prepare delete conflicting turns: %w", err)
	}
	defer func() { _ = del.Close() }()

	delTools, err := tx.Prepare(deleteToolUsesOfConflictingTurnsSQL)
	if err != nil {
		return fmt.Errorf("prepare delete tool uses of conflicting turns: %w", err)
	}
	defer func() { _ = delTools.Close() }()

	ins, err := tx.Prepare(`
		INSERT INTO turns (id, session_id, parent_id, type, timestamp, ordinal, content, raw_json, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert turns: %w", err)
	}
	defer func() { _ = ins.Close() }()

	for _, t := range turns {
		if _, err := delTools.Exec(t.ID, t.SessionID, t.Ordinal); err != nil {
			return fmt.Errorf("delete tool uses of turns conflicting with %s: %w", t.ID, err)
		}
		if _, err := del.Exec(t.ID, t.SessionID, t.Ordinal); err != nil {
			return fmt.Errorf("delete turn conflicting with %s: %w", t.ID, err)
		}
		_, err := ins.Exec(
			t.ID,
			t.SessionID,
			t.ParentID,
			t.Type,
			t.Timestamp,
			t.Ordinal,
			t.Content,
			t.RawJSON,
			t.InputTokens,
			t.OutputTokens,
		)
		if err != nil {
			return fmt.Errorf("insert turn %s: %w", t.ID, err)
		}
	}

	return nil
}

// deleteTurnConflictsSQL removes whatever occupies the id and the position a
// turn is about to take, so the insert that follows cannot conflict.
//
// This replaces INSERT OR REPLACE, and the reason is the whole of issue #93.
// turns_fts is an FTS5 external-content index: a turn's index entry is keyed
// by its rowid, and the only thing that removes one is the turns_ad trigger
// firing on a DELETE. SQLite routes a REPLACE's *implicit* delete through an
// AFTER DELETE trigger only when PRAGMA recursive_triggers is on, and that is
// off by default. So with REPLACE, the index's correctness rested on a
// connection-level setting that the database file cannot require of anything
// that opens it. ccvault's own DSN sets it and verifyConnectionPragmas
// asserts it (#42), which fixed ccvault — and left every other writer able to
// shred the index silently: the sqlite3 CLI, another tool, a driver upgrade
// that renames the DSN parameter, or a ccvault binary built before #42 that
// is still first on someone's PATH. One `sync --full` through such a binary
// left 19,023 documents in the author's turns_fts with no turns row behind
// them, and only FTS5's strict integrity-check could see it.
//
// A DELETE statement fires AFTER DELETE triggers under SQLite's own rules,
// with no pragma involved. That is why tool_uses came through the same sync
// untouched — it is written as an explicit DELETE followed by a plain INSERT —
// and this makes turns match it.
//
// Semantics are unchanged from REPLACE: one or two rows can be deleted (a
// turn can collide by id with one row and by position with another), and both
// go. The OR is two index seeks, sqlite_autoindex_turns_1 and
// idx_turns_session_ordinal, not a scan.
const deleteTurnConflictsSQL = `
	DELETE FROM turns WHERE id = ? OR (session_id = ? AND ordinal = ?)`

// deleteToolUsesOfConflictingTurnsSQL removes the tool uses of whatever
// deleteTurnConflictsSQL is about to delete, so no row is left pointing at a
// turn that no longer exists. Same three bindings, same two index seeks.
//
// This is issue #87: 6,378 rows whose turn_id named no row in turns, 2.4% of
// the table, invisible to search — every query joins through turns — and
// counted by GetToolUsageStats regardless.
//
// tool_uses.turn_id has declared `REFERENCES turns(id) ON DELETE CASCADE`
// since the initial schema and that declaration has never done anything,
// because foreign key enforcement is off by default in SQLite and ccvault's
// DSN does not turn it on. Enforcing it would be a schema-wide change on an
// archive that has never had it — `PRAGMA foreign_key_check` on the author's
// archive reports three violations already, all sessions.parent_session_id
// naming a parent transcript the archive does not hold, which is a legitimate
// shape for a subagent session whose parent was never synced. So the delete is
// explicit here instead, for the same reason #93 made the turns write path
// delete explicitly: a database file cannot require a connection setting of
// whatever opens it, so correctness must not rest on one.
//
// Why a turn gets deleted rather than replaced: sync resolves a position
// collision by deleting whatever holds the position, so a transcript that was
// rewritten rather than appended to (internal/sync reports this as "rewritten
// upstream") removes turns outright. Deleting a session's tool uses by
// session_id does not cover it, because a row can be filed under a session its
// turn does not belong to — 1,281 of them were on the author's archive before
// migration 011 repaired them.
//
// It runs per turn, alongside the DELETE above, which is one more statement in
// the hottest loop in the writer. Measured on a synthetic 200-session,
// 24,000-turn, 24,000-tool-use archive: a first sync goes 2.42s to 2.56s and a
// `sync --full` 3.37s to 3.53s, against a binary built without it.
const deleteToolUsesOfConflictingTurnsSQL = `
	DELETE FROM tool_uses WHERE turn_id IN (
		SELECT id FROM turns WHERE id = ? OR (session_id = ? AND ordinal = ?))`

// deleteToolUsesOfSessionTurnsSQL is the same guarantee for the whole-session
// delete: a session's turns go, so their tool uses go, wherever those rows
// happen to be filed.
const deleteToolUsesOfSessionTurnsSQL = `
	DELETE FROM tool_uses WHERE turn_id IN (
		SELECT id FROM turns WHERE session_id = ?)`

// checkDistinctOrdinals rejects a batch that gives two turns in one session
// the same position.
//
// The unique index on (session_id, ordinal) already forbids this, but the
// write above resolves a position collision by deleting whatever holds the
// position — the same thing INSERT OR REPLACE did before it. So a caller that
// forgot to assign ordinals would not get an error; it would get a session
// holding one turn where it passed twenty, silently, with every earlier turn
// deleted on its way in.
//
// Checked here rather than left to the schema so the failure says which turns
// collided instead of which index did.
func checkDistinctOrdinals(turns []models.Turn) error {
	type position struct {
		sessionID string
		ordinal   int
	}
	seen := make(map[position]string, len(turns))
	for _, t := range turns {
		p := position{t.SessionID, t.Ordinal}
		if first, dup := seen[p]; dup {
			return fmt.Errorf(
				"insert turns: %q and %q both claim ordinal %d of session %q; "+
					"an ordinal is a turn's position in its session and must be unique within it",
				first, t.ID, t.Ordinal, t.SessionID)
		}
		seen[p] = t.ID
	}
	return nil
}

// GetTurns retrieves turns for a session, in session order.
//
// Ordered by ordinal, not timestamp. Timestamp is not a total order over a
// session: turns inside one assistant response tie to the millisecond, and a
// clock that skewed puts turns in the wrong order outright. Sorting on a key
// with ties also leaves the row order free to change between calls, which
// matters more than it sounds — every caller that paginates this result
// (MCP's get_turns slices it by offset and limit) would silently skip and
// repeat rows. ordinal is unique per session, so the sort is total and the
// same every time.
func (db *DB) GetTurns(sessionID string) ([]models.Turn, error) {
	query := `
		SELECT id, session_id, parent_id, type, timestamp, ordinal, content, raw_json, input_tokens, output_tokens
		FROM turns WHERE session_id = ? ORDER BY ordinal ASC`

	rows, err := db.Query(query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query turns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var turns []models.Turn
	for rows.Next() {
		var t models.Turn
		var parentID sql.NullString
		var content sql.NullString
		var rawJSON sql.NullString
		err := rows.Scan(
			&t.ID,
			&t.SessionID,
			&parentID,
			&t.Type,
			&t.Timestamp,
			&t.Ordinal,
			&content,
			&rawJSON,
			&t.InputTokens,
			&t.OutputTokens,
		)
		if err != nil {
			return nil, fmt.Errorf("scan turn: %w", err)
		}
		if parentID.Valid {
			t.ParentID = parentID.String
		}
		if content.Valid {
			t.Content = content.String
		}
		if rawJSON.Valid {
			// Turn.RawJSON is json.RawMessage — downstream json.Marshal
			// (e.g. `ccvault show --json`) will error with "error calling
			// MarshalJSON for type json.RawMessage" if the stored bytes
			// aren't valid JSON. Corrupted rows do exist in the wild —
			// old syncs from before the oversized-line + truncation-
			// placeholder fixes could have stored non-JSON blobs.
			//
			// Validate at scan time and drop invalid raw_json so consumers
			// don't crash on `json.Marshal`. Data is preserved in the
			// text `content` column and in `source_file` for anyone who
			// needs the original.
			raw := []byte(rawJSON.String)
			if json.Valid(raw) {
				t.RawJSON = raw
			}
			// else: leave t.RawJSON as nil; the JSON tag is omitempty so
			// it'll be dropped from output rather than crashing marshal.
		}
		turns = append(turns, t)
	}

	return turns, rows.Err()
}

// TurnCursor names the end of a session's turn sequence.
type TurnCursor struct {
	// LastOrdinal is the highest ordinal the session holds, so the next turn
	// appended belongs at LastOrdinal+1.
	LastOrdinal int
	// LastEntryUUID is the id of the turn sitting at LastOrdinal.
	LastEntryUUID string
	// Found is false for a session with no turns, which is distinct from a
	// session whose only turn is at ordinal 0.
	Found bool
}

// SessionTurnCursor returns the end of a session's turn sequence: the highest
// ordinal and the turn at it.
//
// This is the "everything after position N" primitive. A timestamp watermark
// cannot do the job — it is ambiguous when turns tie (re-send or skip?) and
// wrong when a clock skewed — whereas an ordinal is unique within the session
// by construction.
//
// MAX(ordinal) rather than a stored next_ordinal counter: the unique index on
// (session_id, ordinal) turns this into an index seek, and a denormalized
// counter is one more thing to drift during the per-file replace sync does.
func (db *DB) SessionTurnCursor(sessionID string) (TurnCursor, error) {
	var c TurnCursor
	err := db.QueryRow(`
		SELECT ordinal, id FROM turns
		WHERE session_id = ?
		ORDER BY ordinal DESC
		LIMIT 1`, sessionID).Scan(&c.LastOrdinal, &c.LastEntryUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return TurnCursor{}, nil
	}
	if err != nil {
		return TurnCursor{}, fmt.Errorf("read turn cursor for session %s: %w", sessionID, err)
	}
	c.Found = true
	return c, nil
}

// DeleteTurnsForSession removes all turns for a session (for re-sync), and the
// tool uses belonging to those turns — see deleteToolUsesOfSessionTurnsSQL.
func (db *DB) DeleteTurnsForSession(sessionID string) error {
	return db.WithTx(func(tx *sql.Tx) error {
		return db.DeleteTurnsForSessionTx(tx, sessionID)
	})
}

// DeleteTurnsForSessionTx removes all turns for a session within a transaction,
// and the tool uses belonging to those turns.
func (db *DB) DeleteTurnsForSessionTx(tx *sql.Tx, sessionID string) error {
	if _, err := tx.Exec(deleteToolUsesOfSessionTurnsSQL, sessionID); err != nil {
		return fmt.Errorf("delete tool uses of session %s's turns: %w", sessionID, err)
	}
	_, err := tx.Exec("DELETE FROM turns WHERE session_id = ?", sessionID)
	return err
}

// contentScoped confines a caller's FTS5 expression to turns_fts's content
// column.
//
// turns_fts also indexes search_period, which holds the day and month tokens a
// date filter intersects with (migration 010). An unscoped expression is
// matched against every column, so a caller searching for the literal text of
// one of those tokens would get back every turn of that month instead of the
// turns whose text holds it. Scoping is what keeps the period column
// answering only to internal/search's own period terms.
func contentScoped(expr string) string {
	return "{content} : (" + expr + ")"
}

// SearchTurns performs full-text search on turn content
func (db *DB) SearchTurns(query string, limit int) ([]models.Turn, error) {
	if limit <= 0 {
		limit = 20
	}

	// Use FTS5 MATCH syntax
	sqlQuery := `
		SELECT t.id, t.session_id, t.parent_id, t.type, t.timestamp, t.ordinal, t.content, t.input_tokens, t.output_tokens
		FROM turns t
		JOIN turns_fts fts ON t.rowid = fts.rowid
		WHERE turns_fts MATCH ?
		ORDER BY rank, t.id
		LIMIT ?`

	rows, err := db.Query(sqlQuery, contentScoped(query), limit)
	if err != nil {
		return nil, fmt.Errorf("search turns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var turns []models.Turn
	for rows.Next() {
		var t models.Turn
		var parentID sql.NullString
		var content sql.NullString
		err := rows.Scan(
			&t.ID,
			&t.SessionID,
			&parentID,
			&t.Type,
			&t.Timestamp,
			&t.Ordinal,
			&content,
			&t.InputTokens,
			&t.OutputTokens,
		)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		if parentID.Valid {
			t.ParentID = parentID.String
		}
		if content.Valid {
			t.Content = content.String
		}
		turns = append(turns, t)
	}

	return turns, rows.Err()
}

// SearchTurnsWithFilters performs filtered search
func (db *DB) SearchTurnsWithFilters(textQuery string, projectID int64, model string, toolName string, limit int) ([]models.Turn, error) {
	if limit <= 0 {
		limit = 20
	}

	var conditions []string
	var args []interface{}

	// Build query based on filters
	baseQuery := `
		SELECT DISTINCT t.id, t.session_id, t.parent_id, t.type, t.timestamp, t.ordinal, t.content, t.input_tokens, t.output_tokens
		FROM turns t
		JOIN sessions s ON t.session_id = s.id`

	// Join tool_uses if filtering by tool
	if toolName != "" {
		baseQuery += ` JOIN tool_uses tu ON t.session_id = tu.session_id`
		conditions = append(conditions, "tu.tool_name = ?")
		args = append(args, toolName)
	}

	// Full-text search
	if textQuery != "" {
		baseQuery += ` JOIN turns_fts fts ON t.rowid = fts.rowid`
		conditions = append(conditions, "turns_fts MATCH ?")
		args = append(args, contentScoped(textQuery))
	}

	// Project filter
	if projectID > 0 {
		conditions = append(conditions, "s.project_id = ?")
		args = append(args, projectID)
	}

	// Model filter
	if model != "" {
		conditions = append(conditions, "s.model LIKE ?")
		args = append(args, "%"+model+"%")
	}

	// Combine conditions
	if len(conditions) > 0 {
		baseQuery += " WHERE " + strings.Join(conditions, " AND ")
	}

	// Results span sessions, so ordinal is not an ordering key here — a
	// position only means something inside one session. `t.id` is a
	// tiebreaker, not a preference: turns that share a timestamp would
	// otherwise swap places between calls, and a caller paginating the
	// result would skip and repeat rows.
	baseQuery += " ORDER BY t.timestamp DESC, t.id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(baseQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("search turns with filters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var turns []models.Turn
	for rows.Next() {
		var t models.Turn
		var parentID sql.NullString
		var content sql.NullString
		err := rows.Scan(
			&t.ID,
			&t.SessionID,
			&parentID,
			&t.Type,
			&t.Timestamp,
			&t.Ordinal,
			&content,
			&t.InputTokens,
			&t.OutputTokens,
		)
		if err != nil {
			return nil, fmt.Errorf("scan filtered result: %w", err)
		}
		if parentID.Valid {
			t.ParentID = parentID.String
		}
		if content.Valid {
			t.Content = content.String
		}
		turns = append(turns, t)
	}

	return turns, rows.Err()
}

// GetTurnCount returns total number of turns
func (db *DB) GetTurnCount() (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM turns").Scan(&count)
	return count, err
}

// InsertToolUses inserts tool usage records
func (db *DB) InsertToolUses(toolUses []models.ToolUse) error {
	return db.WithTx(func(tx *sql.Tx) error {
		return db.InsertToolUsesTx(tx, toolUses)
	})
}

// InsertToolUsesTx inserts tool usage records within a transaction.
//
// Each call is numbered by its position within the turn that issued it, which
// is what UNIQUE(turn_id, turn_ordinal) keys the table on (migration 011). The
// position comes from the batch: a transcript's calls arrive in the order the
// parser read them, and internal/sync passes one session's calls in one slice,
// so the Nth call of a turn in this batch is the Nth call of that turn in the
// transcript. A caller that split one turn's calls across two batches would
// restart the numbering and replace the rows the first batch wrote — the one
// thing this function requires of its caller and cannot check.
//
// The delete-then-insert is deliberate and not interchangeable with INSERT OR
// REPLACE. SQLite resolves a REPLACE's unique conflict by deleting the
// conflicting row, so REPLACE against the new index would turn a position
// collision into a silent deletion — #29 found that shape, and #93 rewrote the
// turns write path away from it. A DELETE of our own also fires tool_uses_ad
// under SQLite's own rules rather than only when PRAGMA recursive_triggers is
// on, which is what keeps tool_uses_fts free of orphans on any connection.
func (db *DB) InsertToolUsesTx(tx *sql.Tx, toolUses []models.ToolUse) error {
	del, err := tx.Prepare(`DELETE FROM tool_uses WHERE turn_id = ? AND turn_ordinal = ?`)
	if err != nil {
		return fmt.Errorf("prepare delete conflicting tool_uses: %w", err)
	}
	defer func() { _ = del.Close() }()

	stmt, err := tx.Prepare(`
		INSERT INTO tool_uses (turn_id, session_id, tool_name, file_path, timestamp,
			tool_use_id, input_json, input_length,
			result_content, result_length, result_omitted_reason, turn_ordinal)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert tool_uses: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	next := make(map[string]int, len(toolUses))
	for _, tu := range toolUses {
		ordinal := next[tu.TurnID]
		next[tu.TurnID] = ordinal + 1

		if _, err := del.Exec(tu.TurnID, ordinal); err != nil {
			return fmt.Errorf("delete tool_use conflicting with call %d of turn %s: %w", ordinal, tu.TurnID, err)
		}
		_, err := stmt.Exec(tu.TurnID, tu.SessionID, tu.ToolName, tu.FilePath, tu.Timestamp,
			nullIfEmpty(tu.ToolUseID), nullIfEmpty(tu.InputJSON), nullIfZero(tu.InputLength),
			nullIfEmpty(tu.ResultContent), resultLengthValue(tu), nullIfEmpty(tu.ResultOmittedReason),
			ordinal)
		if err != nil {
			return fmt.Errorf("insert tool_use: %w", err)
		}
	}

	return nil
}

// nullIfEmpty stores an unrecorded string as NULL rather than as the empty
// string. Migration 008 set the precedent for last_entry_uuid: a column
// holding both NULL and the empty string makes every read path treat the
// empty string as a third state.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullIfZero is the same discipline for a length that was never measured.
func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// resultLengthValue encodes "was there a result at all" into the column, which
// is where that fact lives: NULL means nothing answered this call, 0 means it
// answered with nothing. Both are real and they are different.
func resultLengthValue(tu models.ToolUse) any {
	if !tu.HasResult {
		return nil
	}
	return tu.ResultLength
}

// DeleteToolUsesForSession removes tool uses for a session
func (db *DB) DeleteToolUsesForSessionTx(tx *sql.Tx, sessionID string) error {
	_, err := tx.Exec("DELETE FROM tool_uses WHERE session_id = ?", sessionID)
	return err
}

// GetToolUsageStats returns tool usage counts
func (db *DB) GetToolUsageStats(limit int) (map[string]int, error) {
	query := `
		SELECT tool_name, COUNT(*) as count
		FROM tool_uses
		GROUP BY tool_name
		ORDER BY count DESC`

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query tool stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int)
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("scan tool stats: %w", err)
		}
		result[name] = count
	}

	return result, rows.Err()
}

// GetToolNamesLike returns distinct tool names containing fragment
// (case-insensitive), ordered by name. Used for "did you mean" hints
// when a tool: search matches nothing.
//
// The fragment is matched with INSTR rather than LIKE so that '%' and '_' in a
// caller-supplied fragment are literal characters instead of wildcards. LOWER on
// both sides keeps the ASCII case-insensitivity that LIKE provided by default.
func (db *DB) GetToolNamesLike(fragment string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(`
		SELECT DISTINCT tool_name
		FROM tool_uses
		WHERE INSTR(LOWER(tool_name), LOWER(?)) > 0
		ORDER BY tool_name
		LIMIT ?`, fragment, limit)
	if err != nil {
		return nil, fmt.Errorf("query tool names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tool name: %w", err)
		}
		names = append(names, name)
	}

	return names, rows.Err()
}
