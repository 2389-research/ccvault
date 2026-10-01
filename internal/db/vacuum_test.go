// ABOUTME: Tests for storage accounting and compaction of the SQLite archive
// ABOUTME: Builds real dead pages by inserting then deleting rows in a temp dir

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bloatDB fills a fresh database with turns and then deletes most of them,
// leaving a real freelist behind. Returns the data dir; the database is
// closed on return so callers can compact it.
func bloatDB(t *testing.T, turns, keep int) string {
	t.Helper()

	dataDir := t.TempDir()
	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	// ~17 KB of content per turn. Dead pages come from bytes, not row count,
	// so fat rows buy the same freelist as thousands of thin ones without
	// the SQL statement overhead — which matters under -race, where every
	// statement runs through a pure-Go SQLite.
	content := strings.Repeat("dead page filler. ", 960)

	_, err = database.Exec(
		`INSERT INTO projects (path, display_name, first_seen_at, last_activity_at)
		 VALUES ('/tmp/bloat', 'bloat', '2026-01-01', '2026-01-02')`)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_, err = database.Exec(
		`INSERT INTO sessions (id, project_id, started_at, source_file)
		 VALUES ('s1', 1, '2026-01-01', '/tmp/bloat/s1.jsonl')`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	err = database.WithTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			`INSERT INTO turns (id, session_id, type, timestamp, content, raw_json)
			 VALUES (?, 's1', 'assistant', '2026-01-01', ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for i := 0; i < turns; i++ {
			id := fmt.Sprintf("t%06d", i)
			if _, err := stmt.Exec(id, content, `{"id":"`+id+`"}`); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	if _, err := database.Exec(`DELETE FROM turns WHERE rowid > ?`, keep); err != nil {
		t.Fatalf("delete turns: %v", err)
	}

	// Fold the WAL back into the main file so on-disk size reflects the work.
	if _, err := database.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	return dataDir
}

func mustStorageStats(t *testing.T, dataDir string) StorageStats {
	t.Helper()
	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()
	stats, err := database.StorageStats()
	if err != nil {
		t.Fatalf("storage stats: %v", err)
	}
	return stats
}

func TestStorageStats_FreshDatabaseHasNothingWorthReclaiming(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	stats, err := database.StorageStats()
	if err != nil {
		t.Fatalf("storage stats: %v", err)
	}

	if stats.PageSize <= 0 {
		t.Errorf("page size = %d, want > 0", stats.PageSize)
	}
	if stats.PageCount <= 0 {
		t.Errorf("page count = %d, want > 0", stats.PageCount)
	}
	// Running the migrations frees a page or two; what matters is that a
	// database nobody has deleted from holds no meaningful waste.
	if stats.ReclaimableBytes() > 64*1024 {
		t.Errorf("reclaimable = %d bytes on a fresh database, want a handful of pages at most",
			stats.ReclaimableBytes())
	}
	if stats.WorthReclaiming() {
		t.Error("fresh database reported as worth reclaiming")
	}
	if stats.LiveBytes() <= 0 {
		t.Errorf("live bytes = %d, want > 0", stats.LiveBytes())
	}
}

func TestStorageStats_ReportsDeadPagesAfterDeletes(t *testing.T) {
	dataDir := bloatDB(t, 80, 10)

	stats := mustStorageStats(t, dataDir)

	if stats.FreelistCount == 0 {
		t.Fatal("freelist count = 0 after deleting 70 of 80 turns, want > 0")
	}
	if stats.ReclaimableBytes() != stats.FreelistCount*stats.PageSize {
		t.Errorf("reclaimable = %d, want %d", stats.ReclaimableBytes(), stats.FreelistCount*stats.PageSize)
	}
	if stats.FreelistRatio() <= 0.25 {
		t.Errorf("freelist ratio = %f, want > 0.25 after deleting most rows", stats.FreelistRatio())
	}
	if stats.FileBytes <= 0 {
		t.Errorf("file bytes = %d, want > 0", stats.FileBytes)
	}
}

func TestStorageStats_RatioAndWorthReclaiming(t *testing.T) {
	const mb = 1024 * 1024
	tests := []struct {
		name      string
		stats     StorageStats
		wantRatio float64
		wantWorth bool
	}{
		{
			name:      "empty database divides by zero safely",
			stats:     StorageStats{PageSize: 4096},
			wantRatio: 0,
			wantWorth: false,
		},
		{
			name:      "mostly dead and large",
			stats:     StorageStats{PageSize: 4096, PageCount: 1197217, FreelistCount: 713257},
			wantRatio: 0.5958,
			wantWorth: true,
		},
		{
			name:      "high ratio but only kilobytes to reclaim",
			stats:     StorageStats{PageSize: 4096, PageCount: 100, FreelistCount: 90},
			wantRatio: 0.9,
			wantWorth: false,
		},
		{
			name:      "gigabytes dead but a small slice of the file",
			stats:     StorageStats{PageSize: 4096, PageCount: 1000000, FreelistCount: 50000},
			wantRatio: 0.05,
			wantWorth: false,
		},
		{
			name:      "just over both thresholds",
			stats:     StorageStats{PageSize: 4096, PageCount: 40000, FreelistCount: 10000},
			wantRatio: 0.25,
			wantWorth: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stats.FreelistRatio(); got < tt.wantRatio-0.0001 || got > tt.wantRatio+0.0001 {
				t.Errorf("FreelistRatio() = %f, want %f", got, tt.wantRatio)
			}
			if got := tt.stats.WorthReclaiming(); got != tt.wantWorth {
				t.Errorf("WorthReclaiming() = %v, want %v (reclaimable %d MB)",
					got, tt.wantWorth, tt.stats.ReclaimableBytes()/mb)
			}
		})
	}
}

func TestCompact_ReclaimsDeadPagesAndPreservesData(t *testing.T) {
	dataDir := bloatDB(t, 80, 10)
	before := mustStorageStats(t, dataDir)

	result, err := Compact(dataDir)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}

	if result.After.FreelistCount != 0 {
		t.Errorf("freelist after compaction = %d, want 0", result.After.FreelistCount)
	}
	if result.After.FileBytes >= before.FileBytes {
		t.Errorf("file did not shrink: %d -> %d bytes", before.FileBytes, result.After.FileBytes)
	}
	if result.BytesReclaimed() <= 0 {
		t.Errorf("bytes reclaimed = %d, want > 0", result.BytesReclaimed())
	}
	if result.Path != filepath.Join(dataDir, "ccvault.db") {
		t.Errorf("result path = %q, want the live database path", result.Path)
	}

	// The data has to survive, including the FTS index.
	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen after compaction: %v", err)
	}
	defer func() { _ = database.Close() }()

	var turnCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 10 {
		t.Errorf("turns after compaction = %d, want 10", turnCount)
	}

	var ftsHits int
	err = database.QueryRow(`SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH 'filler'`).Scan(&ftsHits)
	if err != nil {
		t.Fatalf("fts search after compaction: %v", err)
	}
	if ftsHits != 10 {
		t.Errorf("fts matches after compaction = %d, want 10", ftsHits)
	}
}

func TestCompact_LeavesNoTemporaryFilesBehind(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)

	if _, err := Compact(dataDir); err != nil {
		t.Fatalf("compact: %v", err)
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "ccvault.db":
		default:
			t.Errorf("unexpected leftover file in data dir: %s", e.Name())
		}
	}
}

func TestCompact_AlreadyCompactDatabaseSucceeds(t *testing.T) {
	dataDir := bloatDB(t, 60, 60) // nothing deleted, so near-zero waste

	result, err := Compact(dataDir)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.Before.WorthReclaiming() {
		t.Errorf("freelist before = %d of %d pages, expected nothing worth reclaiming",
			result.Before.FreelistCount, result.Before.PageCount)
	}
	if result.After.FreelistCount != 0 {
		t.Errorf("freelist after = %d, want 0", result.After.FreelistCount)
	}
	if result.After.PageCount <= 0 {
		t.Errorf("page count after = %d, want > 0", result.After.PageCount)
	}

	stats := mustStorageStats(t, dataDir)
	if stats.PageCount != result.After.PageCount {
		t.Errorf("page count on reopen = %d, want %d", stats.PageCount, result.After.PageCount)
	}
}

func TestCompact_MissingDatabaseIsRefused(t *testing.T) {
	dataDir := t.TempDir()

	_, err := Compact(dataDir)
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("compact error = %v, want ErrNoDatabase", err)
	}

	// Refusing must not leave a freshly created empty database behind.
	if _, err := os.Stat(filepath.Join(dataDir, "ccvault.db")); !os.IsNotExist(err) {
		t.Errorf("stat ccvault.db = %v, want not-exist", err)
	}
}

func TestCompact_InsufficientFreeSpaceIsRefused(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	dbPath := filepath.Join(dataDir, "ccvault.db")

	beforeInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}

	// Pretend the filesystem is nearly full. A disk-space probe is the one
	// thing a temp dir cannot produce on demand.
	restore := availableBytes
	availableBytes = func(string) (int64, error) { return 1024, nil }
	t.Cleanup(func() { availableBytes = restore })

	_, err = Compact(dataDir)
	if !errors.Is(err, ErrInsufficientDiskSpace) {
		t.Fatalf("compact error = %v, want ErrInsufficientDiskSpace", err)
	}
	if !strings.Contains(err.Error(), "free") {
		t.Errorf("error %q does not say how much space is free", err)
	}

	afterInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat database after refusal: %v", err)
	}
	if afterInfo.Size() != beforeInfo.Size() {
		t.Errorf("database size changed on refusal: %d -> %d", beforeInfo.Size(), afterInfo.Size())
	}
}

func TestCompact_CorruptedCopyIsNotSwappedIn(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	dbPath := filepath.Join(dataDir, "ccvault.db")

	beforeInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}

	// Truncate the compacted copy before it can be verified: a half-written
	// copy must never replace a good database.
	_, err = Compact(dataDir, withAfterCopy(func(tmpPath string) error {
		return os.Truncate(tmpPath, 8192)
	}))
	if err == nil {
		t.Fatal("compact succeeded with a truncated copy, want an error")
	}

	afterInfo, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat database after failure: %v", err)
	}
	if afterInfo.Size() != beforeInfo.Size() {
		t.Errorf("live database was modified: %d -> %d bytes", beforeInfo.Size(), afterInfo.Size())
	}

	// The original must still be a working database with its rows intact.
	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen original: %v", err)
	}
	defer func() { _ = database.Close() }()
	var turnCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 5 {
		t.Errorf("turns = %d, want 5", turnCount)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "ccvault.db.compacting")); !os.IsNotExist(err) {
		t.Errorf("temporary copy left behind: %v", err)
	}
}

func TestCompact_RowCountMismatchIsNotSwappedIn(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)

	// A structurally valid copy that is missing rows is the nastier failure:
	// integrity_check passes, so only the row-count comparison catches it.
	_, err := Compact(dataDir, withAfterCopy(func(tmpPath string) error {
		conn, err := sql.Open("sqlite", "file:"+tmpPath)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		_, err = conn.Exec(`DELETE FROM turns WHERE rowid <= 2`)
		return err
	}))
	if err == nil {
		t.Fatal("compact succeeded with a copy missing rows, want an error")
	}
	if !strings.Contains(err.Error(), "turns") {
		t.Errorf("error %q does not name the table that disagreed", err)
	}

	stats := mustStorageStats(t, dataDir)
	if stats.FreelistCount == 0 {
		t.Error("live database looks compacted; the bad copy appears to have been swapped in")
	}
}

func TestCompact_HoldsExclusiveLockWhileCopying(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	dbPath := filepath.Join(dataDir, "ccvault.db")

	var writeErr error
	attempted := false
	result, err := Compact(dataDir, withAfterCopy(func(string) error {
		attempted = true
		other, err := sql.Open("sqlite", "file:"+dbPath+"?_busy_timeout=200")
		if err != nil {
			return err
		}
		defer func() { _ = other.Close() }()
		_, writeErr = other.Exec(`INSERT INTO source_files (path, mtime, synced_at)
			VALUES ('/tmp/sneaky.jsonl', '2026-01-01', '2026-01-01')`)
		return nil
	}))
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !attempted {
		t.Fatal("hook never ran")
	}
	if writeErr == nil {
		t.Error("a concurrent write succeeded during compaction; the original was not locked")
	}
	if result.After.FreelistCount != 0 {
		t.Errorf("freelist after = %d, want 0", result.After.FreelistCount)
	}
}

func TestCompact_RefusesWhileAnotherReaderHoldsTheDatabase(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	dbPath := filepath.Join(dataDir, "ccvault.db")

	reader, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	ctx := context.Background()
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&n); err != nil {
		t.Fatalf("read in tx: %v", err)
	}

	_, err = Compact(dataDir)
	if err == nil {
		_ = tx.Rollback()
		t.Fatal("compact succeeded while another connection held a read transaction")
	}
	if !errors.Is(err, ErrDatabaseBusy) {
		_ = tx.Rollback()
		t.Fatalf("compact error = %v, want ErrDatabaseBusy", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback reader: %v", err)
	}

	// The refusal must leave a working database, and compaction must work
	// once the reader has let go.
	if _, err := Compact(dataDir); err != nil {
		t.Fatalf("compact after reader released: %v", err)
	}
}

func TestCompact_ClearsLeftoverCopyFromACrashedRun(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	leftover := filepath.Join(dataDir, "ccvault.db.compacting")

	// A run killed between the copy and the swap leaves this behind. It is
	// scratch — the live database was never touched — so the next run must
	// clear it instead of refusing forever.
	if err := os.WriteFile(leftover, []byte("half-written garbage"), 0o600); err != nil {
		t.Fatalf("write leftover: %v", err)
	}

	result, err := Compact(dataDir)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.After.FreelistCount != 0 {
		t.Errorf("freelist after = %d, want 0", result.After.FreelistCount)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("stat leftover = %v, want not-exist", err)
	}
}

func TestCompact_PreservesFilePermissions(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	dbPath := filepath.Join(dataDir, "ccvault.db")

	// The archive holds the user's conversations; a compaction that widens
	// the file mode would quietly expose them.
	if err := os.Chmod(dbPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := Compact(dataDir); err != nil {
		t.Fatalf("compact: %v", err)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode after compaction = %o, want 600", got)
	}
}

func TestCompact_ReportsProgressAndDuration(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)

	var messages []string
	result, err := Compact(dataDir, WithCompactProgress(func(msg string) {
		messages = append(messages, msg)
	}))
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(messages) == 0 {
		t.Error("no progress messages reported")
	}
	if result.Duration <= 0 {
		t.Errorf("duration = %s, want > 0", result.Duration)
	}
	if result.Duration > time.Minute {
		t.Errorf("duration = %s, implausible for a test database", result.Duration)
	}
}

func TestAvailableBytes_ReportsPositiveFreeSpace(t *testing.T) {
	free, err := availableBytes(t.TempDir())
	if err != nil {
		t.Fatalf("availableBytes: %v", err)
	}
	if free <= 0 {
		t.Errorf("free bytes = %d, want > 0 on a working filesystem", free)
	}
}
