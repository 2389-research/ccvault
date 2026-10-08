// ABOUTME: Tests the invalid-raw_json integrity report that makes issue #101 visible
// ABOUTME: Covers a clean archive, damaged turns, and the bounded newest-turns window

package db

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedTurnsForRawJSONCheck writes one session and n turns, every raw_json a
// well-formed JSONL entry, and returns the session's source file path.
func seedTurnsForRawJSONCheck(t *testing.T, db *DB, sessionID string, n int) string {
	t.Helper()

	sourceFile := filepath.Join(t.TempDir(), sessionID+".jsonl")
	session := &models.Session{
		ID:         sessionID,
		StartedAt:  time.Now().Add(-time.Hour),
		EndedAt:    time.Now(),
		TurnCount:  n,
		SourceFile: sourceFile,
	}
	if err := db.UpsertSession(session); err != nil {
		t.Fatalf("upsert session %s: %v", sessionID, err)
	}

	turns := make([]models.Turn, 0, n)
	for i := 0; i < n; i++ {
		turns = append(turns, models.Turn{
			ID:        fmt.Sprintf("%s-turn-%d", sessionID, i),
			SessionID: sessionID,
			Type:      "assistant",
			Timestamp: time.Now(),
			Ordinal:   i,
			Content:   fmt.Sprintf("turn %d of %s", i, sessionID),
			RawJSON:   json.RawMessage(fmt.Sprintf(`{"uuid":"%s-turn-%d","type":"assistant"}`, sessionID, i)),
		})
	}
	if err := db.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns for %s: %v", sessionID, err)
	}
	return sourceFile
}

// damageRawJSON overwrites the raw_json of the turns at the given ordinals with
// a mid-document fragment, which is the shape the live archive holds.
//
// The stored bytes are not a truncation of the turn's own line — they are a
// window into unrelated bytes. The historical write path handed SQLite a slice
// of bufio.Scanner's internal buffer (pkg/parser before commit e0ffbca), and
// the buffer had moved on by the time the row was written, so the value stored
// was whatever else the reader held. Damaging rows the same way is what makes
// the test's premise match the archive's.
func damageRawJSON(t *testing.T, db *DB, sessionID string, ordinals ...int) {
	t.Helper()

	const foreign = `ue","description":"a fragment of some other turn entirely"},{"na`
	for _, ordinal := range ordinals {
		res, err := db.Exec(
			"UPDATE turns SET raw_json = ? WHERE session_id = ? AND ordinal = ?",
			[]byte(foreign), sessionID, ordinal)
		if err != nil {
			t.Fatalf("damage %s ordinal %d: %v", sessionID, ordinal, err)
		}
		affected, err := res.RowsAffected()
		if err != nil || affected != 1 {
			t.Fatalf("damage %s ordinal %d: affected %d rows (err %v), want 1",
				sessionID, ordinal, affected, err)
		}
	}
}

// TestCheckRawJSONIntegrityCleanArchive is the baseline: a report that cried
// damage on well-formed rows would be worse than no report at all.
func TestCheckRawJSONIntegrityCleanArchive(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	seedTurnsForRawJSONCheck(t, db, "clean-session", 12)

	got, err := db.CheckRawJSONIntegrity(0)
	if err != nil {
		t.Fatalf("check raw_json integrity: %v", err)
	}

	if got.Turns != 12 || got.Scanned != 12 {
		t.Errorf("Turns/Scanned = %d/%d, want 12/12", got.Turns, got.Scanned)
	}
	if got.Invalid != 0 || got.Sessions != 0 {
		t.Errorf("Invalid/Sessions = %d/%d, want 0/0", got.Invalid, got.Sessions)
	}
	if len(got.SourceFiles) != 0 {
		t.Errorf("SourceFiles = %v, want none", got.SourceFiles)
	}
	if !got.Complete {
		t.Error("Complete = false, want true for a whole-archive scan")
	}
	if !got.Consistent() {
		t.Error("Consistent() = false on a clean archive")
	}
}

// TestCheckRawJSONIntegrityCountsDamagedTurns asserts the report names both the
// turns and the sessions, and hands back the source files that decide whether a
// re-parse can repair them.
func TestCheckRawJSONIntegrityCountsDamagedTurns(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	fileA := seedTurnsForRawJSONCheck(t, db, "damaged-a", 6)
	fileB := seedTurnsForRawJSONCheck(t, db, "damaged-b", 4)
	seedTurnsForRawJSONCheck(t, db, "untouched", 5)

	damageRawJSON(t, db, "damaged-a", 0, 2, 4)
	damageRawJSON(t, db, "damaged-b", 1)

	got, err := db.CheckRawJSONIntegrity(0)
	if err != nil {
		t.Fatalf("check raw_json integrity: %v", err)
	}

	if got.Turns != 15 || got.Scanned != 15 {
		t.Errorf("Turns/Scanned = %d/%d, want 15/15", got.Turns, got.Scanned)
	}
	if got.Invalid != 4 {
		t.Errorf("Invalid = %d, want 4", got.Invalid)
	}
	if got.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", got.Sessions)
	}
	if got.Consistent() {
		t.Error("Consistent() = true with 4 damaged turns")
	}

	want := map[string]bool{fileA: true, fileB: true}
	if len(got.SourceFiles) != len(want) {
		t.Fatalf("SourceFiles = %v, want the two damaged sessions' files", got.SourceFiles)
	}
	for _, path := range got.SourceFiles {
		if !want[path] {
			t.Errorf("SourceFiles contains %q, which is not a damaged session's file", path)
		}
	}
}

// TestCheckRawJSONIntegrityWindow covers the bounded scan `ccvault stats` runs
// by default. json_valid has to read every byte of the column, which is ~35
// seconds over the author's 6 GB archive, so the default report covers the
// newest turns only and says so.
func TestCheckRawJSONIntegrityWindow(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Inserted oldest-first, so "newest" is the second session.
	seedTurnsForRawJSONCheck(t, db, "older", 10)
	seedTurnsForRawJSONCheck(t, db, "newer", 10)

	damageRawJSON(t, db, "older", 0, 1)
	damageRawJSON(t, db, "newer", 5)

	got, err := db.CheckRawJSONIntegrity(10)
	if err != nil {
		t.Fatalf("check raw_json integrity: %v", err)
	}

	if got.Turns != 20 {
		t.Errorf("Turns = %d, want 20 — the total is the whole archive even when the scan is windowed", got.Turns)
	}
	if got.Scanned != 10 {
		t.Errorf("Scanned = %d, want 10", got.Scanned)
	}
	if got.Complete {
		t.Error("Complete = true, want false — the window did not cover the archive")
	}
	if got.Invalid != 1 {
		t.Errorf("Invalid = %d, want 1 — only the damage inside the window is counted", got.Invalid)
	}
	if got.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", got.Sessions)
	}

	// A window at or above the turn count is a whole-archive scan, and has to
	// report itself as one.
	all, err := db.CheckRawJSONIntegrity(20)
	if err != nil {
		t.Fatalf("check raw_json integrity (full window): %v", err)
	}
	if !all.Complete || all.Scanned != 20 || all.Invalid != 3 {
		t.Errorf("window=20: Complete=%v Scanned=%d Invalid=%d, want true/20/3",
			all.Complete, all.Scanned, all.Invalid)
	}
}

// TestCheckRawJSONIntegrityEmptyArchive guards the report on a fresh archive:
// nothing to scan is not an inconsistency.
func TestCheckRawJSONIntegrityEmptyArchive(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	got, err := db.CheckRawJSONIntegrity(0)
	if err != nil {
		t.Fatalf("check raw_json integrity: %v", err)
	}
	if got.Turns != 0 || got.Scanned != 0 || got.Invalid != 0 || !got.Complete || !got.Consistent() {
		t.Errorf("empty archive reported %+v, want zeroes with Complete and Consistent", got)
	}
}
