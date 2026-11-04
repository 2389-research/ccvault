// ABOUTME: Tests that the WAL files survive Open/Close so read-only openers work
// ABOUTME: Asserts the sidecars exist before any connection could recreate them

package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestCloseKeepsWALFilesForReadOnlyOpeners(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Grow the WAL past a few pages so the size check below means something.
	if _, err := db.Exec("CREATE TABLE walgrowth(b BLOB); INSERT INTO walgrowth VALUES (zeroblob(1 << 20))"); err != nil {
		t.Fatalf("grow wal: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	path := filepath.Join(dir, dbFileName)
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("%s after close: %v", suffix, err)
		}
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("-wal is %d bytes after close, want 0 (journal_size_limit truncates it)", fi.Size())
	}

	// A smoke test, not the regression. modernc's bundled SQLite creates the
	// sidecars itself on any open, read-only ones included, so this query
	// succeeds with or without the fix — which is why the stat calls above
	// come first and are what actually pins the behaviour. The failure this
	// guards against is only observable from an external SQLite build
	// (macOS system 3.51.0, Python's sqlite3), which no Go test can reach.
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = ro.Close() }()
	var n int
	if err := ro.QueryRow("SELECT count(*) FROM sessions").Scan(&n); err != nil {
		t.Fatalf("read-only query after close: %v", err)
	}
}
