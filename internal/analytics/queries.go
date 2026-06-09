// ABOUTME: DuckDB analytics queries for ccvault
// ABOUTME: Provides fast aggregate queries over Parquet data

package analytics

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/marcboeker/go-duckdb"
)

// Analyzer runs analytics queries over Parquet data
type Analyzer struct {
	db       *sql.DB
	cacheDir string
}

// NewAnalyzer creates a new DuckDB analyzer
func NewAnalyzer(cacheDir string) (*Analyzer, error) {
	// Open DuckDB in-memory
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open duckdb: %w", err)
	}

	return &Analyzer{
		db:       db,
		cacheDir: cacheDir,
	}, nil
}

// Close closes the DuckDB connection
func (a *Analyzer) Close() error {
	return a.db.Close()
}

// SessionsParquetHasSource returns true when the on-disk sessions.parquet has a
// `source` column. Older parquet files written before multi-source support
// don't, and source-aware queries against them would fail; callers should
// rebuild the cache when this returns false. A missing file also returns false.
func (a *Analyzer) SessionsParquetHasSource() (bool, error) {
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")
	if _, err := os.Stat(sessionsPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat sessions parquet: %w", err)
	}

	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM parquet_schema('%s') WHERE name = 'source'`,
		sessionsPath,
	)
	var count int
	if err := a.db.QueryRow(query).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect parquet schema: %w", err)
	}
	return count > 0, nil
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
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")

	// Check if parquet file exists
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("analytics cache not found. Run 'ccvault build-cache' first")
	}

	// started_at is stored as TIMESTAMP_MILLIS in Parquet, DuckDB reads it as timestamp
	cutoff := time.Now().AddDate(0, 0, -days)
	query := fmt.Sprintf(`
		SELECT
			DATE_TRUNC('day', started_at) as date,
			SUM(input_tokens) as input_tokens,
			SUM(output_tokens) as output_tokens,
			SUM(total_tokens) as total_tokens,
			COUNT(*) as session_count
		FROM read_parquet('%s')
		WHERE started_at > TIMESTAMP '%s'
		GROUP BY DATE_TRUNC('day', started_at)
		ORDER BY date DESC
		LIMIT %d
	`, sessionsPath, cutoff.Format("2006-01-02 15:04:05"), days)

	rows, err := a.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []DailyTokens
	for rows.Next() {
		var d DailyTokens
		if err := rows.Scan(&d.Date, &d.InputTokens, &d.OutputTokens, &d.TotalTokens, &d.SessionCount); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
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
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")

	// Check if parquet file exists
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("analytics cache not found. Run 'ccvault build-cache' first")
	}

	// started_at is already a timestamp from Parquet TIMESTAMP_MILLIS
	query := fmt.Sprintf(`
		SELECT
			project_path,
			COUNT(*) as session_count,
			SUM(total_tokens) as total_tokens,
			MAX(started_at) as last_active
		FROM read_parquet('%s')
		GROUP BY project_path
		ORDER BY total_tokens DESC
		LIMIT %d
	`, sessionsPath, limit)

	rows, err := a.db.Query(query)
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
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")

	// Check if parquet file exists
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("analytics cache not found. Run 'ccvault build-cache' first")
	}

	query := fmt.Sprintf(`
		SELECT
			model,
			COUNT(*) as session_count,
			SUM(total_tokens) as total_tokens
		FROM read_parquet('%s')
		WHERE model IS NOT NULL AND model != ''
		GROUP BY model
		ORDER BY total_tokens DESC
	`, sessionsPath)

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

// SourceTokenStats represents token usage grouped by source
type SourceTokenStats struct {
	Source      string `json:"source"`
	TotalTokens int64  `json:"total_tokens"`
}

// GetTokensBySource returns total tokens grouped by source
func (a *Analyzer) GetTokensBySource() ([]SourceTokenStats, error) {
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")

	// Check if parquet file exists
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("analytics cache not found. Run 'ccvault build-cache' first")
	}

	query := fmt.Sprintf(`
		SELECT
			COALESCE(source, 'claude-code') as source,
			SUM(total_tokens) as total_tokens
		FROM read_parquet('%s')
		GROUP BY COALESCE(source, 'claude-code')
		ORDER BY total_tokens DESC
	`, sessionsPath)

	rows, err := a.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []SourceTokenStats
	for rows.Next() {
		var s SourceTokenStats
		if err := rows.Scan(&s.Source, &s.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, s)
	}

	return results, rows.Err()
}

// SourceSessionStats represents session count grouped by source
type SourceSessionStats struct {
	Source       string `json:"source"`
	SessionCount int    `json:"session_count"`
}

// GetSessionsBySource returns session count grouped by source
func (a *Analyzer) GetSessionsBySource() ([]SourceSessionStats, error) {
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")

	// Check if parquet file exists
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("analytics cache not found. Run 'ccvault build-cache' first")
	}

	query := fmt.Sprintf(`
		SELECT
			COALESCE(source, 'claude-code') as source,
			COUNT(*) as session_count
		FROM read_parquet('%s')
		GROUP BY COALESCE(source, 'claude-code')
		ORDER BY session_count DESC
	`, sessionsPath)

	rows, err := a.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []SourceSessionStats
	for rows.Next() {
		var s SourceSessionStats
		if err := rows.Scan(&s.Source, &s.SessionCount); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		results = append(results, s)
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
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")

	// Check if parquet file exists
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("analytics cache not found. Run 'ccvault build-cache' first")
	}

	// started_at is already a timestamp from Parquet TIMESTAMP_MILLIS
	query := fmt.Sprintf(`
		SELECT
			COUNT(*) as total_sessions,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			MIN(started_at) as first_session,
			MAX(started_at) as last_session,
			COUNT(DISTINCT model) as unique_models
		FROM read_parquet('%s')
	`, sessionsPath)

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
