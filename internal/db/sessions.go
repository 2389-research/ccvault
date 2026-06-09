// ABOUTME: Database operations for sessions
// ABOUTME: Provides CRUD operations for session records

package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// UpsertSession creates or updates a session record
func (db *DB) UpsertSession(s *models.Session) error {
	source := s.Source
	if source == "" {
		source = "claude-code"
	}

	query := `
		INSERT INTO sessions (id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source_mtime, has_error, has_subagent, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ended_at = excluded.ended_at,
			model = COALESCE(excluded.model, sessions.model),
			turn_count = excluded.turn_count,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			source_mtime = excluded.source_mtime,
			has_error = excluded.has_error,
			has_subagent = excluded.has_subagent,
			source = excluded.source`

	_, err := db.Exec(query,
		s.ID,
		s.ProjectID,
		s.StartedAt,
		s.EndedAt,
		s.Model,
		s.GitBranch,
		s.TurnCount,
		s.InputTokens,
		s.OutputTokens,
		s.CacheReadTokens,
		s.CacheWriteTokens,
		s.SourceFile,
		time.Now(),
		s.HasError,
		s.HasSubagent,
		source,
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// UpsertSessionTx creates or updates a session record within a transaction
func (db *DB) UpsertSessionTx(tx *sql.Tx, s *models.Session) error {
	source := s.Source
	if source == "" {
		source = "claude-code"
	}

	query := `
		INSERT INTO sessions (id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source_mtime, has_error, has_subagent, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ended_at = excluded.ended_at,
			model = COALESCE(excluded.model, sessions.model),
			turn_count = excluded.turn_count,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			source_mtime = excluded.source_mtime,
			has_error = excluded.has_error,
			has_subagent = excluded.has_subagent,
			source = excluded.source`

	_, err := tx.Exec(query,
		s.ID,
		s.ProjectID,
		s.StartedAt,
		s.EndedAt,
		s.Model,
		s.GitBranch,
		s.TurnCount,
		s.InputTokens,
		s.OutputTokens,
		s.CacheReadTokens,
		s.CacheWriteTokens,
		s.SourceFile,
		time.Now(),
		s.HasError,
		s.HasSubagent,
		source,
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return nil
}

// GetSession retrieves a session by ID
func (db *DB) GetSession(id string) (*models.Session, error) {
	query := `
		SELECT id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source
		FROM sessions WHERE id = ?`

	s := &models.Session{}
	var endedAt sql.NullTime
	err := db.QueryRow(query, id).Scan(
		&s.ID,
		&s.ProjectID,
		&s.StartedAt,
		&endedAt,
		&s.Model,
		&s.GitBranch,
		&s.TurnCount,
		&s.InputTokens,
		&s.OutputTokens,
		&s.CacheReadTokens,
		&s.CacheWriteTokens,
		&s.SourceFile,
		&s.Source,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if endedAt.Valid {
		s.EndedAt = endedAt.Time
	}
	return s, nil
}

// GetSessions retrieves sessions with optional filters
func (db *DB) GetSessions(projectID int64, limit int) ([]models.Session, error) {
	query := `
		SELECT s.id, s.project_id, s.started_at, s.ended_at, s.model, s.git_branch,
			s.turn_count, s.input_tokens, s.output_tokens, s.cache_read_tokens, s.cache_write_tokens,
			s.source_file, COALESCE(p.path, '') as project_path, s.source
		FROM sessions s
		LEFT JOIN projects p ON s.project_id = p.id`

	var args []interface{}
	if projectID > 0 {
		query += " WHERE s.project_id = ?"
		args = append(args, projectID)
	}

	query += " ORDER BY s.started_at DESC"

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []models.Session
	for rows.Next() {
		var s models.Session
		var endedAt sql.NullTime
		err := rows.Scan(
			&s.ID,
			&s.ProjectID,
			&s.StartedAt,
			&endedAt,
			&s.Model,
			&s.GitBranch,
			&s.TurnCount,
			&s.InputTokens,
			&s.OutputTokens,
			&s.CacheReadTokens,
			&s.CacheWriteTokens,
			&s.SourceFile,
			&s.ProjectPath,
			&s.Source,
		)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		if endedAt.Valid {
			s.EndedAt = endedAt.Time
		}
		sessions = append(sessions, s)
	}

	return sessions, rows.Err()
}

// GetSessionStats returns aggregate statistics for sessions
func (db *DB) GetSessionStats() (count int, totalTurns int, totalTokens int64, err error) {
	query := `
		SELECT
			COUNT(*),
			COALESCE(SUM(turn_count), 0),
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0)
		FROM sessions`
	err = db.QueryRow(query).Scan(&count, &totalTurns, &totalTokens)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("get session stats: %w", err)
	}
	return count, totalTurns, totalTokens, nil
}

// GetSessionBySourceFile retrieves a session by its source file path
func (db *DB) GetSessionBySourceFile(path string) (*models.Session, error) {
	query := `
		SELECT id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source_mtime
		FROM sessions WHERE source_file = ?`

	s := &models.Session{}
	var endedAt sql.NullTime
	var sourceMtime sql.NullTime
	err := db.QueryRow(query, path).Scan(
		&s.ID,
		&s.ProjectID,
		&s.StartedAt,
		&endedAt,
		&s.Model,
		&s.GitBranch,
		&s.TurnCount,
		&s.InputTokens,
		&s.OutputTokens,
		&s.CacheReadTokens,
		&s.CacheWriteTokens,
		&s.SourceFile,
		&sourceMtime,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session by source: %w", err)
	}
	if endedAt.Valid {
		s.EndedAt = endedAt.Time
	}
	return s, nil
}

// GetSourceMtime retrieves the last sync time for a source file
func (db *DB) GetSourceMtime(path string) (time.Time, error) {
	var mtime sql.NullTime
	err := db.QueryRow("SELECT source_mtime FROM sessions WHERE source_file = ?", path).Scan(&mtime)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if mtime.Valid {
		return mtime.Time, nil
	}
	return time.Time{}, nil
}

// GetAllSourceMtimes loads all source file modification times from the source_files tracking table.
// If source is non-empty, only returns entries matching that source.
func (db *DB) GetAllSourceMtimes(source ...string) (map[string]time.Time, error) {
	query := "SELECT path, mtime FROM source_files"
	var args []interface{}
	if len(source) > 0 && source[0] != "" {
		query += " WHERE source = ?"
		args = append(args, source[0])
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]time.Time)
	for rows.Next() {
		var path string
		var mtime time.Time
		if err := rows.Scan(&path, &mtime); err != nil {
			continue
		}
		result[path] = mtime
	}
	return result, rows.Err()
}

// UpsertSourceFileMtime records a source file's mtime after processing.
// Source defaults to "claude-code" if empty.
func (db *DB) UpsertSourceFileMtime(path string, mtime time.Time, source ...string) error {
	src := "claude-code"
	if len(source) > 0 && source[0] != "" {
		src = source[0]
	}
	_, err := db.Exec(
		"INSERT INTO source_files (path, source, mtime, synced_at) VALUES (?, ?, ?, ?) ON CONFLICT(path, source) DO UPDATE SET mtime = excluded.mtime, synced_at = excluded.synced_at",
		path, src, mtime, time.Now(),
	)
	return err
}

// UpsertSourceFileMtimeTx records a source file's mtime within a transaction.
// Source defaults to "claude-code" if empty.
func (db *DB) UpsertSourceFileMtimeTx(tx *sql.Tx, path string, mtime time.Time, source ...string) error {
	src := "claude-code"
	if len(source) > 0 && source[0] != "" {
		src = source[0]
	}
	_, err := tx.Exec(
		"INSERT INTO source_files (path, source, mtime, synced_at) VALUES (?, ?, ?, ?) ON CONFLICT(path, source) DO UPDATE SET mtime = excluded.mtime, synced_at = excluded.synced_at",
		path, src, mtime, time.Now(),
	)
	return err
}

// GetTokensByModel returns token usage grouped by model
func (db *DB) GetTokensByModel() (map[string]int64, error) {
	query := `
		SELECT model, SUM(input_tokens + output_tokens) as tokens
		FROM sessions
		WHERE model IS NOT NULL AND model != ''
		GROUP BY model
		ORDER BY tokens DESC`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query tokens by model: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int64)
	for rows.Next() {
		var model string
		var tokens int64
		if err := rows.Scan(&model, &tokens); err != nil {
			return nil, fmt.Errorf("scan tokens by model: %w", err)
		}
		result[model] = tokens
	}

	return result, rows.Err()
}
