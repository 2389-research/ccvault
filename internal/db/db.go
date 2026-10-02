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

// Connection settings every ccvault connection must end up with. They are
// named here, asserted after Open, and used by the tests, so the DSN and the
// contract can't drift apart.
const (
	// busyTimeoutMS is how long a connection waits for a lock before giving
	// up with SQLITE_BUSY. Sync holds the writer for the length of one
	// session's transaction; a TUI or MCP reader that arrives mid-write
	// should wait it out rather than error.
	busyTimeoutMS = 5000

	// recursiveTriggersOn is PRAGMA recursive_triggers=ON as the pragma
	// reports it back. Turns are written with INSERT OR REPLACE, and SQLite
	// fires a REPLACE's implicit DELETE through AFTER DELETE triggers only
	// when recursive triggers are enabled — which they are not by default. The
	// turns_ad trigger is what removes a row's entry from the turns_fts
	// external-content index, so with the default setting a replaced turn
	// indexed its new content and left the old entry behind, matching text no
	// turns row holds any more.
	//
	// The recursion it allows is one level deep and cannot loop: the three
	// triggers on turns all write to turns_fts, a virtual table, and nothing
	// writes back to turns.
	recursiveTriggersOn = 1

	// synchronousNormal is PRAGMA synchronous=NORMAL as the pragma reports
	// it back. In WAL mode NORMAL skips the per-commit fsync and lets the OS
	// flush at checkpoint instead, which matters because sync commits once
	// per session across tens of thousands of sessions. The exposure is a
	// power loss or kernel panic (not a process crash) losing recently
	// committed transactions; the archive is a derived cache of the JSONL
	// files, so a re-sync rebuilds anything lost.
	synchronousNormal = 1
)

// connectionDSN builds the DSN for the archive.
//
// The parameter names matter more than they look. modernc.org/sqlite honours
// `_pragma=`, `_txlock=` and `_time_format=` only, and silently drops every
// other query parameter. The `_journal_mode=` / `_synchronous=` /
// `_busy_timeout=` spellings belong to mattn/go-sqlite3; this DSN carried
// them for the project's whole history and set none of them, so the archive
// ran in rollback-journal mode at synchronous=FULL with no busy timeout at
// all. Nothing noticed, because an ignored option looks exactly like a
// honoured one from the calling code. verifyConnectionPragmas exists so the
// next such mistake cannot be silent.
func connectionDSN(dbPath string) string {
	return fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=synchronous(NORMAL)&_pragma=recursive_triggers(%d)",
		dbPath, busyTimeoutMS, recursiveTriggersOn)
}

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

	dbPath := filepath.Join(dataDir, dbFileName)

	sqlDB, err := sql.Open("sqlite", connectionDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// One connection. WAL would allow concurrent readers alongside the
	// writer, but this pool is shared by a single process that reads and
	// writes the same rows, and one connection makes "the writer" a single
	// identifiable handle rather than whichever pooled connection got there
	// first. Cross-process concurrency — sync writing while the TUI or MCP
	// server reads — is what WAL plus busy_timeout is actually for here, and
	// that is unaffected by this pool's size.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// Read the settings back off the connection instead of trusting the DSN.
	if err := verifyConnectionPragmas(context.Background(), sqlDB); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

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

// verifyConnectionPragmas reads back every connection setting ccvault depends
// on and refuses the connection if any of them did not take.
//
// This is not defensive padding. The bug it guards against was a DSN whose
// options the driver dropped without an error for the project's entire
// history, costing WAL mode and the busy timeout on every connection ever
// opened. A driver swap, a driver upgrade that renames a parameter, or a typo
// inside the pragma syntax all reproduce it exactly, and all of them are
// invisible unless something asks the connection what it actually did.
func verifyConnectionPragmas(ctx context.Context, q sqlRunner) error {
	var journal string
	if err := q.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("read back journal_mode: %w", err)
	}

	var synchronous int
	if err := q.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil {
		return fmt.Errorf("read back synchronous: %w", err)
	}

	var busyTimeout int
	if err := q.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return fmt.Errorf("read back busy_timeout: %w", err)
	}

	var recursiveTriggers int
	if err := q.QueryRowContext(ctx, "PRAGMA recursive_triggers").Scan(&recursiveTriggers); err != nil {
		return fmt.Errorf("read back recursive_triggers: %w", err)
	}

	var mismatches []string
	if !strings.EqualFold(journal, "wal") {
		mismatches = append(mismatches, fmt.Sprintf("journal_mode is %q, want wal", journal))
	}
	if synchronous != synchronousNormal {
		mismatches = append(mismatches, fmt.Sprintf("synchronous is %d, want %d (NORMAL)", synchronous, synchronousNormal))
	}
	if busyTimeout != busyTimeoutMS {
		mismatches = append(mismatches, fmt.Sprintf("busy_timeout is %d, want %d", busyTimeout, busyTimeoutMS))
	}
	if recursiveTriggers != recursiveTriggersOn {
		mismatches = append(mismatches, fmt.Sprintf("recursive_triggers is %d, want %d", recursiveTriggers, recursiveTriggersOn))
	}

	if len(mismatches) > 0 {
		return fmt.Errorf("the SQLite driver did not apply the connection settings ccvault requires (%s); "+
			"the DSN may use parameter names this driver ignores",
			strings.Join(mismatches, "; "))
	}
	return nil
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
// its lifecycle. Deleting turns clears their turns_fts entries via the AFTER
// DELETE trigger, but only for rows that still exist: an orphaned index entry
// has no turns row to delete and would survive the wipe and the re-sync after
// it. The explicit 'delete-all' is what makes the clean slate actually clean.
//
// The DELETEs run inside a single transaction so mid-reset failure
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
		// Empty the search index outright rather than trusting that every
		// entry had a turns row to delete it.
		if _, err := tx.Exec("INSERT INTO turns_fts(turns_fts) VALUES('delete-all')"); err != nil {
			return fmt.Errorf("clear turns_fts: %w", err)
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
