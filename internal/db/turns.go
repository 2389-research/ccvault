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

	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO turns (id, session_id, parent_id, type, timestamp, ordinal, content, raw_json, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert turns: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, t := range turns {
		_, err := stmt.Exec(
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

// checkDistinctOrdinals rejects a batch that gives two turns in one session
// the same position.
//
// The unique index on (session_id, ordinal) already forbids this, but the
// insert above is INSERT OR REPLACE, and SQLite resolves a REPLACE against a
// unique index by *deleting* the conflicting row. So a caller that forgot to
// assign ordinals would not get an error — it would get a session holding one
// turn where it passed twenty, silently, with every earlier turn deleted on
// its way in.
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

// DeleteTurnsForSession removes all turns for a session (for re-sync)
func (db *DB) DeleteTurnsForSession(sessionID string) error {
	_, err := db.Exec("DELETE FROM turns WHERE session_id = ?", sessionID)
	return err
}

// DeleteTurnsForSessionTx removes all turns for a session within a transaction
func (db *DB) DeleteTurnsForSessionTx(tx *sql.Tx, sessionID string) error {
	_, err := tx.Exec("DELETE FROM turns WHERE session_id = ?", sessionID)
	return err
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

	rows, err := db.Query(sqlQuery, query, limit)
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
		args = append(args, textQuery)
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

// InsertToolUsesTx inserts tool usage records within a transaction
func (db *DB) InsertToolUsesTx(tx *sql.Tx, toolUses []models.ToolUse) error {
	stmt, err := tx.Prepare(`
		INSERT INTO tool_uses (turn_id, session_id, tool_name, file_path, timestamp,
			tool_use_id, input_json, input_length,
			result_content, result_length, result_omitted_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert tool_uses: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, tu := range toolUses {
		_, err := stmt.Exec(tu.TurnID, tu.SessionID, tu.ToolName, tu.FilePath, tu.Timestamp,
			nullIfEmpty(tu.ToolUseID), nullIfEmpty(tu.InputJSON), nullIfZero(tu.InputLength),
			nullIfEmpty(tu.ResultContent), resultLengthValue(tu), nullIfEmpty(tu.ResultOmittedReason))
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
