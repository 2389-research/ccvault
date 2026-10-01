// ABOUTME: Database connection management and initialization
// ABOUTME: Provides SQLite connection with FTS5 support for ccvault

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite database connection
type DB struct {
	*sql.DB
	path string
}

// Open opens or creates the ccvault database
func Open(dataDir string) (*DB, error) {
	// Ensure data directory exists
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "ccvault.db")

	// Open database with WAL mode for better concurrency
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Set connection pool settings
	sqlDB.SetMaxOpenConns(1) // SQLite works best with single writer
	sqlDB.SetMaxIdleConns(1)

	db := &DB{
		DB:   sqlDB,
		path: dbPath,
	}

	// Initialize schema
	if err := db.init(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return db, nil
}

// init creates the database schema via the versioned migration system
func (db *DB) init() error {
	sqlDB := db.DB
	if err := RunMigrations(sqlDB); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

// Path returns the database file path
func (db *DB) Path() string {
	return db.path
}

// Close closes the database connection
func (db *DB) Close() error {
	return db.DB.Close()
}

// ResetAll deletes all data from all tables so `sync --rebuild` can repopulate
// from a clean slate. It is the archive's only destructive operation; ordinary
// and `--full` syncs never call it.
// Schema (tables, triggers, FTS index) is left in place — migrations own
// its lifecycle. Deleting turns cascades to turns_fts via the AFTER DELETE
// trigger, so no FTS drop is needed here.
//
// The five DELETEs run inside a single transaction so mid-reset failure
// (disk full, SIGKILL, SQLITE_BUSY on a WAL-mode writer collision) rolls
// back to the pre-reset state. Otherwise a partial reset would leave
// tool_uses empty but turns still populated, and the per-session /
// per-project aggregate counters would silently drift from row counts.
func (db *DB) ResetAll() error {
	return db.ResetAllContext(context.Background())
}

// ResetAllContext is ResetAll bound to ctx. Wiping a large archive is one long
// transaction, so a caller that can be cancelled mid-sync needs to be able to
// interrupt it; cancelling rolls the whole wipe back, same as any other
// failure part-way through.
func (db *DB) ResetAllContext(ctx context.Context) error {
	// Delete data in child-to-parent order so foreign-key-like invariants hold
	// (turns before sessions, sessions before projects, etc.)
	tables := []string{"tool_uses", "turns", "sessions", "projects", "source_files"}
	return db.WithTxContext(ctx, func(tx *sql.Tx) error {
		for _, table := range tables {
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return fmt.Errorf("delete from %s: %w", table, err)
			}
		}
		return nil
	})
}

// BackupTo writes a complete portable copy of the current SQLite state to
// the given path. Uses `VACUUM INTO`, which works while the DB is open,
// includes all schema + data, and produces a compacted single-file backup
// with no WAL sidecar. Errors when the target path already exists — the
// caller is responsible for choosing a fresh path (typically timestamp-
// suffixed) so an accidental repeat can't clobber a good backup.
//
// Used by `sync --rebuild` to snapshot the archive before ResetAll fires, so
// even if the subsequent re-scan produces bad state, the user can restore the
// pre-rebuild DB by copying the backup file back over the live one — or merge
// it back in with `ccvault import`.
func (db *DB) BackupTo(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backup path already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat backup path: %w", err)
	}
	// SQLite refuses paths that don't exist as directories, but our caller
	// controls the parent — surface the error clearly if missing.
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if _, err := os.Stat(dir); err != nil {
			return fmt.Errorf("backup directory unusable: %w", err)
		}
	}
	// VACUUM INTO takes a string literal; embed the path with SQLite's
	// single-quote escape (double the quote). Path is caller-controlled
	// so we don't parameterize — sqlite3 does not accept ? bindings in
	// VACUUM INTO anyway.
	escaped := strings.ReplaceAll(path, "'", "''")
	if _, err := db.Exec(fmt.Sprintf("VACUUM INTO '%s'", escaped)); err != nil {
		return fmt.Errorf("vacuum into %s: %w", path, err)
	}
	return nil
}

// limitOffsetClause renders the trailing LIMIT/OFFSET for a paginated
// query. A limit of 0 means "no limit", but SQLite only honours OFFSET
// when a LIMIT is present, so an unlimited page with an offset gets the
// `LIMIT -1` spelling for "all remaining rows". Negative inputs are
// treated as zero rather than passed through to SQL.
func limitOffsetClause(limit, offset int) string {
	if offset < 0 {
		offset = 0
	}
	switch {
	case limit > 0:
		if offset > 0 {
			return fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
		}
		return fmt.Sprintf(" LIMIT %d", limit)
	case offset > 0:
		return fmt.Sprintf(" LIMIT -1 OFFSET %d", offset)
	default:
		return ""
	}
}

// BeginTx starts a new transaction
func (db *DB) BeginTx() (*sql.Tx, error) {
	return db.Begin()
}

// WithTx executes a function within a transaction
func (db *DB) WithTx(fn func(*sql.Tx) error) error {
	return db.WithTxContext(context.Background(), fn)
}

// WithTxContext executes a function within a transaction that is bound to ctx.
// Cancelling ctx rolls the transaction back, so statements issued after the
// cancellation fail instead of continuing to write. That is what makes a long
// multi-statement write (a big session's turns) interruptible mid-flight —
// a quit flag checked between units of work cannot do that.
func (db *DB) WithTxContext(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}
