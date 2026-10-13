// ABOUTME: Tests the CLI surface of the strict search-index check and the schema guard
// ABOUTME: Covers the stats --strict rendering, its JSON shape, and its exit verdict

package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

// TestPrintIntegritySection_SaysWhatItChecked. #106: a user reading
// "Orphaned: 0" concluded the index was fine while FTS5 called the same
// database malformed. The number is a lower bound and the section has to say
// so, or it is the most convincing wrong answer in the report.
func TestPrintIntegritySection_SaysWhatItChecked(t *testing.T) {
	out := captureStdout(t, func() {
		printIntegritySection(db.FTSIntegrity{Turns: 4200, Indexed: 4200}, false)
	})

	for _, want := range []string{"quick check", "stats --strict"} {
		if !strings.Contains(out, want) {
			t.Errorf("the search-index section does not mention %q:\n%s", want, out)
		}
	}
}

// TestStrictJSON_CarriesTheVerdictAndItsCoverage. An agent reading
// `"ok": true` off a run that inspected nothing would be exactly where the
// author was: a clean-looking report over a broken index.
func TestStrictJSON_CarriesTheVerdictAndItsCoverage(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}

	out := strictJSON(report)
	if ok, isBool := out["ok"].(bool); !isBool || !ok {
		t.Errorf("ok = %#v on a sound archive, want true", out["ok"])
	}
	checked, isSlice := out["checked"].([]string)
	if !isSlice || len(checked) != 2 {
		t.Errorf("checked = %#v, want both FTS indexes", out["checked"])
	}
	if malformed, isSlice := out["malformed"].([]string); !isSlice || len(malformed) != 0 {
		t.Errorf("malformed = %#v, want an empty list rather than null", out["malformed"])
	}
	if _, present := out["error"]; present {
		t.Errorf("a passing check carries an error field: %#v", out["error"])
	}

	// A nil slice encodes as `null`, which a consumer has to special-case
	// before it can ask for a length.
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(encoded), `"malformed":null`) {
		t.Errorf("malformed encodes as null rather than an empty list: %s", encoded)
	}
}

// TestStrictVerdict_FailsTheCommandOnlyWhenTheCheckRanAndFailed.
//
// The trap this workstream exists to avoid: `sqlite3` exits 0 when the FTS
// integrity-check fails, because the error goes to stderr while the status
// stays clean — which is how a "PASS" gets printed directly above a
// malformed-index error. `stats --strict` is an assertion, so a failed
// assertion has to be a failed command.
func TestStrictVerdict_FailsTheCommandOnlyWhenTheCheckRanAndFailed(t *testing.T) {
	if err := strictVerdict(false, db.FTSStrictReport{}); err != nil {
		t.Errorf("a run without --strict returned %v, want nil", err)
	}

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	sound, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}
	if err := strictVerdict(true, sound); err != nil {
		t.Errorf("a passing check returned %v, want nil", err)
	}

	// Strand a document at a rowid the index already describes, the shape the
	// cheap count cannot see.
	if _, err := database.Exec(
		`INSERT INTO turns_fts(rowid, content, search_period) VALUES (1, 'strandedghost', 'ccvym202610')`,
	); err != nil {
		t.Fatalf("strand a document: %v", err)
	}
	broken, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}
	verdict := strictVerdict(true, broken)
	if verdict == nil {
		t.Fatal("a failed check returned nil, so the command would exit 0 over a malformed index")
	}
	if !strings.Contains(verdict.Error(), "repair-index") {
		t.Errorf("the verdict does not name the repair: %v", verdict)
	}
}

// TestPrintStrictSection_NamesEveryIndexItInspected. A verdict is only worth
// anything if the reader can tell what it covered.
func TestPrintStrictSection_NamesEveryIndexItInspected(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = database.Close() }()

	report, err := database.CheckFTSStrict()
	if err != nil {
		t.Fatalf("strict check: %v", err)
	}
	out := captureStdout(t, func() { printStrictSection(report) })
	for _, want := range []string{"turns_fts", "tool_uses_fts", "ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("strict section does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "repair-index") {
		t.Errorf("a passing strict section suggests a repair:\n%s", out)
	}
}

// TestNameTheBinary_OnlyDecoratesASchemaRefusal. Which ccvault is running is
// the fact that diagnosed #105 — three installs ahead of the repo on PATH —
// and it has no business on an unrelated error.
func TestNameTheBinary_OnlyDecoratesASchemaRefusal(t *testing.T) {
	refusal := nameTheBinary(&db.SchemaTooNewError{ArchiveVersion: 14, KnownVersion: 13})
	if !errors.Is(refusal, db.ErrSchemaTooNew) {
		t.Errorf("decorating lost the error identity: %v", refusal)
	}
	if !strings.Contains(refusal.Error(), "running ccvault") {
		t.Errorf("the refusal does not name the running binary: %v", refusal)
	}

	other := errors.New("disk on fire")
	got := nameTheBinary(other)
	if !errors.Is(got, other) || got.Error() != other.Error() {
		t.Errorf("nameTheBinary rewrote an unrelated error: %v", got)
	}
}
