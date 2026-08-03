// ABOUTME: Sync logic for indexing conversations from configured sources
// ABOUTME: Iterates over source adapters to discover, parse, and populate the ccvault database

package sync

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/adapter"
	"github.com/2389-research/ccvault/pkg/models"
)

// Stats tracks sync statistics
type Stats struct {
	SessionsScanned          int
	SessionsIndexed          int
	SessionsSkipped          int
	SessionsWithSkippedLines int
	TotalSkippedLines        int
	TurnsIndexed             int
	ToolUsesIndexed          int
	ProjectsFound            int
	Errors                   []error
	Duration                 time.Duration
}

// Syncer handles syncing conversation data to ccvault
type Syncer struct {
	db              *db.DB
	sources         []config.SourceConfig
	full            bool
	verbose         bool
	onProgress      func(msg string)
	onCountProgress func(current, total int)
}

// Option configures a Syncer
type Option func(*Syncer)

// WithFullSync forces a complete rescan
func WithFullSync(full bool) Option {
	return func(s *Syncer) {
		s.full = full
	}
}

// WithVerbose enables verbose output
func WithVerbose(verbose bool) Option {
	return func(s *Syncer) {
		s.verbose = verbose
	}
}

// WithProgressCallback sets a callback for progress updates
func WithProgressCallback(fn func(string)) Option {
	return func(s *Syncer) {
		s.onProgress = fn
	}
}

// WithCountProgressCallback sets a callback for numeric progress (current/total)
func WithCountProgressCallback(fn func(current, total int)) Option {
	return func(s *Syncer) {
		s.onCountProgress = fn
	}
}

// New creates a new Syncer
func New(database *db.DB, sources []config.SourceConfig, opts ...Option) *Syncer {
	s := &Syncer{
		db:              database,
		sources:         sources,
		onProgress:      func(string) {},   // no-op default
		onCountProgress: func(int, int) {}, // no-op default
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run performs the sync operation
func (s *Syncer) Run() (*Stats, error) {
	start := time.Now()
	stats := &Stats{}

	// Batch-load all stored mtimes in one query for fast incremental checks
	var storedMtimes map[string]time.Time
	var err error
	if !s.full {
		storedMtimes, err = s.db.GetAllSourceMtimes()
		if err != nil {
			// Non-fatal: fall back to syncing everything
			s.progress("Warning: could not load mtimes, will rescan all")
			storedMtimes = make(map[string]time.Time)
		}
		s.progress("Loaded %d stored mtimes", len(storedMtimes))
	}

	// Track unique projects
	projectsSeen := make(map[string]bool)

	// Collect all session files across all sources
	type sourceSession struct {
		file       adapter.SessionFile
		sourceName string
		sourceType string
		adapter    adapter.SourceAdapter
	}
	var allSessions []sourceSession

	for _, src := range s.sources {
		s.progress("Scanning source %q (%s) at %s...", src.Name, src.Type, src.Path)

		adpt, err := adapter.Get(src.Type)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Errorf("source %q: %w", src.Name, err))
			s.progress("Error: unknown adapter type %q for source %q", src.Type, src.Name)
			continue
		}

		files, err := adpt.Discover(src.Path)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Errorf("source %q discover: %w", src.Name, err))
			s.progress("Error scanning source %q: %v", src.Name, err)
			continue
		}

		s.progress("Found %d session files in source %q", len(files), src.Name)

		for _, f := range files {
			allSessions = append(allSessions, sourceSession{
				file:       f,
				sourceName: src.Name,
				sourceType: src.Type,
				adapter:    adpt,
			})
		}
	}

	stats.SessionsScanned = len(allSessions)
	s.progress("Total: %d session files across %d sources", len(allSessions), len(s.sources))

	// Process each session
	total := len(allSessions)
	for i, ss := range allSessions {
		if err := s.processSession(ss.file, ss.adapter, ss.sourceName, stats, storedMtimes, projectsSeen); err != nil {
			stats.Errors = append(stats.Errors, fmt.Errorf("session %s: %w", ss.file.Path, err))
			if s.verbose {
				s.progress("Error processing %s: %v", ss.file.Path, err)
			}
		}

		s.onCountProgress(i+1, total)

		if (i+1)%100 == 0 || i == len(allSessions)-1 {
			s.progress("Processed %d/%d sessions", i+1, total)
		}
	}

	stats.ProjectsFound = len(projectsSeen)
	stats.Duration = time.Since(start)

	s.progress("Sync complete: %d sessions indexed, %d turns, %d tool uses",
		stats.SessionsIndexed, stats.TurnsIndexed, stats.ToolUsesIndexed)
	if stats.TotalSkippedLines > 0 {
		s.progress("Skipped %d malformed line(s) across %d session(s)",
			stats.TotalSkippedLines, stats.SessionsWithSkippedLines)
	}

	return stats, nil
}

// processSession handles a single session file using the given adapter
func (s *Syncer) processSession(sf adapter.SessionFile, adpt adapter.SourceAdapter, sourceName string, stats *Stats, storedMtimes map[string]time.Time, projectsSeen map[string]bool) error {
	// Check if we need to process this file
	if !s.full {
		if !s.needsSync(sf, storedMtimes) {
			stats.SessionsSkipped++
			// For skipped sessions, use scanner's path as best-effort approximation
			projectsSeen[sf.ProjectPath] = true
			return nil
		}
	}

	// Parse the session using the adapter
	parsed, err := adpt.Parse(sf.Path)
	if err != nil {
		return fmt.Errorf("parse session: %w", err)
	}

	if parsed.ID == "" {
		// Record mtime so we skip this empty file next time
		_ = s.db.UpsertSourceFileMtime(sf.Path, sf.ModTime, sourceName)
		stats.SessionsSkipped++
		return nil // Empty or invalid session
	}

	// Prefer CWD extracted from JSONL (ground truth) over scanner's lossy decode
	if parsed.ProjectPath == "" {
		parsed.ProjectPath = sf.ProjectPath
	}

	projectsSeen[parsed.ProjectPath] = true

	// Build models from parsed data
	session := &models.Session{
		ID:          parsed.ID,
		ProjectPath: parsed.ProjectPath,
		Model:       parsed.Model,
		GitBranch:   parsed.GitBranch,
		StartedAt:   parsed.StartedAt,
		EndedAt:     parsed.EndedAt,
		SourceFile:  sf.Path,
		Source:      sourceName,
	}

	// Detect session flags from adapter metadata
	if v, ok := parsed.Metadata["has_error"]; ok {
		if b, ok := v.(bool); ok {
			session.HasError = b
		}
	}
	if v, ok := parsed.Metadata["has_subagent"]; ok {
		if b, ok := v.(bool); ok {
			session.HasSubagent = b
		}
	}
	// Surface any lines the parser had to skip (see issue #11). We don't
	// persist this on the session — it's diagnostic output for the sync run.
	if v, ok := parsed.Metadata["skipped_lines"]; ok {
		if n, ok := v.(int); ok && n > 0 {
			stats.SessionsWithSkippedLines++
			stats.TotalSkippedLines += n
			if s.verbose {
				s.progress("session %s: skipped %d malformed line(s)", parsed.ID, n)
			}
		}
	}

	// Convert parsed turns to model turns and collect tool uses
	turns := make([]models.Turn, len(parsed.Turns))
	var toolUses []models.ToolUse
	for i, pt := range parsed.Turns {
		turns[i] = models.Turn{
			ID:           pt.ID,
			SessionID:    parsed.ID,
			ParentID:     pt.ParentID,
			Type:         pt.Type,
			Timestamp:    pt.Timestamp,
			Content:      pt.Content,
			RawJSON:      pt.RawJSON,
			InputTokens:  int(pt.InputTokens),
			OutputTokens: int(pt.OutputTokens),
		}

		// Accumulate session token totals
		session.InputTokens += pt.InputTokens
		session.OutputTokens += pt.OutputTokens

		// Convert tool uses
		for _, ptu := range pt.ToolUses {
			toolUses = append(toolUses, models.ToolUse{
				TurnID:    pt.ID,
				SessionID: parsed.ID,
				ToolName:  ptu.ToolName,
				FilePath:  ptu.FilePath,
				Timestamp: pt.Timestamp,
			})
		}
	}

	session.TurnCount = len(turns)

	// Store everything in a transaction
	err = s.db.WithTx(func(tx *sql.Tx) error {
		// Upsert project — use display name from adapter (source-specific logic)
		displayName := parsed.DisplayName
		if displayName == "" {
			displayName = session.ProjectPath
		}
		project := &models.Project{
			Path:           session.ProjectPath,
			DisplayName:    displayName,
			FirstSeenAt:    session.StartedAt,
			LastActivityAt: session.EndedAt,
			SessionCount:   1,
			TotalTokens:    session.TotalTokens(),
			Source:         sourceName,
		}
		if err := s.db.UpsertProjectTx(tx, project); err != nil {
			return fmt.Errorf("upsert project: %w", err)
		}

		// Set project ID on session
		session.ProjectID = project.ID

		// Delete existing turns for this session (for re-sync)
		if err := s.db.DeleteTurnsForSessionTx(tx, session.ID); err != nil {
			return fmt.Errorf("delete old turns: %w", err)
		}

		// Delete existing tool uses
		if err := s.db.DeleteToolUsesForSessionTx(tx, session.ID); err != nil {
			return fmt.Errorf("delete old tool uses: %w", err)
		}

		// Upsert session
		if err := s.db.UpsertSessionTx(tx, session); err != nil {
			return fmt.Errorf("upsert session: %w", err)
		}

		// Insert turns
		if err := s.db.InsertTurnsTx(tx, turns); err != nil {
			return fmt.Errorf("insert turns: %w", err)
		}

		// Insert tool uses
		if len(toolUses) > 0 {
			if err := s.db.InsertToolUsesTx(tx, toolUses); err != nil {
				return fmt.Errorf("insert tool uses: %w", err)
			}
		}

		// Record source file mtime so incremental sync skips this file next time
		if err := s.db.UpsertSourceFileMtimeTx(tx, sf.Path, sf.ModTime, sourceName); err != nil {
			return fmt.Errorf("upsert source mtime: %w", err)
		}

		return nil
	})

	if err != nil {
		return err
	}

	stats.SessionsIndexed++
	stats.TurnsIndexed += len(turns)
	stats.ToolUsesIndexed += len(toolUses)

	return nil
}

// needsSync checks if a session file needs to be synced using the pre-loaded mtime map
func (s *Syncer) needsSync(sf adapter.SessionFile, storedMtimes map[string]time.Time) bool {
	storedMtime, exists := storedMtimes[sf.Path]
	if !exists || storedMtime.IsZero() {
		return true // No stored time, needs sync
	}

	// Use the mtime from the directory scan (avoids a separate stat call per file)
	if sf.ModTime.IsZero() {
		return true // No mtime available, assume needs sync
	}

	return sf.ModTime.After(storedMtime)
}

// progress logs a progress message
func (s *Syncer) progress(format string, args ...interface{}) {
	s.onProgress(fmt.Sprintf(format, args...))
}
