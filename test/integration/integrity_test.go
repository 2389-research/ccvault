// ABOUTME: Drives the real binary through the integrity guards of #104, #105 and #106
// ABOUTME: Covers the schema refusal, stats --strict, and vacuum's pre-swap verdict

package integration

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// stampFutureSchema marks the fixture's archive as carrying a migration no
// ccvault ships, which is what a newer binary leaves behind.
func stampFutureSchema(t *testing.T, dataDir string) int {
	t.Helper()

	path := filepath.Join(dataDir, "ccvault.db")
	pool, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = pool.Close() }()

	var current int
	if err := pool.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&current); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	future := current + 1
	if _, err := pool.Exec("INSERT INTO schema_version (version) VALUES (?)", future); err != nil {
		t.Fatalf("stamp schema version %d: %v", future, err)
	}
	return future
}

// strandTurnsFTSDoc forges the damage of #93 in the fixture's archive: a
// second document at a rowid a live turn already owns. The cheap orphan count
// reads zero afterwards and PRAGMA integrity_check says "ok".
func strandTurnsFTSDoc(t *testing.T, dataDir string) {
	t.Helper()

	path := filepath.Join(dataDir, "ccvault.db")
	pool, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = pool.Close() }()

	var rowid int64
	if err := pool.QueryRow("SELECT rowid FROM turns LIMIT 1").Scan(&rowid); err != nil {
		t.Fatalf("find a live turn rowid: %v", err)
	}
	if _, err := pool.Exec(
		"INSERT INTO turns_fts(rowid, content, search_period) VALUES (?, 'strandedghost', 'ccvym202610')",
		rowid); err != nil {
		t.Fatalf("strand a turns_fts document: %v", err)
	}
}

// TestCLI_RefusesToWriteAnArchiveFromTheFuture is #105 end to end. On
// 2026-10-13 a v0.2.0 binary opened a schema-13 archive read-write, deleted
// 35,090 turns and stranded 136,279 index documents, and said nothing. A
// binary that does not understand the archive must not write to it.
func TestCLI_RefusesToWriteAnArchiveFromTheFuture(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)
	f.Run(t, "sync")
	future := stampFutureSchema(t, f.DataDir)

	// The two writes a user reaches for by hand. `import` is a write too and
	// takes the same guard, through the same Open.
	for _, args := range [][]string{
		{"sync"},
		{"vacuum"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := f.TryRun(t, args...)
			if r.Err == nil {
				t.Fatalf("ccvault %s wrote an archive from the future:\nstdout:\n%s", strings.Join(args, " "), r.Stdout)
			}
			said := r.Stdout + r.Stderr
			for _, want := range []string{"schema version", "upgrade"} {
				if !strings.Contains(strings.ToLower(said), want) {
					t.Errorf("refusal does not mention %q:\n%s", want, said)
				}
			}
			if !strings.Contains(said, "13") && !strings.Contains(said, "14") {
				t.Errorf("refusal names no version numbers (archive is at %d):\n%s", future, said)
			}
		})
	}
}

// TestCLI_ReadsAnArchiveFromTheFutureWithAWarning. Refusing a read is more
// disruptive than useful; saying nothing is how the write happens next.
func TestCLI_ReadsAnArchiveFromTheFutureWithAWarning(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)
	f.Run(t, "sync")
	stampFutureSchema(t, f.DataDir)

	r := f.TryRun(t, "stats")
	if r.Err != nil {
		t.Fatalf("stats refused to read an archive from the future: %v\nstderr:\n%s", r.Err, r.Stderr)
	}
	if !strings.Contains(strings.ToLower(r.Stderr), "schema version") {
		t.Errorf("stats read a newer archive without warning:\nstderr:\n%s", r.Stderr)
	}
}

// TestCLI_StatsStrictIsOptInAndSeesAMalformedIndex is #106. The default report
// is a lower bound and says so; --strict is the only thing that returns a
// verdict.
func TestCLI_StatsStrictIsOptInAndSeesAMalformedIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)
	f.Run(t, "sync")

	// Opt-in: the plain report still succeeds and points at the real check.
	clean := f.Run(t, "stats")
	if !strings.Contains(clean.Stdout, "--strict") {
		t.Errorf("the default report does not point at the strict check:\n%s", clean.Stdout)
	}

	strictClean := f.Run(t, "stats", "--strict")
	for _, want := range []string{"turns_fts", "tool_uses_fts"} {
		if !strings.Contains(strictClean.Stdout, want) {
			t.Errorf("stats --strict does not say it checked %s:\n%s", want, strictClean.Stdout)
		}
	}

	strandTurnsFTSDoc(t, f.DataDir)

	// The trap this whole workstream is about: the cheap count reads clean.
	var stats struct {
		SearchIndex struct {
			Consistent bool `json:"consistent"`
			Orphaned   int  `json:"orphaned"`
		} `json:"search_index"`
	}
	f.Run(t, "stats", "--json").JSON(t, &stats)
	if !stats.SearchIndex.Consistent || stats.SearchIndex.Orphaned != 0 {
		t.Fatalf("the quick check already sees this damage (%+v), so --strict proves nothing", stats.SearchIndex)
	}

	bad := f.TryRun(t, "stats", "--strict")
	if bad.Err == nil {
		t.Fatalf("stats --strict passed a malformed index:\n%s", bad.Stdout)
	}
	said := bad.Stdout + bad.Stderr
	if !strings.Contains(said, "turns_fts") {
		t.Errorf("stats --strict does not name the index that failed:\n%s", said)
	}
	if !strings.Contains(said, "repair-index") {
		t.Errorf("stats --strict does not name the repair:\n%s", said)
	}

	// --json has to carry the same verdict, or an agent reading it is back
	// where the author was: a clean-looking number over a broken index.
	var strictJSON struct {
		SearchIndexStrict struct {
			Checked   []string `json:"checked"`
			Malformed []string `json:"malformed"`
			OK        bool     `json:"ok"`
		} `json:"search_index_strict"`
	}
	badJSON := f.TryRun(t, "stats", "--strict", "--json")
	badJSON.JSON(t, &strictJSON)
	if strictJSON.SearchIndexStrict.OK {
		t.Errorf("stats --strict --json reports ok on a malformed index: %+v", strictJSON.SearchIndexStrict)
	}
	if len(strictJSON.SearchIndexStrict.Checked) != 2 {
		t.Errorf("stats --strict --json checked %v, want both indexes", strictJSON.SearchIndexStrict.Checked)
	}
}

// TestCLI_VacuumRefusesToSwapAMalformedCopy is #104. The copy verifies
// structurally and by row count and is still a bad database; compaction is the
// one operation whose bad outcome re-running cannot undo.
func TestCLI_VacuumRefusesToSwapAMalformedCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	f := newCLIFixture(t)
	f.Run(t, "sync")
	strandTurnsFTSDoc(t, f.DataDir)

	r := f.TryRun(t, "vacuum")
	if r.Err == nil {
		t.Fatalf("vacuum swapped in a copy with a malformed index:\n%s", r.Stdout)
	}
	said := r.Stdout + r.Stderr
	if !strings.Contains(said, "turns_fts") {
		t.Errorf("vacuum does not name the index that failed:\n%s", said)
	}
	if !strings.Contains(said, "repair-index") {
		t.Errorf("vacuum does not name the repair:\n%s", said)
	}

	// The repair it names has to actually unblock it. #135: that used to be a
	// re-parse of every transcript; it is now the rebuild the index is
	// derivable from.
	f.Run(t, "repair-index")
	if r := f.TryRun(t, "vacuum"); r.Err != nil {
		t.Errorf("vacuum still refused after the repair it told the user to run: %v\n%s%s",
			r.Err, r.Stdout, r.Stderr)
	}
}
