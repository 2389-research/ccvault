// ABOUTME: Tests LastSyncedAt, the archive's own answer to "when did a sync last write anything"
// ABOUTME: Fact 4 of `ccvault schedule status` reads this, so a wrong answer here is a staleness warning that lies

package db

import (
	"testing"
	"time"
)

// An archive nobody has synced must say so, not report an age measured from
// the year 1. Assemble turns the zero time into "never synced"; it can only
// do that if this returns a zero time rather than guessing.
func TestLastSyncedAt_EmptyArchiveReturnsZero(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	got, err := database.LastSyncedAt()
	if err != nil {
		t.Fatalf("LastSyncedAt() error = %v", err)
	}
	if !got.IsZero() {
		t.Errorf("LastSyncedAt() = %s, want the zero time for an archive with no source_files rows", got)
	}
}

func TestLastSyncedAt_ReturnsTheNewestRow(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	oldest := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 10, 29, 11, 30, 0, 0, time.UTC)
	middle := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

	// synced_at is written as "now" by UpsertSourceFileMtime, so set it
	// directly: the point of the test is which of several rows wins.
	for path, syncedAt := range map[string]time.Time{
		"/a.jsonl": oldest,
		"/b.jsonl": newest,
		"/c.jsonl": middle,
	} {
		if _, err := database.Exec(
			"INSERT INTO source_files (path, source, mtime, synced_at) VALUES (?, 'claude-code', ?, ?)",
			path, FormatTimestamp(syncedAt), FormatTimestamp(syncedAt),
		); err != nil {
			t.Fatalf("insert %s: %v", path, err)
		}
	}

	got, err := database.LastSyncedAt()
	if err != nil {
		t.Fatalf("LastSyncedAt() error = %v", err)
	}
	if !got.Equal(newest) {
		t.Errorf("LastSyncedAt() = %s, want %s — MAX(synced_at), not the first or last row inserted", got, newest)
	}
}

// The canonical layout sorts as text, which is what makes MAX() correct
// here. A row written by an older ccvault does not, so the parse has to cope
// and the comparison must still land on the real newest instant.
func TestLastSyncedAt_AdvancesWhenASyncWrites(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	before := time.Now().UTC()
	if err := database.UpsertSourceFileMtime("/x.jsonl", before); err != nil {
		t.Fatalf("UpsertSourceFileMtime: %v", err)
	}

	first, err := database.LastSyncedAt()
	if err != nil {
		t.Fatalf("LastSyncedAt() error = %v", err)
	}
	if first.Before(before.Add(-time.Minute)) || first.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("LastSyncedAt() = %s, want an instant around now (%s)", first, before)
	}

	// Re-syncing the same file has to move synced_at forward, or a schedule
	// re-reading an unchanged tree would look permanently stale.
	time.Sleep(10 * time.Millisecond)
	if err := database.UpsertSourceFileMtime("/x.jsonl", before); err != nil {
		t.Fatalf("second UpsertSourceFileMtime: %v", err)
	}
	second, err := database.LastSyncedAt()
	if err != nil {
		t.Fatalf("LastSyncedAt() error = %v", err)
	}
	if !second.After(first) {
		t.Errorf("LastSyncedAt() = %s after a re-sync, want it later than %s", second, first)
	}
}

// Deliberately not an error: the rest of the archive is still readable, and a
// staleness report is not worth failing a command over. It reads as "never
// synced", which is the conservative answer.
func TestLastSyncedAt_UnparseableValueReadsAsNeverSynced(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	if _, err := database.Exec(
		"INSERT INTO source_files (path, source, mtime, synced_at) VALUES ('/a.jsonl', 'claude-code', ?, 'not a timestamp')",
		FormatTimestamp(time.Now()),
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := database.LastSyncedAt()
	if err != nil {
		t.Fatalf("LastSyncedAt() error = %v, want it to tolerate a value it cannot parse", err)
	}
	if !got.IsZero() {
		t.Errorf("LastSyncedAt() = %s, want the zero time", got)
	}
}
