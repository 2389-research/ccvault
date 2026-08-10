// ABOUTME: Database connection management and initialization
// ABOUTME: Provides SQLite connection with FTS5 support for ccvault

package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

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

// ResetAll deletes all data from all tables for a clean full resync.
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
	// Delete data in child-to-parent order so foreign-key-like invariants hold
	// (turns before sessions, sessions before projects, etc.)
	tables := []string{"tool_uses", "turns", "sessions", "projects", "source_files"}
	return db.WithTx(func(tx *sql.Tx) error {
		for _, table := range tables {
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return fmt.Errorf("delete from %s: %w", table, err)
			}
		}
		return nil
	})
}

// BeginTx starts a new transaction
func (db *DB) BeginTx() (*sql.Tx, error) {
	return db.Begin()
}

// WithTx executes a function within a transaction
func (db *DB) WithTx(fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx()
	if err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}
