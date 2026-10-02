// ABOUTME: Sync logic for indexing conversations from configured sources
// ABOUTME: Iterates over source adapters to discover, parse, and populate the ccvault database

package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/adapter"
	"github.com/2389-research/ccvault/pkg/models"
)

// Stats tracks sync statistics
type Stats struct {
	SessionsScanned            int
	SessionsIndexed            int
	SessionsSkipped            int
	SessionsWithSkippedLines   int
	TotalSkippedLines          int
	SessionsWithTruncatedTurns int
	TurnsWithTruncatedRawJSON  int
	TurnsIndexed               int
	ToolUsesIndexed            int
	ProjectsFound              int
	Errors                     []error
	Duration                   time.Duration
}

// Syncer handles syncing conversation data to ccvault
type Syncer struct {
	db              *db.DB
	sources         []config.SourceConfig
	full            bool
	rebuild         bool
	verbose         bool
	cacheDir        string // analytics cache dir; full/rebuild invalidate it
	onProgress      func(msg string)
	onCountProgress func(current, total int)
}

// Option configures a Syncer
type Option func(*Syncer)

// WithFullSync re-parses every discovered session file, ignoring the mtime
// skip-check. It is NOT destructive: rows whose source file has since been
// pruned upstream stay in the archive. Each re-parsed file replaces its own
// rows as the scan reaches it.
func WithFullSync(full bool) Option {
	return func(s *Syncer) {
		s.full = full
	}
}

// WithRebuild clears every table before re-scanning, so the archive ends up
// holding exactly what is on disk right now. Destructive by design, and the
// only mode that is: anything the source has pruned is gone afterwards.
// Implies a full re-parse.
func WithRebuild(rebuild bool) Option {
	return func(s *Syncer) {
		s.rebuild = rebuild
	}
}

// WithCacheDir tells the Syncer where the analytics parquet cache lives so
// that a re-parse can invalidate it alongside the SQLite tables. Without
// this, `ccvault sync --full` or `--rebuild` rewrites SQLite but the DuckDB
// analytics view (TUI + MCP get_analytics) keeps reading stale numbers out
// of sessions.parquet.
func WithCacheDir(dir string) Option {
	return func(s *Syncer) {
		s.cacheDir = dir
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

// Run performs the sync operation. Cancelling ctx abandons the run: the
// current session's transaction is rolled back, the remaining sessions are
// skipped, and Run returns the stats gathered so far alongside ctx.Err().
// Sessions already committed stay committed — a cancelled sync is a short
// sync, not a failed one.
func (s *Syncer) Run(ctx context.Context) (*Stats, error) {
	start := time.Now()
	stats := &Stats{}

	// Track unique projects. projectsSeen counts everything discovered (for
	// stats); projectsTouched records only the projects we actually wrote a
	// session into, which is the set whose aggregates need reconciling.
	projectsSeen := make(map[string]bool)
	projectsTouched := make(map[string]bool)

	// cancelled wraps up a run that the caller abandoned. The partial stats go
	// back so the caller can still report what landed, and the project
	// aggregates get reconciled first: UpsertProject writes them as "existing
	// + incoming", so stopping mid-run without reconciling leaves the session
	// and token counters inflated in every view that reads them.
	cancelled := func() (*Stats, error) {
		s.reconcileProjects(stats, projectsTouched)
		stats.Duration = time.Since(start)
		return stats, ctx.Err()
	}

	if ctx.Err() != nil {
		return cancelled()
	}

	// Both --full and --rebuild re-parse everything, so numbers in the
	// DuckDB analytics cache can change either way; invalidate it so the
	// TUI Analytics tab and MCP get_analytics don't serve pre-sync figures.
	// The TUI auto-rebuilds when sessions.parquet is missing.
	//
	// This runs BEFORE the --rebuild wipe on purpose (#25): if removing the
	// parquet fails (read-only cache dir, EROFS, quota) we abort with the
	// archive still intact, rather than leaving SQLite empty behind a stale
	// cache that nothing warns the user about.
	if s.reparseAll() && s.cacheDir != "" {
		parquetPath := filepath.Join(s.cacheDir, "sessions.parquet")
		if err := os.Remove(parquetPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("invalidate analytics cache: %w", err)
		}
	}

	// --rebuild makes the archive a mirror of what's on disk right now, so
	// everything the source has pruned is discarded. --full never does this:
	// an archive that drops what the source no longer keeps isn't an archive.
	if s.rebuild {
		s.progress("Rebuild: clearing existing data...")
		if err := s.db.ResetAllContext(ctx); err != nil {
			return nil, fmt.Errorf("reset database: %w", err)
		}
	} else if s.full {
		// Said out loud because --full used to wipe the archive. Anyone whose
		// script or cron job still passes it should see what it does now.
		s.progress("Full sync: re-parsing every session file. Sessions whose source file is gone are preserved (use --rebuild to discard them).")
	}

	// Batch-load all stored mtimes in one query for fast incremental checks
	var storedMtimes map[string]time.Time
	var err error
	if !s.reparseAll() {
		storedMtimes, err = s.db.GetAllSourceMtimes()
		if err != nil {
			// Non-fatal: fall back to syncing everything
			s.progress("Warning: could not load mtimes, will rescan all")
			storedMtimes = make(map[string]time.Time)
		}
		s.progress("Loaded %d stored mtimes", len(storedMtimes))
	}

	// Collect all session files across all sources
	type sourceSession struct {
		file       adapter.SessionFile
		sourceName string
		sourceType string
		adapter    adapter.SourceAdapter
	}
	var allSessions []sourceSession

	for _, src := range s.sources {
		if ctx.Err() != nil {
			return cancelled()
		}
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
		if ctx.Err() != nil {
			return cancelled()
		}

		if err := s.processSession(ctx, ss.file, ss.adapter, ss.sourceName, stats, storedMtimes, projectsSeen, projectsTouched); err != nil {
			// A cancellation shows up here as a rolled-back transaction. That
			// is not a data problem, so it doesn't belong in stats.Errors.
			if ctx.Err() != nil {
				return cancelled()
			}
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

	s.reconcileProjects(stats, projectsTouched)

	stats.ProjectsFound = len(projectsSeen)
	stats.Duration = time.Since(start)

	s.progress("Sync complete: %d sessions indexed, %d turns, %d tool uses",
		stats.SessionsIndexed, stats.TurnsIndexed, stats.ToolUsesIndexed)
	if stats.TotalSkippedLines > 0 {
		s.progress("Skipped %d malformed line(s) across %d session(s)",
			stats.TotalSkippedLines, stats.SessionsWithSkippedLines)
	}
	if stats.TurnsWithTruncatedRawJSON > 0 {
		s.progress("Truncated raw_json for %d turn(s) across %d session(s)",
			stats.TurnsWithTruncatedRawJSON, stats.SessionsWithTruncatedTurns)
	}

	return stats, nil
}

// reconcileProjects recomputes the project aggregate columns against the
// sessions table. UpsertProject writes session_count / total_tokens as
// "existing + incoming", so every re-parse of an already-indexed session adds
// to them again. The old destructive --full hid that by emptying the table
// first; now that re-parsing preserves rows, the counters have to be recomputed
// from the rows that actually exist. Runs at the end of a complete sync and
// also when a run is cancelled part-way — a half-finished run inflates the
// counters just as surely as a finished one.
//
// Deliberately not context-bound: this is the repair step, and skipping it
// because the run was cancelled is what leaves the archive reading wrong.
func (s *Syncer) reconcileProjects(stats *Stats, projectsTouched map[string]bool) {
	if len(projectsTouched) == 0 {
		return
	}
	paths := make([]string, 0, len(projectsTouched))
	for p := range projectsTouched {
		paths = append(paths, p)
	}
	if err := s.db.ReconcileProjectAggregates(paths); err != nil {
		// Non-fatal: the session/turn rows are already committed, which is the
		// data that matters. Only the display counters are off.
		stats.Errors = append(stats.Errors, fmt.Errorf("reconcile project aggregates: %w", err))
		s.progress("Warning: could not reconcile project counts: %v", err)
	}
}

// processSession handles a single session file using the given adapter. ctx
// bounds the write transaction, so a cancellation partway through a large
// session rolls that session back instead of finishing it.
func (s *Syncer) processSession(ctx context.Context, sf adapter.SessionFile, adpt adapter.SourceAdapter, sourceName string, stats *Stats, storedMtimes map[string]time.Time, projectsSeen, projectsTouched map[string]bool) error {
	// Check if we need to process this file
	if !s.reparseAll() {
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

	// Aggregate parser diagnostics BEFORE the empty-session early-return.
	// A file that consists entirely of malformed JSONL lines has parsed.ID
	// == "" but still has skipped_lines / truncated turns we want to surface.
	// Falls back to sf.Path for the verbose label when we have no session ID.
	sessionLabel := parsed.ID
	if sessionLabel == "" {
		sessionLabel = sf.Path
	}
	if v, ok := parsed.Metadata["skipped_lines"]; ok {
		if n, ok := v.(int); ok && n > 0 {
			stats.SessionsWithSkippedLines++
			stats.TotalSkippedLines += n
			if s.verbose {
				s.progress("session %s: skipped %d malformed line(s)", sessionLabel, n)
			}
		}
	}
	if v, ok := parsed.Metadata["turns_with_truncated_raw_json"]; ok {
		if n, ok := v.(int); ok && n > 0 {
			stats.SessionsWithTruncatedTurns++
			stats.TurnsWithTruncatedRawJSON += n
			if s.verbose {
				s.progress("session %s: truncated raw_json for %d turn(s)", sessionLabel, n)
			}
		}
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
	projectsTouched[parsed.ProjectPath] = true

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
	// A subagent transcript is its own session row, linked to the session that
	// dispatched it. Adapters derive the parent id from the on-disk layout
	// (the transcript's own sessionId field is the parent's, which is why it
	// can't be used as an identity). Dropping this on the floor is what left
	// every previously-ingested sidechain row orphaned.
	if v, ok := parsed.Metadata["parent_session_id"]; ok {
		if s, ok := v.(string); ok {
			session.ParentSessionID = s
		}
	}
	// skipped_lines and turns_with_truncated_raw_json diagnostics are
	// aggregated immediately after Parse, above the empty-session guard,
	// so a file with no valid turns still contributes to sync totals.

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
	err = s.db.WithTxContext(ctx, func(tx *sql.Tx) error {
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

// reparseAll reports whether this run must re-parse every discovered file
// instead of consulting stored mtimes. Both --full and --rebuild do; they
// differ only in whether existing rows are wiped first.
func (s *Syncer) reparseAll() bool {
	return s.full || s.rebuild
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
