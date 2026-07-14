// ABOUTME: Database operations for turns and full-text search
// ABOUTME: Provides CRUD operations for turn records and FTS5 search

package db

import (
	"database/sql"
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
	stmt, err := tx.Prepare(`
		INSERT OR REPLACE INTO turns (id, session_id, parent_id, type, timestamp, content, raw_json, input_tokens, output_tokens)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
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

// GetTurns retrieves turns for a session
func (db *DB) GetTurns(sessionID string) ([]models.Turn, error) {
	query := `
		SELECT id, session_id, parent_id, type, timestamp, content, raw_json, input_tokens, output_tokens
		FROM turns WHERE session_id = ? ORDER BY timestamp ASC`

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
			t.RawJSON = []byte(rawJSON.String)
		}
		turns = append(turns, t)
	}

	return turns, rows.Err()
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
		SELECT t.id, t.session_id, t.parent_id, t.type, t.timestamp, t.content, t.input_tokens, t.output_tokens
		FROM turns t
		JOIN turns_fts fts ON t.rowid = fts.rowid
		WHERE turns_fts MATCH ?
		ORDER BY rank
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
		SELECT DISTINCT t.id, t.session_id, t.parent_id, t.type, t.timestamp, t.content, t.input_tokens, t.output_tokens
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

	baseQuery += " ORDER BY t.timestamp DESC LIMIT ?"
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
		INSERT INTO tool_uses (turn_id, session_id, tool_name, file_path, timestamp)
		VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert tool_uses: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, tu := range toolUses {
		_, err := stmt.Exec(tu.TurnID, tu.SessionID, tu.ToolName, tu.FilePath, tu.Timestamp)
		if err != nil {
			return fmt.Errorf("insert tool_use: %w", err)
		}
	}

	return nil
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
func (db *DB) GetToolNamesLike(fragment string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := db.Query(`
		SELECT DISTINCT tool_name
		FROM tool_uses
		WHERE tool_name LIKE '%' || ? || '%'
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
