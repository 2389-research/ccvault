// ABOUTME: Analytics queries for ccvault
// ABOUTME: Provides aggregate queries over session data via SQLite

package analytics

import (
	"fmt"
	"time"

	"github.com/2389-research/ccvault/internal/db"
)

// Analyzer runs analytics queries over session data
type Analyzer struct {
	db *db.DB
}

// NewAnalyzer creates a new SQLite-backed analyzer
func NewAnalyzer(database *db.DB) *Analyzer {
	return &Analyzer{db: database}
}

// Close is a no-op (DB lifecycle managed by caller)
func (a *Analyzer) Close() error {
	return nil
}

// TokensByDay returns token usage grouped by day
type DailyTokens struct {
	Date         time.Time `json:"date"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalTokens  int64     `json:"total_tokens"`
	SessionCount int       `json:"session_count"`
}

// GetTokensByDay returns token usage grouped by day
func (a *Analyzer) GetTokensByDay(days int) ([]DailyTokens, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	query := `
		SELECT
			DATE(started_at) as date,
			SUM(input_tokens) as input_tokens,
			SUM(output_tokens) as output_tokens,
			SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens) as total_tokens,
			COUNT(*) as session_count
		FROM sessions
		WHERE started_at > ?
		GROUP BY DATE(started_at)
		ORDER BY date DESC
		LIMIT ?`

	rows, err := a.db.Query(query, cutoff, days)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []DailyTokens
	for rows.Next() {
		var d DailyTokens
		var dateStr string
		if err := rows.Scan(&dateStr, &d.InputTokens, &d.OutputTokens, &d.TotalTokens, &d.SessionCount); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		d.Date, _ = time.Parse("2006-01-02", dateStr)
		results = append(results, d)
	}

	return results, rows.Err()
}

// ProjectStats represents aggregated project statistics
type ProjectStats struct {
	ProjectPath  string    `json:"project_path"`
	SessionCount int       `json:"session_count"`
	TotalTokens  int64     `json:"total_tokens"`
	LastActive   time.Time `json:"last_active"`
}

// GetTopProjects returns top projects by token usage
func (a *Analyzer) GetTopProjects(limit int) ([]ProjectStats, error) {
	query := `
		SELECT
			p.path as project_path,
			COUNT(*) as session_count,
			SUM(s.input_tokens + s.output_tokens + s.cache_read_tokens + s.cache_write_tokens) as total_tokens,
			MAX(s.started_at) as last_active
		FROM sessions s
		JOIN projects p ON s.project_id = p.id
		GROUP BY p.path
		ORDER BY total_tokens DESC
		LIMIT ?`

	rows, err := a.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []ProjectStats
	for rows.Next() {
		var p ProjectStats
		if err := rows.Scan(&p.ProjectPath, &p.SessionCount, &p.TotalTokens, &p.LastActive); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, p)
	}

	return results, rows.Err()
}

// ModelStats represents aggregated model statistics
type ModelStats struct {
	Model        string `json:"model"`
	SessionCount int    `json:"session_count"`
	TotalTokens  int64  `json:"total_tokens"`
}

// GetTokensByModel returns token usage grouped by model
func (a *Analyzer) GetTokensByModel() ([]ModelStats, error) {
	query := `
		SELECT
			model,
			COUNT(*) as session_count,
			SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens) as total_tokens
		FROM sessions
		WHERE model IS NOT NULL AND model != ''
		GROUP BY model
		ORDER BY total_tokens DESC`

	rows, err := a.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []ModelStats
	for rows.Next() {
		var m ModelStats
		if err := rows.Scan(&m.Model, &m.SessionCount, &m.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, m)
	}

	return results, rows.Err()
}

// Summary returns overall analytics summary
type Summary struct {
	TotalSessions int       `json:"total_sessions"`
	TotalTokens   int64     `json:"total_tokens"`
	FirstSession  time.Time `json:"first_session"`
	LastSession   time.Time `json:"last_session"`
	UniqueModels  int       `json:"unique_models"`
}

// GetSummary returns overall statistics
func (a *Analyzer) GetSummary() (*Summary, error) {
	query := `
		SELECT
			COUNT(*) as total_sessions,
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens), 0) as total_tokens,
			MIN(started_at) as first_session,
			MAX(started_at) as last_session,
			COUNT(DISTINCT model) as unique_models
		FROM sessions`

	var s Summary
	err := a.db.QueryRow(query).Scan(
		&s.TotalSessions,
		&s.TotalTokens,
		&s.FirstSession,
		&s.LastSession,
		&s.UniqueModels,
	)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}

	return &s, nil
}
