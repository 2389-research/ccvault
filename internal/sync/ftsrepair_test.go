// ABOUTME: Tests that a full re-parse repairs a turns_fts index left damaged by an earlier writer
// ABOUTME: Covers both the repair and the deliberate silence of an incremental sync

package sync

import (
	"context"
	"os"
	"testing"

	"github.com/2389-research/ccvault/internal/config"
	"github.com/2389-research/ccvault/internal/db"
)

// injectFTSOrphan writes a document into turns_fts at a rowid no turns row
// owns, which is the state a writer without recursive_triggers leaves behind
// every time it replaces a turn.
func injectFTSOrphan(t *testing.T, database *db.DB) {
	t.Helper()

	var freeRowid int64
	if err := database.QueryRow("SELECT COALESCE(MAX(rowid), 0) + 1000 FROM turns").Scan(&freeRowid); err != nil {
		t.Fatalf("find a free rowid: %v", err)
	}
	if _, err := database.Exec(
		"INSERT INTO turns_fts(rowid, content) VALUES (?, 'strandedcontent')", freeRowid); err != nil {
		t.Fatalf("inject orphan: %v", err)
	}
}

func claudeHomeWithOneSession(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "claude-home-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	writeTestSession(t, home, "a1b2c3d4-e5f6-7890-abcd-ef1234567890", "-Users-test-myproject")
	return home
}

// A full re-parse has to repair an index that already describes turns the
// archive no longer holds. Nothing else can: the triggers only fire on changes
// to turns, and an orphaned document has no turns row left to change.
func TestFullSyncRebuildsDamagedSearchIndex(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	home := claudeHomeWithOneSession(t)
	sources := []config.SourceConfig{{Name: "claude-code", Type: "claude-code", Path: home}}

	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	injectFTSOrphan(t, database)

	damaged, err := database.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if damaged.Orphaned != 1 {
		t.Fatalf("Orphaned = %d before the repair, want 1", damaged.Orphaned)
	}

	stats, err := New(database, sources, WithFullSync(true)).Run(context.Background())
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("full sync reported errors: %v", stats.Errors)
	}
	if !stats.SearchIndexRebuilt {
		t.Error("SearchIndexRebuilt is false after a full sync")
	}

	repaired, err := database.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity after the repair: %v", err)
	}
	if !repaired.Consistent() {
		t.Errorf("index still inconsistent after a full sync: %+v", repaired)
	}
	if _, err := database.Exec("INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1)"); err != nil {
		t.Errorf("FTS5 strict integrity-check after the repair: %v", err)
	}

	// The session's own turns must still be findable, or the repair traded one
	// defect for a worse one. "there" comes from the assistant turn's "Hi
	// there!" — the fixture's user turn parses to empty content, so searching
	// for its text would pass whether the index survived or not.
	hits, err := database.SearchTurns("there", 10)
	if err != nil {
		t.Fatalf("SearchTurns: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("the repaired index returns %d hits for content the archive holds, want 1", len(hits))
	}

	// And the orphan's content must not.
	if _, err := database.Exec("INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1)"); err != nil {
		t.Errorf("strict integrity-check after searching the repaired index: %v", err)
	}
}

// The case that made the rebuild unconditional.
//
// A re-parse gives every turn a fresh rowid from the end of the table, so the
// rowids a stranded index entry sits on get reused. Once they are, the
// %_docsize count agrees with the turns count again and CheckFTSIntegrity
// calls the archive clean — while the index still answers for the stranded
// document's text and FTS5's strict integrity-check still calls the file
// malformed. A repair gated on the cheap check would skip exactly this.
func TestFullSyncRebuildsIndexWhoseOrphanRowidsWereReused(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	home, err := os.MkdirTemp("", "claude-home-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	// Two sessions, so the first one's re-parse leaves the table's high-water
	// rowid above zero and the second one's inserts land on the stranded ids.
	writeTestSession(t, home, "11111111-1111-1111-1111-111111111111", "-Users-test-one")
	writeTestSession(t, home, "22222222-2222-2222-2222-222222222222", "-Users-test-two")

	sources := []config.SourceConfig{{Name: "claude-code", Type: "claude-code", Path: home}}
	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	var nextRowid int64
	if err := database.QueryRow("SELECT COALESCE(MAX(rowid), 0) + 1 FROM turns").Scan(&nextRowid); err != nil {
		t.Fatalf("read the next rowid: %v", err)
	}
	if _, err := database.Exec(
		"INSERT INTO turns_fts(rowid, content) VALUES (?, 'reusedghostcontent')", nextRowid); err != nil {
		t.Fatalf("inject orphan: %v", err)
	}

	stats, err := New(database, sources, WithFullSync(true)).Run(context.Background())
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if !stats.SearchIndexRebuilt {
		t.Fatal("SearchIndexRebuilt is false; the rest of this test proves nothing")
	}

	// The cheap check is expected to see nothing here, which is the point.
	integrity, err := database.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if !integrity.Consistent() {
		t.Logf("note: the cheap check did see this one (%+v); the strict assertions below are what matter", integrity)
	}

	if _, err := database.Exec("INSERT INTO turns_fts(turns_fts, rank) VALUES('integrity-check', 1)"); err != nil {
		t.Errorf("FTS5 strict integrity-check after the rebuild: %v", err)
	}
	var ghosts int
	if err := database.QueryRow(
		"SELECT COUNT(*) FROM turns_fts WHERE turns_fts MATCH '{content} : reusedghostcontent'").Scan(&ghosts); err != nil {
		t.Fatalf("probe the index for the stranded text: %v", err)
	}
	if ghosts != 0 {
		t.Errorf("the index still answers for the stranded text with %d documents", ghosts)
	}
}

// A rebuild of a healthy index has to be a no-op in effect: the turns stay
// searchable and nothing is reported as wrong.
func TestFullSyncRebuildLeavesHealthyIndexSearchable(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	home := claudeHomeWithOneSession(t)
	sources := []config.SourceConfig{{Name: "claude-code", Type: "claude-code", Path: home}}

	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	stats, err := New(database, sources, WithFullSync(true)).Run(context.Background())
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	for _, err := range stats.Errors {
		t.Errorf("full sync on a healthy archive reported: %v", err)
	}

	integrity, err := database.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if !integrity.Consistent() {
		t.Errorf("index inconsistent after rebuilding a healthy one: %+v", integrity)
	}
	hits, err := database.SearchTurns("there", 10)
	if err != nil {
		t.Fatalf("SearchTurns: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("search returns %d hits after the rebuild, want 1", len(hits))
	}
}

// An incremental sync deliberately does not rebuild. A run that touched four
// files has no business spending a pass over the whole index.
func TestIncrementalSyncDoesNotRebuildSearchIndex(t *testing.T) {
	database, cleanup := setupTestDB(t)
	defer cleanup()

	home := claudeHomeWithOneSession(t)
	sources := []config.SourceConfig{{Name: "claude-code", Type: "claude-code", Path: home}}

	if _, err := New(database, sources).Run(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	injectFTSOrphan(t, database)

	stats, err := New(database, sources).Run(context.Background())
	if err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	if stats.SearchIndexRebuilt {
		t.Error("an incremental sync rebuilt the search index")
	}

	still, err := database.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if still.Orphaned != 1 {
		t.Errorf("Orphaned = %d after an incremental sync, want the orphan left for --full to find", still.Orphaned)
	}
}
