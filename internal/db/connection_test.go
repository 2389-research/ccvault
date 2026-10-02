// ABOUTME: Tests that the connection pragmas Open asks for are actually in effect
// ABOUTME: Pins journal_mode, synchronous and busy_timeout read back from a real Open

package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenAppliesConnectionPragmas is the regression test for a DSN whose
// options were silently dropped by the driver for the lifetime of the
// project. Every value here is read back from the connection Open returns,
// never inferred from the DSN string.
func TestOpenAppliesConnectionPragmas(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var synchronous int
	if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatalf("pragma synchronous: %v", err)
	}
	if synchronous != synchronousNormal {
		t.Errorf("synchronous = %d, want %d (NORMAL)", synchronous, synchronousNormal)
	}

	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("pragma busy_timeout: %v", err)
	}
	if busyTimeout != busyTimeoutMS {
		t.Errorf("busy_timeout = %d, want %d", busyTimeout, busyTimeoutMS)
	}
}

// TestOpenCreatesWALSidecars proves WAL mode reached the file and not just
// the connection: a rollback-journal database has no -wal sidecar, so its
// presence is independent evidence that the pragma took.
func TestOpenCreatesWALSidecars(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// A write is what forces frames into the log.
	if _, err := db.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatalf("dirty the header: %v", err)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, dbFileName+suffix)); err != nil {
			t.Errorf("expected %s sidecar after a write in WAL mode: %v", suffix, err)
		}
	}
}

// TestOpenReopensExistingDatabaseInWAL covers the upgrade path: journal_mode
// lives in the file header, so an archive created before this change opens as
// a rollback-journal file and has to be converted on open, not just asked
// nicely via the DSN.
func TestOpenReopensExistingDatabaseInWAL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, dbFileName)

	// Create the file the way the old DSN did: default journal mode, which
	// the driver leaves as delete.
	seed, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if _, err := seed.Exec("CREATE TABLE seeded (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	var seededMode string
	if err := seed.QueryRow("PRAGMA journal_mode").Scan(&seededMode); err != nil {
		t.Fatalf("seed journal_mode: %v", err)
	}
	if strings.EqualFold(seededMode, "wal") {
		t.Fatalf("seed database was already WAL (%q); this test can't prove the conversion", seededMode)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("journal_mode = %q after reopening a rollback-journal archive, want wal", journal)
	}
}

// TestVerifyConnectionPragmasRejectsIgnoredOptions is the guard that makes the
// rest of this file self-enforcing. It feeds verifyConnectionPragmas the
// exact DSN spelling that was broken — mattn/go-sqlite3's parameter names,
// which modernc.org/sqlite drops without a word — and requires a loud
// failure naming the pragma that did not take.
func TestVerifyConnectionPragmasRejectsIgnoredOptions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), dbFileName)
	ignoredDSN := "file:" + dbPath + "?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"

	pool, err := sql.Open("sqlite", ignoredDSN)
	if err != nil {
		t.Fatalf("open with ignored options: %v", err)
	}
	defer func() { _ = pool.Close() }()

	err = verifyConnectionPragmas(context.Background(), pool)
	if err == nil {
		t.Fatal("verifyConnectionPragmas accepted a connection where every option was ignored")
	}
	for _, want := range []string{"journal_mode", "busy_timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// TestVerifyConnectionPragmasAcceptsCorrectDSN pins the positive case against
// the same helper, so a future DSN edit that still satisfies the contract
// passes and one that doesn't cannot slip through unverified.
func TestVerifyConnectionPragmasAcceptsCorrectDSN(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), dbFileName)

	pool, err := sql.Open("sqlite", connectionDSN(dbPath))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = pool.Close() }()
	pool.SetMaxOpenConns(1)

	if err := verifyConnectionPragmas(context.Background(), pool); err != nil {
		t.Fatalf("verifyConnectionPragmas rejected the production DSN: %v", err)
	}
}
