// ABOUTME: Tests the repair-index command end to end against a deliberately malformed index
// ABOUTME: Asserts the strict check fails before the repair and passes after it

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/internal/db"
	"github.com/2389-research/ccvault/pkg/models"
)

// seedMalformedArchive builds an archive whose turns_fts index holds a
// stranded document: a second entry at a rowid a live turn already owns. The
// cheap orphan count cannot see it; FTS5's strict check calls it malformed.
func seedMalformedArchive(t *testing.T, dir string) {
	t.Helper()

	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = database.Close() }()

	project := &models.Project{Path: "/proj/repair", DisplayName: "repair"}
	if err := database.UpsertProject(project); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	session := &models.Session{
		ID:         "repair-session",
		ProjectID:  project.ID,
		StartedAt:  time.Now().Add(-time.Hour),
		EndedAt:    time.Now(),
		SourceFile: "/proj/repair/repair-session.jsonl",
		Source:     "claude-code",
	}
	if err := database.UpsertSession(session); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	turns := []models.Turn{
		{ID: "turn-one", SessionID: "repair-session", Type: "user", Timestamp: time.Now(), Content: "alpha bravo"},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}

	var rowid int64
	if err := database.QueryRow("SELECT rowid FROM turns WHERE id = 'turn-one'").Scan(&rowid); err != nil {
		t.Fatalf("find a live turn rowid: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO turns_fts(rowid, content, search_period) VALUES (?, 'strandedghost', 'ccvym202610')`,
		rowid,
	); err != nil {
		t.Fatalf("strand a turns_fts document: %v", err)
	}
}

// strictVerdictOf reopens the archive and reports whether FTS5 calls every
// index sound.
func strictVerdictOf(t *testing.T, dir string) bool {
	t.Helper()

	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("reopen archive: %v", err)
	}
	defer func() { _ = database.Close() }()

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}
	return report.OK()
}

// TestRunRepairIndex_RepairsAMalformedIndex is #135. The advice `--strict`
// used to give was `ccvault sync --full`, which re-reads every transcript in
// the archive — 47,000 files on the author's — to repair a structure derived
// entirely from the turns table. This is the 22-second alternative.
func TestRunRepairIndex_RepairsAMalformedIndex(t *testing.T) {
	dir := t.TempDir()
	seedMalformedArchive(t, dir)

	if strictVerdictOf(t, dir) {
		t.Fatal("the index is sound before the repair, so repairing it proves nothing")
	}

	var err error
	out := captureStdout(t, func() {
		err = runRepairIndex(dir, false)
	})
	if err != nil {
		t.Fatalf("repair-index on a malformed archive: %v\n%s", err, out)
	}

	if !strictVerdictOf(t, dir) {
		t.Fatalf("the index is still malformed after repair-index:\n%s", out)
	}

	// It has to say what it rebuilt, for the reason --strict names what it
	// checked: a repair that silently touched nothing reads exactly like one
	// that worked.
	for _, want := range []string{"turns_fts", "tool_uses_fts"} {
		if !strings.Contains(out, want) {
			t.Errorf("repair-index never mentions %s:\n%s", want, out)
		}
	}
}

// TestRunRepairIndex_RunsWithAMigrationPending is the bootstrapping case, and
// the reason this is a command rather than a flag on sync.
//
// On 2026-10-13 migration 012 — which ends in FTS5's own integrity-check —
// aborted on the live archive's malformed turns_fts. Opening an archive runs
// migrations, so the damage stood in front of every in-tool path to its own
// repair, and the fix had to be typed into raw sqlite3.
func TestRunRepairIndex_RunsWithAMigrationPending(t *testing.T) {
	dir := t.TempDir()
	seedMalformedArchive(t, dir)

	known, err := db.MaxKnownSchemaVersion()
	if err != nil {
		t.Fatalf("read the highest migration this binary ships: %v", err)
	}
	pending := known - 1
	if pending < 1 {
		t.Skip("this binary ships fewer than two migrations, so nothing can be pending")
	}
	rollBackSchemaVersion(t, dir, pending)

	out := captureStdout(t, func() {
		err = runRepairIndex(dir, false)
	})
	if err != nil {
		t.Fatalf("repair-index with a migration pending: %v\n%s", err, out)
	}

	// Reopening with db.Open would migrate the archive, so check the verdict
	// on the same un-migrated file the repair ran against.
	repaired, err := db.OpenForRepair(dir)
	if err != nil {
		t.Fatalf("reopen for repair: %v", err)
	}
	defer func() { _ = repaired.Close() }()

	report, err := repaired.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}
	if !report.OK() {
		t.Fatalf("the index is still malformed: %v\n%s", report.Err(), out)
	}

	var recorded int
	if err := repaired.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&recorded); err != nil {
		t.Fatalf("read the recorded schema version: %v", err)
	}
	if recorded != pending {
		t.Errorf("repair-index migrated the archive from %d to %d", pending, recorded)
	}
}

// rollBackSchemaVersion forgets every migration above version, leaving the
// archive in the state where an open would try to migrate it.
func rollBackSchemaVersion(t *testing.T, dir string, version int) {
	t.Helper()

	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = database.Close() }()

	if _, err := database.Exec("DELETE FROM schema_version WHERE version > ?", version); err != nil {
		t.Fatalf("roll the recorded schema version back: %v", err)
	}
}

// TestRepairAdvice_PointsAtTheCheapRepair. The verdict is load-bearing: it is
// the line that tells a user what to do about an index that is lying to them,
// and it used to send them at a full re-parse.
func TestRepairAdvice_PointsAtTheCheapRepair(t *testing.T) {
	dir := t.TempDir()
	seedMalformedArchive(t, dir)

	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = database.Close() }()

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}

	out := captureStdout(t, func() { printStrictSection(report) })
	if !strings.Contains(out, "repair-index") {
		t.Errorf("the strict section does not name repair-index:\n%s", out)
	}
	if strings.Contains(out, "sync --full") {
		t.Errorf("the strict section still advises a full re-parse:\n%s", out)
	}

	integrity := captureStdout(t, func() {
		printIntegritySection(db.FTSIntegrity{Turns: 10, Indexed: 12, Orphaned: 2}, false)
	})
	if !strings.Contains(integrity, "repair-index") {
		t.Errorf("the quick-check section does not name repair-index:\n%s", integrity)
	}
}
