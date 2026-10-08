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

// UpsertProject creates or updates a project record.
// Projects are keyed by path alone (not path+source), so multiple sources working on
// the same project directory aggregate into a single project row. The source field
// reflects the last source that synced a session for this project.
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

// GetProjects retrieves all projects from the start of the sorted set.
func (db *DB) GetProjects(orderBy string, limit int) ([]models.Project, error) {
	return db.GetProjectsPage(orderBy, limit, 0)
}

// GetProjectsPage retrieves one page of projects: up to limit rows
// (0 means unlimited) starting at offset in the sorted set.
func (db *DB) GetProjectsPage(orderBy string, limit, offset int) ([]models.Project, error) {
	// Every sort adds `path ASC` as a tiebreaker so results are stable
	// across syncs even when the primary key (display_name, timestamp,
	// count) collides — e.g. multiple projects sharing a basename don't
	// swap positions between calls, and agent pagination stays coherent.
	validOrders := map[string]string{
		"name":     "display_name ASC, path ASC",
		"activity": "last_activity_at DESC, path ASC",
		"tokens":   "total_tokens DESC, path ASC",
		"sessions": "session_count DESC, path ASC",
	}

	order, ok := validOrders[orderBy]
	if !ok {
		order = "last_activity_at DESC, path ASC"
	}

	query := fmt.Sprintf(`
		SELECT id, path, display_name, first_seen_at, last_activity_at, session_count, total_tokens, source
		FROM projects ORDER BY %s`, order)
	query += limitOffsetClause(limit, offset)

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

// ReconcileProjectAggregates recomputes session_count and total_tokens from
// the sessions table for the given project paths. Pass no paths to reconcile
// every project.
//
// Those two columns are maintained additively by UpsertProject ("existing +
// incoming"), so re-parsing a session file that is already indexed inflates
// them. Recomputing from the rows that actually exist is the only way to get
// them back in step, and it is cheap: sessions are indexed by project_id.
// Each token counter is COALESCEd before it is added, not just the SUM
// afterwards. Addition in SQL yields NULL if any operand is NULL and SUM skips
// a NULL input rather than failing on it, so a session holding one NULL counter
// used to contribute nothing at all to its project's total — a silent
// undercount in the very function that exists to stop these counters drifting.
// Same shape GetSessionStats fixed for #64; see #110.
func (db *DB) ReconcileProjectAggregates(paths []string) error {
	// first_seen_at and last_activity_at are deliberately left alone: the
	// former is write-once at insert, the latter is already updated with a
	// max() comparison, so neither drifts on re-parse.
	//
	// That exclusion is the whole reason this function exists rather than a
	// recompute of every aggregate column. A `MAX(ended_at)` over the stored
	// datetime text is a string compare over values the driver formatted with
	// a timezone suffix, so it can pick an earlier moment as the maximum —
	// which would make last_activity_at worse than leaving it untouched.
	const recompute = `
		UPDATE projects SET
			session_count = (
				SELECT COUNT(*) FROM sessions WHERE sessions.project_id = projects.id
			),
			total_tokens = (
				SELECT COALESCE(SUM(COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0)
					+ COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0)), 0)
				FROM sessions WHERE sessions.project_id = projects.id
			)`

	if len(paths) == 0 {
		if _, err := db.Exec(recompute); err != nil {
			return fmt.Errorf("reconcile project aggregates: %w", err)
		}
		return nil
	}

	// Chunked so a sync touching thousands of projects can't exceed SQLite's
	// bound-parameter ceiling.
	const chunk = 500
	for start := 0; start < len(paths); start += chunk {
		end := min(start+chunk, len(paths))
		batch := paths[start:end]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, len(batch))
		for i, p := range batch {
			placeholders[i] = "?"
			args[i] = p
		}

		query := recompute + " WHERE path IN (" + strings.Join(placeholders, ", ") + ")"
		if _, err := db.Exec(query, args...); err != nil {
			return fmt.Errorf("reconcile project aggregates: %w", err)
		}
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
