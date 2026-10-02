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
			`INSERT INTO turns (id, session_id, type, timestamp, ordinal, content, raw_json)
			 VALUES (?, 's1', 'assistant', '2026-01-01', ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for i := 0; i < turns; i++ {
			id := fmt.Sprintf("t%06d", i)
			if _, err := stmt.Exec(id, i, content, `{"id":"`+id+`"}`); err != nil {
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

// switchToWAL asserts the database is in WAL mode rather than putting it
// there: Open does that now. It stays because the tests that call it are
// specifically about WAL behaviour, and they should fail at the fixture with
// a clear message if that ever stops being true, not deep inside a
// compaction assertion. journal_mode is persisted in the file header, so it
// survives close and reopen.
func switchToWAL(t *testing.T, dataDir string) {
	t.Helper()

	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	var mode string
	if err := database.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		t.Fatalf("set wal: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q after asking for WAL", mode)
	}
}

// switchToRollbackJournal takes a fixture out of WAL. Open now puts every
// database in WAL, so a test about rollback-journal behaviour has to ask for
// that mode explicitly instead of relying on the default it used to get.
func switchToRollbackJournal(t *testing.T, dataDir string) {
	t.Helper()

	pool, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ccvault.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = pool.Close() }()

	var mode string
	if err := pool.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatalf("set rollback journal: %v", err)
	}
	if strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q after asking for DELETE", mode)
	}
}

func mustJournalMode(t *testing.T, dataDir string) string {
	t.Helper()

	pool, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ccvault.db")+"?mode=ro")
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer func() { _ = pool.Close() }()

	var mode string
	if err := pool.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	return mode
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
		// _pragma= is the only spelling modernc.org/sqlite honours; the
		// `_busy_timeout=` form this used to carry was silently dropped,
		// so the write failed instantly instead of waiting its 200ms and
		// the test passed without ever exercising the wait (#47).
		other, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(200)")
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
	// Closing, not just ending the transaction: in WAL mode an open
	// connection keeps the -shm index mapped, which is enough on its own to
	// deny the exclusive lock. See
	// TestCompact_RefusesWhileAnIdleConnectionHoldsTheWALIndex.
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}

	// The refusal must leave a working database, and compaction must work
	// once the reader has let go.
	if _, err := Compact(dataDir); err != nil {
		t.Fatalf("compact after reader released: %v", err)
	}
}

// TestCompact_RefusesWhileAnIdleConnectionHoldsTheWALIndex pins how much
// stricter compaction is in WAL mode than in rollback-journal mode.
//
// In rollback-journal mode an idle connection with no open transaction holds
// no lock, so compaction proceeded. In WAL mode the first query maps the -shm
// WAL index, and locking_mode=EXCLUSIVE needs that index to itself — so a TUI
// or MCP server sitting idle with the database open is enough to refuse the
// run. That is the safe answer rather than a regression (compaction deletes
// the -wal, so it must not share it), but it is a real change in what the
// user has to do, and the refusal has to stay a clean ErrDatabaseBusy with
// the "close other ccvault processes" guidance rather than a raw SQLite
// string.
func TestCompact_RefusesWhileAnIdleConnectionHoldsTheWALIndex(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	if mode := mustJournalMode(t, dataDir); !strings.EqualFold(mode, "wal") {
		t.Fatalf("fixture journal_mode = %q, want wal; this test is about WAL semantics", mode)
	}
	dbPath := filepath.Join(dataDir, "ccvault.db")

	idle, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open idle connection: %v", err)
	}
	defer func() { _ = idle.Close() }()

	// sql.Open is lazy, so a query is what actually establishes the
	// connection and maps the WAL index. No transaction is left open.
	var n int
	if err := idle.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&n); err != nil {
		t.Fatalf("query on idle connection: %v", err)
	}

	_, err = Compact(dataDir)
	if err == nil {
		t.Fatal("compact succeeded while another connection held the WAL index")
	}
	if !errors.Is(err, ErrDatabaseBusy) {
		t.Fatalf("compact error = %v, want ErrDatabaseBusy", err)
	}

	if err := idle.Close(); err != nil {
		t.Fatalf("close idle connection: %v", err)
	}
	if _, err := Compact(dataDir); err != nil {
		t.Fatalf("compact after the idle connection closed: %v", err)
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

// TestCheckpointIfWAL_RefusesWhenFramesStayInTheWAL is the guard on the one
// step of compaction that can destroy data. Compact deletes the -wal right
// after checkpointing, so a checkpoint that only partly folded the WAL into
// the main file would take committed transactions with it. A reader holding
// an older snapshot produces exactly that: SQLite reports busy with fewer
// frames checkpointed than the WAL holds.
func TestCheckpointIfWAL_RefusesWhenFramesStayInTheWAL(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	switchToWAL(t, dataDir)
	dbPath := filepath.Join(dataDir, "ccvault.db")
	ctx := context.Background()

	writer, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(200)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = writer.Close() }()
	writer.SetMaxOpenConns(1)

	addFrames := func(from, to int) {
		t.Helper()
		for i := from; i < to; i++ {
			_, err := writer.ExecContext(ctx,
				`INSERT INTO sync_state (key, value) VALUES (?, 'frame')`, fmt.Sprint(i))
			if err != nil {
				t.Fatalf("write frame %d: %v", i, err)
			}
		}
	}
	addFrames(0, 50)

	// A reader pinned to this snapshot — what an open TUI or MCP server is.
	reader, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(200)")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	rtx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	var seen int
	if err := rtx.QueryRow(`SELECT COUNT(*) FROM sync_state`).Scan(&seen); err != nil {
		t.Fatalf("read in tx: %v", err)
	}

	// Frames committed after the reader's snapshot cannot be backfilled
	// while it holds on.
	addFrames(100, 150)

	err = checkpointIfWAL(ctx, writer)
	if err == nil {
		t.Fatal("checkpoint reported success while frames were still in the WAL")
	}
	if !errors.Is(err, ErrDatabaseBusy) {
		t.Fatalf("checkpoint error = %v, want ErrDatabaseBusy", err)
	}
	// The numbers are the evidence; a bare "busy" would not show that frames
	// were left behind.
	for _, want := range []string{"log=", "checkpointed="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not report %s", err, want)
		}
	}

	if err := rtx.Rollback(); err != nil {
		t.Fatalf("rollback reader: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}

	// With the reader gone the WAL folds completely and the guard passes.
	if err := checkpointIfWAL(ctx, writer); err != nil {
		t.Errorf("checkpoint after reader released: %v", err)
	}
}

// TestCheckpointIfWAL_NoOpOutsideWAL covers why the mode check has to come
// first: outside WAL, SQLite answers the checkpoint pragma with (0, -1, -1),
// which is not a real result to validate.
func TestCheckpointIfWAL_NoOpOutsideWAL(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	dbPath := filepath.Join(dataDir, "ccvault.db")

	switchToRollbackJournal(t, dataDir)
	if mode := mustJournalMode(t, dataDir); strings.EqualFold(mode, "wal") {
		t.Fatalf("fixture is in WAL mode (%q); this test needs a rollback-journal database", mode)
	}

	pool, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = pool.Close() }()

	if err := checkpointIfWAL(context.Background(), pool); err != nil {
		t.Errorf("checkpoint on a rollback-journal database: %v", err)
	}
}

// TestCompact_PreservesWALJournalMode: VACUUM INTO always writes a
// rollback-journal database, so compacting a WAL archive would quietly
// downgrade it — undoing the WAL fix every time someone reclaimed space.
func TestCompact_PreservesWALJournalMode(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	switchToWAL(t, dataDir)

	result, err := Compact(dataDir)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result.After.FreelistCount != 0 {
		t.Errorf("freelist after = %d, want 0", result.After.FreelistCount)
	}

	if mode := mustJournalMode(t, dataDir); !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode after compaction = %q, want wal", mode)
	}

	// Data intact, and no sidecar left pointing at the file we replaced.
	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("reopen after compaction: %v", err)
	}
	defer func() { _ = database.Close() }()
	var turnCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turnCount != 5 {
		t.Errorf("turns after compaction = %d, want 5", turnCount)
	}
}

// TestCompact_RefusesWhileAnotherReaderHoldsAWALDatabase is the WAL twin of
// the rollback-journal refusal. With frames already in the WAL, the lock
// attempt fails on its first read rather than on its write — and that has to
// reach the user as "something else has the database", not as a raw SQLite
// string from whichever statement happened to trip first.
func TestCompact_RefusesWhileAnotherReaderHoldsAWALDatabase(t *testing.T) {
	dataDir := bloatDB(t, 60, 5)
	switchToWAL(t, dataDir)
	dbPath := filepath.Join(dataDir, "ccvault.db")
	ctx := context.Background()

	// Put frames in the WAL, so the reader's snapshot depends on it.
	writer, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(200)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	for i := 0; i < 50; i++ {
		_, err := writer.ExecContext(ctx,
			`INSERT INTO sync_state (key, value) VALUES (?, 'frame')`, fmt.Sprint(i))
		if err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	reader, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(200)")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM turns`).Scan(&n); err != nil {
		t.Fatalf("read in tx: %v", err)
	}

	_, err = Compact(dataDir)
	if !errors.Is(err, ErrDatabaseBusy) {
		_ = tx.Rollback()
		t.Fatalf("compact error = %v, want ErrDatabaseBusy", err)
	}
	if !strings.Contains(err.Error(), "ccvault") {
		t.Errorf("error %q does not tell the user which processes to close", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback reader: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	if _, err := Compact(dataDir); err != nil {
		t.Fatalf("compact after reader released: %v", err)
	}
}

// TestLockExclusive_ReportsBusyWhateverStatementTripsFirst pins down the
// other half of the lock refusal. Taking the exclusive lock in WAL mode
// needs exclusive access to the WAL index, so depending on what is in the
// WAL the refusal can surface on the pragma read rather than on the write.
// Both have to come back as ErrDatabaseBusy.
func TestLockExclusive_ReportsBusyWhateverStatementTripsFirst(t *testing.T) {
	dataDir := t.TempDir()
	database, err := Open(dataDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	var mode string
	if err := database.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		t.Fatalf("set wal: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		_, err := database.ExecContext(ctx,
			`INSERT INTO sync_state (key, value) VALUES (?, 'frame')`, fmt.Sprint(i))
		if err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	dbPath := filepath.Join(dataDir, "ccvault.db")
	reader, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(200)")
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	rtx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read tx: %v", err)
	}
	defer func() { _ = rtx.Rollback() }()
	var seen int
	if err := rtx.QueryRow(`SELECT COUNT(*) FROM sync_state`).Scan(&seen); err != nil {
		t.Fatalf("read in tx: %v", err)
	}

	pool, err := sql.Open("sqlite",
		fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)", dbPath, compactBusyTimeoutMS))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer func() { _ = pool.Close() }()
	pool.SetMaxOpenConns(1)
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	err = lockExclusive(ctx, conn)
	if err == nil {
		t.Fatal("took the exclusive lock while a reader held a WAL snapshot")
	}
	if !errors.Is(err, ErrDatabaseBusy) {
		t.Fatalf("lockExclusive error = %v, want ErrDatabaseBusy", err)
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
