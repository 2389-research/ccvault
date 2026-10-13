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

// SessionsParquetMissingColumns reports which of the columns the current
// SessionRecord declares are absent from the on-disk sessions.parquet. An
// empty result means the cache carries today's schema; anything else means
// the caller should rebuild before querying, because a column DuckDB cannot
// find is a Binder Error at best and a wrong answer in a query written
// defensively around it.
//
// A missing file reports every expected column, so one call answers "should I
// rebuild?" for both the never-built and the outdated cache.
//
// This replaced a check for the literal column name `source`, which could not
// see `parent_session_id` when #147 added it (#149). The expectation is
// derived from the struct, so the next column is covered without touching
// this function.
func (a *Analyzer) SessionsParquetMissingColumns() ([]string, error) {
	expected := sessionRecordColumns()

	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")
	if _, err := os.Stat(sessionsPath); err != nil {
		if os.IsNotExist(err) {
			return expected, nil
		}
		return nil, fmt.Errorf("stat sessions parquet: %w", err)
	}

	// parquet_schema takes the path as a bound parameter, so the cache dir
	// never reaches the SQL text — unlike the read_parquet calls below, which
	// predate this and interpolate.
	rows, err := a.db.Query(`SELECT name FROM parquet_schema(?)`, sessionsPath)
	if err != nil {
		return nil, fmt.Errorf("inspect parquet schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan parquet schema: %w", err)
		}
		present[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read parquet schema: %w", err)
	}

	var missing []string
	for _, column := range expected {
		if _, ok := present[column]; !ok {
			missing = append(missing, column)
		}
	}
	return missing, nil
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

	// Alias intentionally not named `source`: on a legacy parquet without a
	// source column, DuckDB's binder shadows the missing column with the
	// SELECT alias and fails inside the COALESCE. Using `source_out` avoids
	// the collision and rows.Scan is positional so the Go-side struct field
	// mapping is unaffected.
	query := fmt.Sprintf(`
		SELECT
			COALESCE(source, 'claude-code') as source_out,
			SUM(total_tokens) as total_tokens
		FROM read_parquet('%s')
		GROUP BY 1
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

	// Alias intentionally not named `source` — see GetTokensBySource for
	// the DuckDB binder rationale.
	query := fmt.Sprintf(`
		SELECT
			COALESCE(source, 'claude-code') as source_out,
			COUNT(*) as session_count
		FROM read_parquet('%s')
		GROUP BY 1
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

// Agent roles a session can hold. A session either was dispatched by another
// session or it was not; `parent_session_id` carries that and nothing else.
const (
	AgentRoleTopLevel = "top-level"
	AgentRoleSubagent = "subagent"
)

// AgentRoleStats aggregates sessions by agent role. It stores counts only —
// the averages, which are the reason the split is worth drawing, are derived
// so no caller can arrive at a different per-session figure than another.
type AgentRoleStats struct {
	Role         string `json:"role"`
	SessionCount int    `json:"session_count"`
	TotalTurns   int64  `json:"total_turns"`
	TotalTokens  int64  `json:"total_tokens"`
}

// AvgTurnsPerSession returns turns per session, 0 for a role with none.
func (s AgentRoleStats) AvgTurnsPerSession() float64 {
	if s.SessionCount == 0 {
		return 0
	}
	return float64(s.TotalTurns) / float64(s.SessionCount)
}

// AvgTokensPerSession returns tokens per session, 0 for a role with none.
func (s AgentRoleStats) AvgTokensPerSession() float64 {
	if s.SessionCount == 0 {
		return 0
	}
	return float64(s.TotalTokens) / float64(s.SessionCount)
}

// GetStatsByAgentRole splits the cache into top-level and subagent sessions.
//
// Aggregate share is the less interesting half: on the author's archive
// subagents are 0.8% of sessions and 7.9% of tokens, so #74's headline claim
// of 49% of turns described files on disk rather than the archive (#111).
// What the split is for is that the two populations have very different
// shapes — 128.9 turns and 31,807 tokens a session against 21.5 and 2,851 —
// which makes any per-session average over the undivided pool a figure
// describing neither.
//
// Deliberately not written defensively around a missing parent_session_id: a
// COALESCE would report a pre-#147 cache as entirely top-level, which is a
// wrong answer wearing a right answer's clothes. Erroring lets the caller's
// staleness check (#149) rebuild instead.
func (a *Analyzer) GetStatsByAgentRole() ([]AgentRoleStats, error) {
	sessionsPath := filepath.Join(a.cacheDir, "sessions.parquet")

	// Check if parquet file exists
	if _, err := os.Stat(sessionsPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("analytics cache not found. Run 'ccvault build-cache' first")
	}

	// Alias intentionally not named `role`: see GetTokensBySource for the
	// DuckDB binder collision this avoids.
	query := fmt.Sprintf(`
		SELECT
			CASE WHEN parent_session_id <> '' THEN '%s' ELSE '%s' END as role_out,
			COUNT(*) as session_count,
			COALESCE(SUM(turn_count), 0) as total_turns,
			COALESCE(SUM(total_tokens), 0) as total_tokens
		FROM read_parquet('%s')
		GROUP BY 1
		ORDER BY session_count DESC
	`, AgentRoleSubagent, AgentRoleTopLevel, sessionsPath)

	rows, err := a.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []AgentRoleStats
	for rows.Next() {
		var s AgentRoleStats
		if err := rows.Scan(&s.Role, &s.SessionCount, &s.TotalTurns, &s.TotalTokens); err != nil {
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
