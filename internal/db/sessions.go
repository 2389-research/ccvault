// ABOUTME: Database operations for sessions
// ABOUTME: Provides CRUD operations for session records

package db

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// sessionWriter is the overlap between *sql.DB and *sql.Tx that upserting a
// session needs. It exists so UpsertSession and UpsertSessionTx share one
// implementation rather than two copies of a sixteen-column statement that
// have to be edited in lockstep.
type sessionWriter interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

const upsertSessionSQL = `
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
		source_file = CASE
			WHEN excluded.source_file != '' THEN excluded.source_file
			ELSE sessions.source_file
		END,
		source_mtime = excluded.source_mtime,
		has_error = excluded.has_error,
		has_subagent = excluded.has_subagent,
		source = excluded.source`

// UpsertSession creates or updates a session record. It runs in its own
// transaction so the row update and the source_files bookkeeping that follows
// a moved file either both land or neither does.
func (db *DB) UpsertSession(s *models.Session) error {
	return db.WithTx(func(tx *sql.Tx) error {
		return upsertSession(tx, s)
	})
}

// UpsertSessionTx creates or updates a session record within a transaction
func (db *DB) UpsertSessionTx(tx *sql.Tx, s *models.Session) error {
	return upsertSession(tx, s)
}

// upsertSession writes the session row and then follows the file if it moved.
//
// source_file is in the ON CONFLICT update list because a session's file does
// move — a project directory renamed, an archive relocated, an agent dir
// restructured. Leaving it out pinned the row to the original path for the
// life of the archive, so anything resolving a session back to its file got a
// path that no longer existed, and `sync --rebuild`'s confirmation prompt
// counted the session as having no file on disk, making a rebuild look more
// destructive than it was.
//
// Making the column updatable also made it clobberable, so the update guards
// against an empty incoming path the same way the statement already guards
// model: an upsert that carries no path keeps the one on the row.
// sessionsWithoutSourceFiles skips rows whose source_file is the empty
// string, so an empty value would drop the session out of the rebuild safety
// count altogether.
func upsertSession(w sessionWriter, s *models.Session) error {
	source := s.Source
	if source == "" {
		source = "claude-code"
	}

	// Read the path the row currently holds before overwriting it; the old
	// value is the only way to find the source_files row left behind.
	var previousFile string
	err := w.QueryRow("SELECT source_file FROM sessions WHERE id = ?", s.ID).Scan(&previousFile)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read current source file for session %s: %w", s.ID, err)
	}

	_, err = w.Exec(upsertSessionSQL,
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

	return forgetSupersededSourceFile(w, previousFile, s.SourceFile)
}

// forgetSupersededSourceFile drops the source_files row for a path a session
// has moved away from, so incremental sync stops tracking a file that is gone
// and the archive stops reporting a source file that cannot be opened.
//
// The on-disk check is what distinguishes a move from two live copies of the
// same session. If the old path still exists, both files are real and both
// mtimes are worth tracking — deleting either row would make incremental sync
// re-parse that file on every run, forever.
func forgetSupersededSourceFile(w sessionWriter, previousFile, currentFile string) error {
	if previousFile == "" || previousFile == currentFile {
		return nil
	}
	if _, err := os.Stat(previousFile); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		// Can't tell whether the file is gone (permissions, a dead mount).
		// Keeping the tracking row costs a re-parse; deleting one that is
		// still live costs a re-parse every sync. Keep it.
		return nil
	}

	if _, err := w.Exec("DELETE FROM source_files WHERE path = ?", previousFile); err != nil {
		return fmt.Errorf("delete superseded source file %s: %w", previousFile, err)
	}
	return nil
}

// GetSession retrieves a session by ID. A session with no project reads back
// with ProjectID 0.
//
// sessions.project_id is nullable, and MergeFrom's contract deliberately
// imports a session whose project row is missing from the incoming archive
// with a NULL rather than dropping it — the turns are the part worth keeping.
// So every read path here has to be able to represent "no project"; scanning
// project_id straight into models.Session.ProjectID (an int64) errors out on
// the NULL instead, which is what it used to do.
//
// Zero rather than a nullable field in models.Session, and zero rather than a
// NOT NULL column with a synthetic "unknown project" row, because:
//
//   - `ProjectID > 0` is already the tree-wide test for "has a project"
//     (cmd/ccvault, internal/tui, internal/mcp, internal/projectref all spell
//     it that way), so zero-means-absent is the convention already in force.
//   - GetSessionsPage already LEFT JOINs projects and COALESCEs the path, i.e.
//     this layer was written for sessions with no project from the start. Only
//     the project_id scan disagreed with the schema.
//   - A synthetic project row would be counted by GetProjectStats, listed by
//     `ccvault list-projects` and reported by `ccvault stats` — a repair that
//     shows up as a fake project in every aggregate.
//   - Making the column NOT NULL means rewriting the sessions table, a large
//     blast radius for a defect only reachable by importing an archive that is
//     already inconsistent.
func (db *DB) GetSession(id string) (*models.Session, error) {
	query := `
		SELECT id, project_id, started_at, ended_at, model, git_branch,
			turn_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			source_file, source
		FROM sessions WHERE id = ?`

	s := &models.Session{}
	var endedAt sql.NullTime
	var projectID sql.NullInt64
	err := db.QueryRow(query, id).Scan(
		&s.ID,
		&projectID,
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
	s.ProjectID = projectID.Int64
	if endedAt.Valid {
		s.EndedAt = endedAt.Time
	}
	return s, nil
}

// GetSessions retrieves sessions from the start of the sorted set,
// optionally filtered to one project.
func (db *DB) GetSessions(projectID int64, limit int) ([]models.Session, error) {
	return db.GetSessionsPage(projectID, limit, 0)
}

// GetSessionsPage retrieves one page of sessions: up to limit rows
// (0 means unlimited) starting at offset in the sorted set.
func (db *DB) GetSessionsPage(projectID int64, limit, offset int) ([]models.Session, error) {
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

	// `s.id ASC` is a tiebreaker, not a preference: sessions that share a
	// started_at would otherwise swap positions between calls and offset
	// pagination would skip or repeat rows.
	query += " ORDER BY s.started_at DESC, s.id ASC"
	query += limitOffsetClause(limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []models.Session
	for rows.Next() {
		var s models.Session
		var endedAt sql.NullTime
		var projectID sql.NullInt64
		err := rows.Scan(
			&s.ID,
			&projectID,
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
		s.ProjectID = projectID.Int64
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
	var projectID sql.NullInt64
	err := db.QueryRow(query, path).Scan(
		&s.ID,
		&projectID,
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
	s.ProjectID = projectID.Int64
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
