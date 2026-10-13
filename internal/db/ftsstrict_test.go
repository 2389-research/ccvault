// ABOUTME: Tests the strict FTS5 integrity-check that the cheap orphan count cannot replace
// ABOUTME: Proves it fails on a deliberately malformed index and covers every FTS table

package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/2389-research/ccvault/pkg/models"
)

// seedStrictFixture builds a small archive with content in both FTS indexes.
func seedStrictFixture(t *testing.T, dir string) *DB {
	t.Helper()

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	seedTurnSession(t, database, "strict-session")
	turns := []models.Turn{
		{ID: "turn-one", SessionID: "strict-session", Type: "user", Timestamp: time.Now(), Content: "alpha bravo"},
		{ID: "turn-two", SessionID: "strict-session", Type: "assistant", Timestamp: time.Now(), Content: "charlie delta", Ordinal: 1},
	}
	if err := database.InsertTurns(turns); err != nil {
		t.Fatalf("insert turns: %v", err)
	}
	uses := []models.ToolUse{
		{TurnID: "turn-two", SessionID: "strict-session", ToolName: "Bash", Timestamp: time.Now(), InputJSON: `{"command":"echo echoecho"}`},
	}
	if err := database.InsertToolUses(uses); err != nil {
		t.Fatalf("insert tool uses: %v", err)
	}
	return database
}

// strandTurnsFTSDocument forges the condition the cheap check cannot see: a
// second document in the index at a rowid that a live turn already owns.
//
// This is #106's measurement in miniature. An index entry whose rowid has been
// reused leaves %_docsize holding exactly one row per turn, so the orphan count
// is zero and Consistent() is true — while the index still holds the stranded
// terms and still answers queries with them.
func strandTurnsFTSDocument(t *testing.T, database *DB, rowid int64) {
	t.Helper()

	if _, err := database.Exec(
		`INSERT INTO turns_fts(rowid, content, search_period) VALUES (?, 'strandedghost', 'ccvym202610')`,
		rowid,
	); err != nil {
		t.Fatalf("strand a turns_fts document: %v", err)
	}
}

// TestCheckFTSStrict_PassesOnASoundArchive. A check that reported trouble on a
// healthy archive would be as useless as one that cannot fail.
func TestCheckFTSStrict_PassesOnASoundArchive(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict check: %v", err)
	}
	if !report.OK() {
		t.Errorf("strict check failed on a sound archive: %v", report.Err())
	}
}

// TestCheckFTSStrict_CoversEveryFTSIndex. A strict check that silently
// inspected nothing would pass on every archive, which is worse than no check
// at all: it manufactures confidence. The report names what it looked at so
// that "clean" is always attributable.
func TestCheckFTSStrict_CoversEveryFTSIndex(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict check: %v", err)
	}

	want := map[string]bool{"turns_fts": false, "tool_uses_fts": false}
	for _, table := range report.Tables() {
		if _, ok := want[table]; !ok {
			t.Errorf("strict check inspected unexpected table %q", table)
			continue
		}
		want[table] = true
	}
	for table, seen := range want {
		if !seen {
			t.Errorf("strict check never inspected %s", table)
		}
	}
}

// TestCheckFTSStrict_FailsOnAMalformedTurnsIndex is the acceptance criterion
// for #106. The cheap count says clean; the strict check must not.
func TestCheckFTSStrict_FailsOnAMalformedTurnsIndex(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	var rowid int64
	if err := database.QueryRow("SELECT rowid FROM turns WHERE id = 'turn-one'").Scan(&rowid); err != nil {
		t.Fatalf("find a live turn rowid: %v", err)
	}
	strandTurnsFTSDocument(t, database, rowid)

	// The premise of the test: the cheap check has nothing to report, and the
	// stranded content is still matchable. Without both, the strict check below
	// would be proving nothing.
	cheap, err := database.CheckFTSIntegrity()
	if err != nil {
		t.Fatalf("cheap integrity check: %v", err)
	}
	if !cheap.Consistent() {
		t.Fatalf("the cheap check already sees this damage (%+v), so it cannot show what strict adds", cheap)
	}
	if n := ftsMatchCount(t, database, "{content} : (strandedghost)"); n == 0 {
		t.Fatal("the stranded document is not in the index, so there is nothing to detect")
	}

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict check: %v", err)
	}
	if report.OK() {
		t.Fatal("strict check passed a malformed turns_fts index")
	}
	if !strings.Contains(strings.Join(report.Malformed(), ","), "turns_fts") {
		t.Errorf("strict check blamed %v, want turns_fts", report.Malformed())
	}
	if report.Err() == nil {
		t.Error("report.Err() is nil on a failed check, so a caller judging by the error sees success")
	}
}

// TestCheckFTSStrict_FailsOnAMalformedToolUsesIndex. #93 left tool_uses_fts
// sound and turns_fts shredded, which is exactly why a check that only looked
// at turns_fts would have been believed.
func TestCheckFTSStrict_FailsOnAMalformedToolUsesIndex(t *testing.T) {
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

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict check: %v", err)
	}
	if report.OK() {
		t.Fatal("strict check passed a malformed tool_uses_fts index")
	}
	if !strings.Contains(strings.Join(report.Malformed(), ","), "tool_uses_fts") {
		t.Errorf("strict check blamed %v, want tool_uses_fts", report.Malformed())
	}
}

// TestCheckFTSStrict_NamesTheRepair. A verdict the user cannot act on costs
// them the same hour it cost the author.
func TestCheckFTSStrict_NamesTheRepair(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	var rowid int64
	if err := database.QueryRow("SELECT rowid FROM turns WHERE id = 'turn-one'").Scan(&rowid); err != nil {
		t.Fatalf("find a live turn rowid: %v", err)
	}
	strandTurnsFTSDocument(t, database, rowid)

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict check: %v", err)
	}
	if !strings.Contains(report.Err().Error(), "repair-index") {
		t.Errorf("failure does not name the repair: %v", report.Err())
	}
}

// TestCheckFTSStrict_SurvivesPlainIntegrityCheck records why the strict check
// has to exist at all: SQLite's own structural check calls the same database
// sound.
func TestCheckFTSStrict_SurvivesPlainIntegrityCheck(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	var rowid int64
	if err := database.QueryRow("SELECT rowid FROM turns WHERE id = 'turn-one'").Scan(&rowid); err != nil {
		t.Fatalf("find a live turn rowid: %v", err)
	}
	strandTurnsFTSDocument(t, database, rowid)

	var check string
	if err := database.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatalf("pragma integrity_check: %v", err)
	}
	if check != "ok" {
		t.Skipf("PRAGMA integrity_check reported %q, so this premise no longer holds", check)
	}

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("run strict check: %v", err)
	}
	if report.OK() {
		t.Error("PRAGMA integrity_check says ok and so does the strict check, so nothing can see this damage")
	}
}

// TestListFTSTables_FindsAnIndexDeclaredWithOddWhitespace is #137. Discovery
// used to match `lower(sql) LIKE '%using fts5%'`, so an index whose
// declaration put a newline or a second space between USING and fts5 fell out
// of --strict's coverage silently: the report simply listed one fewer table.
// Formatting cannot defeat a structural test.
func TestListFTSTables_FindsAnIndexDeclaredWithOddWhitespace(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	if _, err := database.Exec("CREATE VIRTUAL TABLE oddly_fts USING\n  fts5(body)"); err != nil {
		t.Fatalf("create an oddly-formatted fts5 index: %v", err)
	}

	tables, err := listFTSTables(context.Background(), database.DB)
	if err != nil {
		t.Fatalf("list fts5 indexes: %v", err)
	}
	if !containsString(tables, "oddly_fts") {
		t.Errorf("listFTSTables found %v, missing the oddly-formatted index oddly_fts", tables)
	}
}

// TestListFTSTables_IgnoresEverythingThatIsNotAnFTS5Index. The other half of
// the guarantee: a structural test that over-reported would send the strict
// check at a table whose integrity-check does not exist.
func TestListFTSTables_IgnoresEverythingThatIsNotAnFTS5Index(t *testing.T) {
	database := seedStrictFixture(t, t.TempDir())

	if _, err := database.Exec("CREATE TABLE plain_fts (body TEXT)"); err != nil {
		t.Fatalf("create a plain table: %v", err)
	}
	if _, err := database.Exec("CREATE VIEW turns_fts_view AS SELECT 1"); err != nil {
		t.Fatalf("create a view: %v", err)
	}
	// An fts4 index is a virtual table with shadow tables of its own, and the
	// one case a careless structural test would swallow. It has no
	// 'integrity-check', so the strict check must not be pointed at it.
	fts4Created := true
	if _, err := database.Exec("CREATE VIRTUAL TABLE legacy_fts USING fts4(body)"); err != nil {
		fts4Created = false
		t.Logf("this build has no fts4, so that half of the assertion is untested: %v", err)
	}
	if fts4Created {
		defer func() { _, _ = database.Exec("DROP TABLE legacy_fts") }()
	}

	tables, err := listFTSTables(context.Background(), database.DB)
	if err != nil {
		t.Fatalf("list fts5 indexes: %v", err)
	}
	unwanted := []string{"plain_fts", "turns_fts_view", "turns_fts_data", "turns_fts_idx", "turns_fts_config", "turns_fts_docsize"}
	if fts4Created {
		unwanted = append(unwanted, "legacy_fts")
	}
	for _, name := range unwanted {
		if containsString(tables, name) {
			t.Errorf("listFTSTables reported %q as an fts5 index: %v", name, tables)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
