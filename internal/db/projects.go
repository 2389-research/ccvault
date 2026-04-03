// ABOUTME: Database operations for projects
// ABOUTME: Provides CRUD operations for project records

package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// UpsertProject creates or updates a project record
func (db *DB) UpsertProject(p *models.Project) error {
	source := p.Source
	if source == "" {
		source = "claude-code"
	}

	query := `
		INSERT INTO projects (path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			display_name = excluded.display_name,
			last_activity_at = CASE
				WHEN excluded.last_activity_at > projects.last_activity_at
				THEN excluded.last_activity_at
				ELSE projects.last_activity_at
			END,
			session_count = projects.session_count + excluded.session_count,
			total_tokens = projects.total_tokens + excluded.total_tokens,
			source = excluded.source
		RETURNING id`

	err := db.QueryRow(query,
		p.Path,
		p.DisplayName,
		p.FirstSeenAt,
		p.LastActivityAt,
		p.SessionCount,
		p.TotalTokens,
		source,
	).Scan(&p.ID)

	if err != nil {
		return fmt.Errorf("upsert project: %w", err)
	}
	return nil
}

// UpsertProjectTx creates or updates a project record within a transaction
func (db *DB) UpsertProjectTx(tx *sql.Tx, p *models.Project) error {
	source := p.Source
	if source == "" {
		source = "claude-code"
	}

	query := `
		INSERT INTO projects (path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			display_name = excluded.display_name,
			last_activity_at = CASE
				WHEN excluded.last_activity_at > projects.last_activity_at
				THEN excluded.last_activity_at
				ELSE projects.last_activity_at
			END,
			session_count = projects.session_count + excluded.session_count,
			total_tokens = projects.total_tokens + excluded.total_tokens,
			source = excluded.source
		RETURNING id`

	err := tx.QueryRow(query,
		p.Path,
		p.DisplayName,
		p.FirstSeenAt,
		p.LastActivityAt,
		p.SessionCount,
		p.TotalTokens,
		source,
	).Scan(&p.ID)

	if err != nil {
		return fmt.Errorf("upsert project: %w", err)
	}
	return nil
}

// GetProject retrieves a project by ID
func (db *DB) GetProject(id int64) (*models.Project, error) {
	query := `
		SELECT id, path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source
		FROM projects WHERE id = ?`

	p := &models.Project{}
	err := db.QueryRow(query, id).Scan(
		&p.ID,
		&p.Path,
		&p.DisplayName,
		&p.FirstSeenAt,
		&p.LastActivityAt,
		&p.SessionCount,
		&p.TotalTokens,
		&p.Source,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

// GetProjectByPath retrieves a project by path
func (db *DB) GetProjectByPath(path string) (*models.Project, error) {
	query := `
		SELECT id, path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source
		FROM projects WHERE path = ?`

	p := &models.Project{}
	err := db.QueryRow(query, path).Scan(
		&p.ID,
		&p.Path,
		&p.DisplayName,
		&p.FirstSeenAt,
		&p.LastActivityAt,
		&p.SessionCount,
		&p.TotalTokens,
		&p.Source,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get project by path: %w", err)
	}
	return p, nil
}

// GetProjects retrieves all projects
func (db *DB) GetProjects(orderBy string, limit int) ([]models.Project, error) {
	validOrders := map[string]string{
		"name":     "display_name ASC",
		"activity": "last_activity_at DESC",
		"tokens":   "total_tokens DESC",
		"sessions": "session_count DESC",
	}

	order, ok := validOrders[orderBy]
	if !ok {
		order = "last_activity_at DESC"
	}

	query := fmt.Sprintf(`
		SELECT id, path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source
		FROM projects ORDER BY %s`, order)

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var projects []models.Project
	for rows.Next() {
		var p models.Project
		err := rows.Scan(
			&p.ID,
			&p.Path,
			&p.DisplayName,
			&p.FirstSeenAt,
			&p.LastActivityAt,
			&p.SessionCount,
			&p.TotalTokens,
			&p.Source,
		)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}

	return projects, rows.Err()
}

// GetProjectStats returns aggregate statistics for projects
func (db *DB) GetProjectStats() (count int, totalTokens int64, err error) {
	query := `SELECT COUNT(*), COALESCE(SUM(total_tokens), 0) FROM projects`
	err = db.QueryRow(query).Scan(&count, &totalTokens)
	if err != nil {
		return 0, 0, fmt.Errorf("get project stats: %w", err)
	}
	return count, totalTokens, nil
}

// UpdateProjectStats recalculates project statistics from sessions
func (db *DB) UpdateProjectStats(projectID int64) error {
	query := `
		UPDATE projects SET
			session_count = (SELECT COUNT(*) FROM sessions WHERE project_id = ?),
			total_tokens = (SELECT COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0) FROM sessions WHERE project_id = ?),
			last_activity_at = (SELECT MAX(ended_at) FROM sessions WHERE project_id = ?)
		WHERE id = ?`

	_, err := db.Exec(query, projectID, projectID, projectID, projectID)
	if err != nil {
		return fmt.Errorf("update project stats: %w", err)
	}
	return nil
}

// GetFirstAndLastActivity returns the date range of all activity
func (db *DB) GetFirstAndLastActivity() (first, last time.Time, err error) {
	query := `SELECT MIN(first_seen_at), MAX(last_activity_at) FROM projects`
	var firstStr, lastStr sql.NullString
	err = db.QueryRow(query).Scan(&firstStr, &lastStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("get activity range: %w", err)
	}
	if firstStr.Valid {
		first = parseTimeString(firstStr.String)
	}
	if lastStr.Valid {
		last = parseTimeString(lastStr.String)
	}
	return first, last, nil
}

// parseTimeString tries multiple formats to parse a time string from SQLite
func parseTimeString(s string) time.Time {
	// Handle Go's time.Time String() format from SQLite aggregates
	// e.g., "2026-02-13 17:50:43.387338 -0600 CST m=+0.003560667"
	if idx := strings.Index(s, " m="); idx > 0 {
		s = s[:idx]
	}

	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999 -0700 MST",
		"2006-01-02 15:04:05.999999999Z",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
