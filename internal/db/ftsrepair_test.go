// ABOUTME: Tests the FTS5 'rebuild' repair and the open path that reaches it with migrations pending
// ABOUTME: Proves the strict check fails before the rebuild and passes after it

package db

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRebuildFTSIndexes_TurnsIndexFailsStrictBeforeAndPassesAfter is #135's
// acceptance criterion. The repair that worked on the live archive was one
// statement — INSERT INTO turns_fts(turns_fts) VALUES('rebuild') — and the
// test that proves anything has to assert the strict check failed first.
func TestRebuildFTSIndexes_TurnsIndexFailsStrictBeforeAndPassesAfter(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	var rowid int64
	if err := database.QueryRow("SELECT rowid FROM turns WHERE id = 'turn-one'").Scan(&rowid); err != nil {
		t.Fatalf("find a live turn rowid: %v", err)
	}
	strandTurnsFTSDocument(t, database, rowid)

	before, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check before the repair: %v", err)
	}
	if before.OK() {
		t.Fatal("the index is sound before the repair, so repairing it proves nothing")
	}

	results, err := database.RebuildFTSIndexes()
	if err != nil {
		t.Fatalf("rebuild the indexes: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("the rebuild reported no indexes, so it cannot have repaired one")
	}

	after, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check after the repair: %v", err)
	}
	if !after.OK() {
		t.Fatalf("the index is still malformed after the rebuild: %v", after.Err())
	}

	// The whole point of 'rebuild' over a re-parse: the content tables are
	// never read from disk again, so nothing in them can change.
	var turns int64
	if err := database.QueryRow("SELECT COUNT(*) FROM turns").Scan(&turns); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turns != 2 {
		t.Errorf("the rebuild changed the turns table: %d rows, want 2", turns)
	}
	if n := ftsMatchCount(t, database, "{content} : (strandedghost)"); n != 0 {
		t.Errorf("the stranded document still matches after the rebuild (%d hits)", n)
	}
}

// TestRebuildFTSIndexes_RepairsToolUsesToo. #93 left tool_uses_fts sound and
// turns_fts shredded; a repair that only knew about turns_fts would leave the
// next incident half-fixed.
func TestRebuildFTSIndexes_RepairsToolUsesToo(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	var rowid int64
	if err := database.QueryRow("SELECT rowid FROM tool_uses LIMIT 1").Scan(&rowid); err != nil {
		t.Fatalf("find a live tool_uses rowid: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO tool_uses_fts(rowid, input_json, result_content, search_period)
		 VALUES (?, 'strandedghost', 'strandedghost', 'ccvym202610')`,
		rowid,
	); err != nil {
		t.Fatalf("strand a tool_uses_fts document: %v", err)
	}

	before, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check before the repair: %v", err)
	}
	if before.OK() {
		t.Fatal("tool_uses_fts is sound before the repair, so repairing it proves nothing")
	}

	if _, err := database.RebuildFTSIndexes(); err != nil {
		t.Fatalf("rebuild the indexes: %v", err)
	}

	after, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check after the repair: %v", err)
	}
	if !after.OK() {
		t.Fatalf("tool_uses_fts is still malformed after the rebuild: %v", after.Err())
	}
}

// TestRebuildFTSIndexes_CoversEveryIndexItFound. The repair and the check have
// to agree about what "every index" means, or a user reads a clean verdict
// over something the repair never touched.
func TestRebuildFTSIndexes_CoversEveryIndexItFound(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	results, err := database.RebuildFTSIndexes()
	if err != nil {
		t.Fatalf("rebuild the indexes: %v", err)
	}

	want := map[string]bool{"turns_fts": false, "tool_uses_fts": false}
	for _, r := range results {
		if _, known := want[r.Table]; !known {
			t.Errorf("rebuilt unexpected table %q", r.Table)
			continue
		}
		want[r.Table] = true
	}
	for table, seen := range want {
		if !seen {
			t.Errorf("the rebuild never touched %s", table)
		}
	}
}

// TestOpenForRepair_RunsNoMigrations is the bootstrapping case, and the reason
// the repair cannot go through Open.
//
// On 2026-10-13 the live archive's turns_fts was malformed badly enough that
// migration 012 — which ends in FTS5's own integrity-check — aborted. Opening
// the archive runs migrations, so every in-tool path to the repair ran through
// the thing the damage was blocking, and the only way through was raw sqlite3.
// A repair that is unreachable exactly when it is needed is not a repair.
func TestOpenForRepair_RunsNoMigrations(t *testing.T) {
	dir := t.TempDir()
	database := seedStrictFixture(t, dir)

	known, err := MaxKnownSchemaVersion()
	if err != nil {
		t.Fatalf("read the highest migration this binary ships: %v", err)
	}
	pending := known - 1
	if pending < 1 {
		t.Skip("this binary ships fewer than two migrations, so nothing can be pending")
	}

	var rowid int64
	if err := database.QueryRow("SELECT rowid FROM turns WHERE id = 'turn-one'").Scan(&rowid); err != nil {
		t.Fatalf("find a live turn rowid: %v", err)
	}
	strandTurnsFTSDocument(t, database, rowid)

	// Put the archive one migration behind this binary, which is the state in
	// which Open would try to migrate it.
	if _, err := database.Exec("DELETE FROM schema_version WHERE version > ?", pending); err != nil {
		t.Fatalf("roll the recorded schema version back: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close the seeded archive: %v", err)
	}

	repair, err := OpenForRepair(dir)
	if err != nil {
		t.Fatalf("open for repair with a migration pending: %v", err)
	}
	defer func() { _ = repair.Close() }()

	var recorded int
	if err := repair.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&recorded); err != nil {
		t.Fatalf("read the recorded schema version: %v", err)
	}
	if recorded != pending {
		t.Fatalf("opening for repair advanced the schema from %d to %d; the repair must not migrate", pending, recorded)
	}

	if _, err := repair.RebuildFTSIndexes(); err != nil {
		t.Fatalf("rebuild with a migration pending: %v", err)
	}
	report, err := repair.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check after the repair: %v", err)
	}
	if !report.OK() {
		t.Fatalf("the index is still malformed after a repair run with migrations pending: %v", report.Err())
	}
}

// TestOpenForRepair_RefusesAnArchiveThatIsNotThere. A repair that created an
// empty archive would answer "clean" to a user who had mistyped --data-dir,
// and leave a stray file behind.
func TestOpenForRepair_RefusesAnArchiveThatIsNotThere(t *testing.T) {
	dir := t.TempDir()

	if _, err := OpenForRepair(dir); err == nil {
		t.Fatal("opening an absent archive for repair succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, dbFileName)); !os.IsNotExist(err) {
		t.Errorf("opening for repair created an archive at %s", filepath.Join(dir, dbFileName))
	}
}

// TestOpenForRepair_RefusesAnArchiveFromANewerCCVault. A rebuild is a write,
// and #105's rule for writes does not get an exception for being a small one.
func TestOpenForRepair_RefusesAnArchiveFromANewerCCVault(t *testing.T) {
	dir := t.TempDir()
	database := seedStrictFixture(t, dir)

	known, err := MaxKnownSchemaVersion()
	if err != nil {
		t.Fatalf("read the highest migration this binary ships: %v", err)
	}
	if _, err := database.Exec("INSERT INTO schema_version (version) VALUES (?)", known+1); err != nil {
		t.Fatalf("stamp a newer schema version: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close the seeded archive: %v", err)
	}

	_, err = OpenForRepair(dir)
	if err == nil {
		t.Fatal("opening an archive from a newer ccvault for repair succeeded")
	}
	var tooNew *SchemaTooNewError
	if !errors.As(err, &tooNew) {
		t.Fatalf("refusal is not a SchemaTooNewError: %v", err)
	}
}
