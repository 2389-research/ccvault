// ABOUTME: Tests that the turns_fts external-content index stays in step with turns
// ABOUTME: Covers REPLACE-induced ghost entries, the integrity report, and ResetAll

package db

import (
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// ftsMatchCount probes the FTS index directly rather than through SearchTurns.
// SearchTurns joins turns_fts back to turns on rowid, which silently drops a
// ghost entry — the join finds no turns row for it — so a search through the
// public API cannot see the defect this file is about.
func ftsMatchCount(t *testing.T, db *DB, match string) int {
	t.Helper()

	var n int
	if err := db.QueryRow("SELECT count(*) FROM turns_fts WHERE turns_fts MATCH ?", match).Scan(&n); err != nil {
		t.Fatalf("probe turns_fts for %q: %v", match, err)
	}
	return n
}

func seedTurnSession(t *testing.T, db *DB, sessionID string) {
	t.Helper()

	p := &models.Project{Path: "/proj/fts", DisplayName: "fts"}
	if err := db.UpsertProject(p); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	s := &models.Session{
		ID:         sessionID,
		ProjectID:  p.ID,
		StartedAt:  time.Now().Add(-time.Hour),
		EndedAt:    time.Now(),
		SourceFile: "/proj/fts/" + sessionID + ".jsonl",
		Source:     "claude-code",
	}
	if err := db.UpsertSession(s); err != nil {
		t.Fatalf("upsert session %s: %v", sessionID, err)
	}
}

func TestFTSIntegrity_MissingAndConsistent(t *testing.T) {
	tests := []struct {
		name           string
		integrity      FTSIntegrity
		wantMissing    int64
		wantConsistent bool
	}{
		{
			name:           "index and turns agree",
			integrity:      FTSIntegrity{Turns: 10, Indexed: 10, Orphaned: 0},
			wantMissing:    0,
			wantConsistent: true,
		},
		{
			name:           "ghosts inflate the index",
			integrity:      FTSIntegrity{Turns: 10, Indexed: 12, Orphaned: 2},
			wantMissing:    0,
			wantConsistent: false,
		},
		{
			name:           "turns the index never got",
			integrity:      FTSIntegrity{Turns: 10, Indexed: 8, Orphaned: 0},
			wantMissing:    2,
			wantConsistent: false,
		},
		{
			name: "the counts match by coincidence while both faults are present",
			// Three ghosts and three unindexed turns: Indexed == Turns, so a
			// check that only compared the two totals would call this clean.
			integrity:      FTSIntegrity{Turns: 10, Indexed: 10, Orphaned: 3},
			wantMissing:    3,
			wantConsistent: false,
		},
		{
			name:           "empty archive",
			integrity:      FTSIntegrity{},
			wantMissing:    0,
			wantConsistent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.integrity.Missing(); got != tt.wantMissing {
				t.Errorf("Missing() = %d, want %d", got, tt.wantMissing)
			}
			if got := tt.integrity.Consistent(); got != tt.wantConsistent {
				t.Errorf("Consistent() = %v, want %v", got, tt.wantConsistent)
			}
		})
	}
}

// Turns are written with INSERT OR REPLACE. SQLite fires a REPLACE's implicit
// DELETE through the AFTER DELETE trigger only when recursive_triggers is on,
// and it is off by default — so a colliding turn id used to index the new
// content while leaving the old row's FTS entry behind, pointing at content
// that no longer exists.
//
// A collision needs two sources to mint the same turn id, which real Claude
// Code data does not do. It is reachable all the same: PR #36 found the test
// fixture writeTestSession had been giving every session the identical turn
// uuids, so one session's insert stole another's turns.
func TestInsertTurns_CollidingTurnIDLeavesNoFTSGhost(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	seedTurnSession(t, db, "session-first")
	seedTurnSession(t, db, "session-second")

	now := time.Now()
	if err := db.InsertTurns([]models.Turn{{
		ID:        "turn-collide",
		SessionID: "session-first",
		Type:      "user",
		Timestamp: now,
		Content:   "zymurgyghost is the word only the replaced row ever held",
	}}); err != nil {
		t.Fatalf("insert original turn: %v", err)
	}

	// Sanity: the word is in the index before the replace, so a later count of
	// zero means the entry was removed rather than never written.
	if got := ftsMatchCount(t, db, "zymurgyghost"); got != 1 {
		t.Fatalf("index hits for the original content = %d, want 1 — the fixture is not exercising the index", got)
	}

	// The collision: same turn id, different session, different content.
	if err := db.InsertTurns([]models.Turn{{
		ID:        "turn-collide",
		SessionID: "session-second",
		Type:      "user",
		Timestamp: now.Add(time.Minute),
		Content:   "sarsaparilla is the word the surviving row holds",
	}}); err != nil {
		t.Fatalf("insert colliding turn: %v", err)
	}

	if got := ftsMatchCount(t, db, "zymurgyghost"); got != 0 {
		t.Errorf("index still holds %d entries for the replaced row's content; it is a ghost pointing at content that no longer exists", got)
	}

	integrity, err := db.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if !integrity.Consistent() {
		t.Errorf("index inconsistent after a replace: %+v", integrity)
	}

	// The replacement must still be findable through the public API, so the
	// fix cannot be "stop indexing on replace".
	hits, err := db.SearchTurns("sarsaparilla", 10)
	if err != nil {
		t.Fatalf("SearchTurns: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("search for the surviving content returned %d hits, want 1", len(hits))
	}
	if hits[0].SessionID != "session-second" {
		t.Errorf("surviving turn belongs to %q, want session-second", hits[0].SessionID)
	}
}

// CheckFTSIntegrity has to be able to fail, or reporting it in `ccvault stats`
// says nothing. A ghost entry is written straight into the index here, which
// is exactly the state a REPLACE used to leave behind.
func TestCheckFTSIntegrity_ReportsOrphanedIndexEntries(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	seedTurnSession(t, db, "session-integrity")
	if err := db.InsertTurns([]models.Turn{{
		ID:        "turn-live",
		SessionID: "session-integrity",
		Type:      "user",
		Timestamp: time.Now(),
		Content:   "a turn that really exists",
	}}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	clean, err := db.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity: %v", err)
	}
	if !clean.Consistent() {
		t.Fatalf("a freshly written index reports inconsistent: %+v", clean)
	}
	if clean.Turns != 1 || clean.Indexed != 1 {
		t.Errorf("counts = %+v, want 1 turn and 1 indexed document", clean)
	}

	// A document at a rowid no turns row owns: a ghost.
	var freeRowid int64
	if err := db.QueryRow("SELECT COALESCE(MAX(rowid), 0) + 1000 FROM turns").Scan(&freeRowid); err != nil {
		t.Fatalf("find a free rowid: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO turns_fts(rowid, content) VALUES (?, 'phantom content with no turn row')",
		freeRowid); err != nil {
		t.Fatalf("inject ghost entry: %v", err)
	}

	drifted, err := db.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity after injecting a ghost: %v", err)
	}
	if drifted.Orphaned != 1 {
		t.Errorf("Orphaned = %d, want 1", drifted.Orphaned)
	}
	if drifted.Consistent() {
		t.Error("Consistent() is true with a ghost entry in the index")
	}
}

// ResetAll's contract is that `sync --rebuild` starts from a clean slate. It
// clears turns with a DELETE, which fires the AFTER DELETE trigger for every
// live row — but a ghost entry has no turns row to delete, so it used to
// survive the wipe and the re-sync that followed.
func TestResetAll_ClearsOrphanedFTSEntries(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	seedTurnSession(t, db, "session-reset")
	if err := db.InsertTurns([]models.Turn{{
		ID:        "turn-reset",
		SessionID: "session-reset",
		Type:      "user",
		Timestamp: time.Now(),
		Content:   "live content",
	}}); err != nil {
		t.Fatalf("insert turn: %v", err)
	}

	var freeRowid int64
	if err := db.QueryRow("SELECT COALESCE(MAX(rowid), 0) + 1000 FROM turns").Scan(&freeRowid); err != nil {
		t.Fatalf("find a free rowid: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO turns_fts(rowid, content) VALUES (?, 'leftoverphantom')",
		freeRowid); err != nil {
		t.Fatalf("inject ghost entry: %v", err)
	}
	if got := ftsMatchCount(t, db, "leftoverphantom"); got != 1 {
		t.Fatalf("ghost not in the index before the reset (%d hits); nothing to clear", got)
	}

	if err := db.ResetAll(); err != nil {
		t.Fatalf("ResetAll: %v", err)
	}

	if got := ftsMatchCount(t, db, "leftoverphantom"); got != 0 {
		t.Errorf("ghost survived ResetAll (%d hits)", got)
	}
	after, err := db.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("CheckFTSIntegrity after reset: %v", err)
	}
	if after.Indexed != 0 || after.Turns != 0 {
		t.Errorf("after ResetAll: %+v, want an empty index and no turns", after)
	}

	// The index has to still work after being emptied, or a rebuild would
	// produce an archive nothing can search.
	seedTurnSession(t, db, "session-after-reset")
	if err := db.InsertTurns([]models.Turn{{
		ID:        "turn-after-reset",
		SessionID: "session-after-reset",
		Type:      "user",
		Timestamp: time.Now(),
		Content:   "reindexedafterwards",
	}}); err != nil {
		t.Fatalf("insert turn after reset: %v", err)
	}
	hits, err := db.SearchTurns("reindexedafterwards", 10)
	if err != nil {
		t.Fatalf("SearchTurns after reset: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("turns written after ResetAll are not searchable (%d hits)", len(hits))
	}
}
