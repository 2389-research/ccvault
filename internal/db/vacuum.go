// ABOUTME: Storage accounting and whole-file compaction of the SQLite archive
// ABOUTME: Compacts with VACUUM INTO plus a verified atomic swap, never in place

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// dbFileName is the archive's single SQLite file inside the data dir.
const dbFileName = "ccvault.db"

// Thresholds for calling a database worth compacting. Both must be crossed:
// a ratio alone would nag about a 400 KB freelist in a tiny archive, and a
// byte count alone would nag about 40 MB of slack in a 40 GB file.
const (
	reclaimRatioThreshold = 0.25
	reclaimBytesThreshold = 32 << 20 // 32 MiB
)

// compactingSuffix names the temporary compacted copy. It lives beside the
// live database so the final rename stays on one filesystem and is atomic.
const compactingSuffix = ".compacting"

// Compaction needs room for the compacted copy to exist next to the
// original. headroom is added on top of the estimated copy size to cover
// SQLite's own temp space for index rebuilds and page rounding.
const (
	compactHeadroomFraction = 10 // one tenth of the estimated copy
	compactHeadroomFloor    = 16 << 20
)

// compactBusyTimeoutMS bounds how long compaction waits for other readers
// and writers to let go of the database before it gives up.
const compactBusyTimeoutMS = 1000

var (
	// ErrNoDatabase reports that there is nothing at the expected path to
	// compact. Compaction must never create a database as a side effect.
	ErrNoDatabase = errors.New("no ccvault database found")

	// ErrInsufficientDiskSpace reports that the compacted copy would not
	// fit beside the original. Returned before anything is written.
	ErrInsufficientDiskSpace = errors.New("insufficient disk space to compact")

	// ErrDatabaseBusy reports that another connection holds the database, so
	// compaction cannot guarantee the copy matches the file it replaces.
	ErrDatabaseBusy = errors.New("database is in use by another process")

	// errFreeSpaceUnsupported tells the compaction preflight that this
	// platform has no probe, so it should proceed without one. Compaction is
	// still safe without it: a full disk fails the copy, and a failed copy
	// leaves the original untouched.
	errFreeSpaceUnsupported = errors.New("free disk space is not reported on this platform")
)

// StorageStats reports how much of the database file is live data and how
// much is freelist — pages SQLite has released internally but never returned
// to the filesystem. Incremental sync replaces turn rows on every changed
// session file, so the freelist grows without bound relative to the data.
type StorageStats struct {
	// FileBytes is the size of the main database file on disk. Sidecars
	// (-wal, -journal) are excluded: they are transient and small.
	FileBytes int64
	// PageSize, PageCount and FreelistCount come straight from the
	// same-named pragmas.
	PageSize      int64
	PageCount     int64
	FreelistCount int64
}

// ReclaimableBytes is how much of the file compaction would hand back.
func (s StorageStats) ReclaimableBytes() int64 {
	return s.FreelistCount * s.PageSize
}

// LiveBytes estimates the size of a compacted copy: the pages holding data.
func (s StorageStats) LiveBytes() int64 {
	live := s.PageCount - s.FreelistCount
	if live < 0 {
		live = 0
	}
	return live * s.PageSize
}

// FreelistRatio is the dead fraction of the file, 0 for an empty database.
func (s StorageStats) FreelistRatio() float64 {
	if s.PageCount <= 0 {
		return 0
	}
	return float64(s.FreelistCount) / float64(s.PageCount)
}

// WorthReclaiming reports whether the waste is large enough, both in share
// of the file and in absolute bytes, to be worth telling the user about.
func (s StorageStats) WorthReclaiming() bool {
	return s.FreelistRatio() >= reclaimRatioThreshold &&
		s.ReclaimableBytes() >= reclaimBytesThreshold
}

// StorageStats reports page accounting for the open database.
func (db *DB) StorageStats() (StorageStats, error) {
	return storageStats(context.Background(), db.DB, db.path)
}

// CompactResult describes one compaction run.
type CompactResult struct {
	Path     string
	Before   StorageStats
	After    StorageStats
	Duration time.Duration
}

// BytesReclaimed is the drop in on-disk file size.
func (r CompactResult) BytesReclaimed() int64 {
	return r.Before.FileBytes - r.After.FileBytes
}

// CompactOption configures a compaction run.
type CompactOption func(*compactConfig)

type compactConfig struct {
	progress func(string)
	// afterCopy runs once the compacted copy exists but before it has been
	// verified or swapped in. It exists so tests can damage the copy or
	// race a writer against the lock; nothing in production sets it.
	afterCopy func(tmpPath string) error
}

// WithCompactProgress reports each step. Compaction of a multi-gigabyte
// archive takes tens of seconds, so silence looks like a hang.
func WithCompactProgress(fn func(string)) CompactOption {
	return func(c *compactConfig) { c.progress = fn }
}

func withAfterCopy(fn func(tmpPath string) error) CompactOption {
	return func(c *compactConfig) { c.afterCopy = fn }
}

// Compact reclaims the freelist of the archive in dataDir by writing a
// compacted copy with VACUUM INTO, verifying it, and renaming it over the
// original.
//
// It is deliberately not an in-place VACUUM. In-place VACUUM needs free
// space roughly equal to the whole file including its dead pages, and it
// rewrites the live file, so a crash or a full disk partway through leaves
// the user holding the wreckage. Copy-verify-swap needs only the size of
// the live data, leaves the original untouched until a verified replacement
// exists, and finishes with a single atomic rename. Every failure path
// before that rename leaves the original database exactly as it was.
//
// The database is held under an exclusive lock for the whole run, so the
// copy cannot drift from the file it replaces. Compaction refuses rather
// than waiting indefinitely if another process is using the archive.
func Compact(dataDir string, opts ...CompactOption) (CompactResult, error) {
	cfg := compactConfig{progress: func(string) {}}
	for _, opt := range opts {
		opt(&cfg)
	}

	dbPath := filepath.Join(dataDir, dbFileName)
	info, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return CompactResult{}, fmt.Errorf("%w at %s", ErrNoDatabase, dbPath)
		}
		return CompactResult{}, fmt.Errorf("stat database: %w", err)
	}
	// VACUUM INTO creates the copy under the process umask, which is wider
	// than the mode of a database holding someone's conversations.
	liveMode := info.Mode().Perm()

	start := time.Now()
	result := CompactResult{Path: dbPath}
	ctx := context.Background()

	// Open without running migrations: compaction is schema-agnostic, and
	// the command that shrinks a database should not also be the one that
	// changes its schema.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)", dbPath, compactBusyTimeoutMS)
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return result, fmt.Errorf("open database: %w", err)
	}
	pool.SetMaxOpenConns(1)
	poolOpen := true
	closePool := func() error {
		if !poolOpen {
			return nil
		}
		poolOpen = false
		return pool.Close()
	}
	defer func() { _ = closePool() }()

	// Pin one connection: locking_mode is per-connection, so the lock has to
	// be taken and held on the same handle that does the copy.
	conn, err := pool.Conn(ctx)
	if err != nil {
		return result, fmt.Errorf("acquire connection: %w", err)
	}
	connOpen := true
	closeConn := func() error {
		if !connOpen {
			return nil
		}
		connOpen = false
		return conn.Close()
	}
	defer func() { _ = closeConn() }()

	result.Before, err = storageStats(ctx, conn, dbPath)
	if err != nil {
		return result, err
	}

	if err := checkCompactSpace(dataDir, result.Before); err != nil {
		return result, err
	}

	if err := lockExclusive(ctx, conn); err != nil {
		return result, err
	}

	dataVersionBefore, err := dataVersion(ctx, conn)
	if err != nil {
		return result, err
	}

	tmpPath := dbPath + compactingSuffix
	// A leftover copy means a previous run died before its rename. It is
	// scratch by definition — the live database was never touched — so it
	// can go.
	if _, err := os.Stat(tmpPath); err == nil {
		cfg.progress(fmt.Sprintf("removing leftover copy from an earlier run: %s", tmpPath))
		if err := os.Remove(tmpPath); err != nil {
			return result, fmt.Errorf("remove leftover copy: %w", err)
		}
	}

	swapped := false
	defer func() {
		if !swapped {
			_ = os.Remove(tmpPath)
		}
	}()

	cfg.progress(fmt.Sprintf("writing compacted copy (%s of live data)", formatBytes(result.Before.LiveBytes())))
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escapeSQLiteString(tmpPath))); err != nil {
		return result, fmt.Errorf("vacuum into %s: %w", tmpPath, err)
	}

	if cfg.afterCopy != nil {
		if err := cfg.afterCopy(tmpPath); err != nil {
			return result, fmt.Errorf("after-copy hook: %w", err)
		}
	}

	if err := os.Chmod(tmpPath, liveMode); err != nil {
		return result, fmt.Errorf("match permissions of compacted copy to the live database: %w", err)
	}

	cfg.progress("verifying compacted copy")
	sourceCounts, err := tableRowCounts(ctx, conn)
	if err != nil {
		return result, fmt.Errorf("count rows in live database: %w", err)
	}
	if err := verifyCompactedCopy(ctx, tmpPath, sourceCounts); err != nil {
		return result, err
	}

	// Belt and braces behind the exclusive lock: data_version changes if any
	// other connection commits to the database we just copied.
	dataVersionAfter, err := dataVersion(ctx, conn)
	if err != nil {
		return result, err
	}
	if dataVersionAfter != dataVersionBefore {
		return result, fmt.Errorf("%w: it was written to while being copied (data_version %d -> %d)",
			ErrDatabaseBusy, dataVersionBefore, dataVersionAfter)
	}

	// Fold any WAL content into the main file, then let go of the database
	// before moving files around underneath it.
	if err := checkpointIfWAL(ctx, conn); err != nil {
		return result, err
	}
	if err := closeConn(); err != nil {
		return result, fmt.Errorf("close connection: %w", err)
	}
	if err := closePool(); err != nil {
		return result, fmt.Errorf("close database: %w", err)
	}

	// Sidecars describe the file we are about to replace. After a clean
	// close they hold nothing the main file does not, and one left beside
	// the compacted copy would be read as belonging to it. Everything up to
	// here, including this, leaves a complete and valid database in place.
	cfg.progress("swapping compacted copy over the live database")
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return result, fmt.Errorf("remove %s%s: %w", dbPath, suffix, err)
		}
	}

	if err := os.Rename(tmpPath, dbPath); err != nil {
		return result, fmt.Errorf("swap compacted copy into place: %w", err)
	}
	swapped = true

	result.After, err = readOnlyStorageStats(ctx, dbPath)
	if err != nil {
		return result, fmt.Errorf("read back compacted database: %w", err)
	}
	result.Duration = time.Since(start)
	return result, nil
}

// sqlRunner is the overlap between *sql.DB and *sql.Conn that compaction
// needs, so the same helpers serve a pool and a pinned connection.
type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func storageStats(ctx context.Context, q sqlRunner, path string) (StorageStats, error) {
	var stats StorageStats

	info, err := os.Stat(path)
	if err != nil {
		return stats, fmt.Errorf("stat database file: %w", err)
	}
	stats.FileBytes = info.Size()

	for _, p := range []struct {
		pragma string
		dest   *int64
	}{
		{"page_size", &stats.PageSize},
		{"page_count", &stats.PageCount},
		{"freelist_count", &stats.FreelistCount},
	} {
		if err := q.QueryRowContext(ctx, "PRAGMA "+p.pragma).Scan(p.dest); err != nil {
			return stats, fmt.Errorf("pragma %s: %w", p.pragma, err)
		}
	}

	return stats, nil
}

// readOnlyStorageStats reads page accounting without opening the database
// for writing, so reporting on a file never creates a journal beside it.
func readOnlyStorageStats(ctx context.Context, dbPath string) (StorageStats, error) {
	pool, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return StorageStats{}, fmt.Errorf("open read-only: %w", err)
	}
	defer func() { _ = pool.Close() }()
	return storageStats(ctx, pool, dbPath)
}

// checkCompactSpace refuses the run when the compacted copy would not fit
// beside the original, rather than discovering it partway through a
// multi-gigabyte write.
func checkCompactSpace(dataDir string, before StorageStats) error {
	free, err := availableBytes(dataDir)
	if err != nil {
		if errors.Is(err, errFreeSpaceUnsupported) {
			// No probe on this platform. VACUUM INTO will fail on a full
			// disk without touching the original, so proceed.
			return nil
		}
		return fmt.Errorf("check free disk space: %w", err)
	}

	needed := before.LiveBytes()
	headroom := needed / compactHeadroomFraction
	if headroom < compactHeadroomFloor {
		headroom = compactHeadroomFloor
	}
	needed += headroom

	if free < needed {
		return fmt.Errorf("%w: the compacted copy needs about %s beside the original, but only %s is free on %s",
			ErrInsufficientDiskSpace, formatBytes(needed), formatBytes(free), dataDir)
	}
	return nil
}

// lockExclusive takes a file-level exclusive lock that is held until the
// connection closes, so nothing can read or write the database between the
// copy and the swap. SQLite only takes the exclusive lock on the first
// write under locking_mode=EXCLUSIVE, so a write is what claims it: setting
// user_version to the value it already has dirties the header page without
// changing anything the archive cares about.
func lockExclusive(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "PRAGMA locking_mode = EXCLUSIVE"); err != nil {
		return fmt.Errorf("set exclusive locking mode: %w", err)
	}

	var userVersion int64
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", userVersion)); err != nil {
		if isBusyError(err) {
			return fmt.Errorf("%w: close other ccvault processes (TUI, MCP server, sync) and try again: %w",
				ErrDatabaseBusy, err)
		}
		return fmt.Errorf("take exclusive lock: %w", err)
	}
	return nil
}

func dataVersion(ctx context.Context, q sqlRunner) (int64, error) {
	var v int64
	if err := q.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("pragma data_version: %w", err)
	}
	return v, nil
}

// checkpointIfWAL folds WAL content into the main file. It is a no-op in
// rollback-journal mode, where there is no WAL to fold.
func checkpointIfWAL(ctx context.Context, q sqlRunner) error {
	var mode string
	if err := q.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("pragma journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return nil
	}
	if _, err := q.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint wal: %w", err)
	}
	return nil
}

// verifyCompactedCopy refuses to let a copy replace a good database unless
// SQLite calls it structurally sound and every table holds the same number
// of rows as the original. integrity_check alone would pass a copy that is
// valid but missing rows.
func verifyCompactedCopy(ctx context.Context, tmpPath string, sourceCounts map[string]int64) error {
	pool, err := sql.Open("sqlite", "file:"+tmpPath+"?mode=ro")
	if err != nil {
		return fmt.Errorf("open compacted copy: %w", err)
	}
	defer func() { _ = pool.Close() }()

	var check string
	if err := pool.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return fmt.Errorf("integrity check on compacted copy: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("compacted copy failed integrity check: %s", check)
	}

	copyCounts, err := tableRowCounts(ctx, pool)
	if err != nil {
		return fmt.Errorf("count rows in compacted copy: %w", err)
	}

	for table, want := range sourceCounts {
		got, ok := copyCounts[table]
		if !ok {
			return fmt.Errorf("compacted copy is missing table %s", table)
		}
		if got != want {
			return fmt.Errorf("compacted copy has %d rows in %s, live database has %d", got, table, want)
		}
	}
	for table := range copyCounts {
		if _, ok := sourceCounts[table]; !ok {
			return fmt.Errorf("compacted copy has unexpected table %s", table)
		}
	}
	return nil
}

// tableRowCounts counts every real table, including the FTS5 shadow tables
// that hold the search index. The fts5 virtual table itself is skipped: its
// count is derived from the turns table it indexes, so counting it would
// re-scan that table for nothing.
func tableRowCounts(ctx context.Context, q sqlRunner) (map[string]int64, error) {
	names, err := listTables(ctx, q)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int64, len(names))
	for _, name := range names {
		var count int64
		query := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, strings.ReplaceAll(name, `"`, `""`))
		if err := q.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		counts[name] = count
	}
	return counts, nil
}

// listTables names every real table in the schema, skipping SQLite's own
// bookkeeping and the fts5 virtual tables.
func listTables(ctx context.Context, q sqlRunner) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT name FROM sqlite_master
		WHERE type = 'table'
		  AND name NOT LIKE 'sqlite_%'
		  AND sql NOT LIKE 'CREATE VIRTUAL TABLE%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan table name: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return names, nil
}

// isBusyError reports whether err is SQLite refusing because someone else
// holds the database.
func isBusyError(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "database table is locked")
}

// escapeSQLiteString renders a Go string as the body of a SQLite single-
// quoted literal. VACUUM INTO takes a literal, not a bound parameter.
func escapeSQLiteString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// formatBytes renders a byte count for error messages and progress lines.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}
