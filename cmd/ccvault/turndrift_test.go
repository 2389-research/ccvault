// ABOUTME: Tests the turn-count section of `ccvault stats` — that it names each cause of drift and only prescribes a recovery that can work (#121).
// ABOUTME: Red first: the section printed one figure and one instruction, and on the author's archive that instruction could not be satisfied.

package main

import (
	"strings"
	"testing"

	"github.com/2389-research/ccvault/internal/db"
)

// TestPrintTurnCountSection_SilentOnAHealthyArchive is the floor: a clean
// archive should not have to read a line saying so. The same discipline the
// storage, raw_json and cache-token sections already hold.
func TestPrintTurnCountSection_SilentOnAHealthyArchive(t *testing.T) {
	out := captureStdout(t, func() {
		printTurnCountSection(db.TurnDrift{Counted: 100, Rows: 100})
	})
	if out != "" {
		t.Errorf("a healthy archive printed a turn-count section:\n%s", out)
	}
}

// TestPrintTurnCountSection_NamesEachCause is #121's deliverable. The author's
// archive decomposes into 3 sessions whose transcripts are gone and 1 whose
// surplus lines cannot be stored, and a report that says so is a report
// somebody reads once; a single number with a recovery command attached is a
// line that gets skipped every run.
func TestPrintTurnCountSection_NamesEachCause(t *testing.T) {
	drift := db.TurnDrift{
		Counted:  1098229 + 8716,
		Rows:     1098229,
		Sessions: 4,
		Causes: []db.DriftByCause{
			{Cause: db.DriftTranscriptGone, Sessions: 3, Claimed: 10332, Rows: 1638},
			{Cause: db.DriftUnstorable, Sessions: 1, Claimed: 125, Rows: 103},
		},
	}

	out := captureStdout(t, func() { printTurnCountSection(drift) })

	for _, want := range []string{
		"Turn counts:",
		"4 session(s)",
		"3 session(s)",
		"no longer on disk",
		"1 session(s)",
		"cannot be stored",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the section does not mention %q:\n%s", want, out)
		}
	}

	// The instruction is the point. Every turn of this drift is accounted for
	// and none of it is recoverable, so telling a user to run a recovery is
	// telling them to spend 46,000 file reads to change nothing.
	if strings.Contains(out, "sync --full") {
		t.Errorf("the section prescribes a recovery that cannot help:\n%s", out)
	}
}

// TestPrintTurnCountSection_PrescribesRecoveryOnlyWhenItCanWork is the other
// half: drift from a transcript that is on disk and not yet re-parsed is
// exactly what `sync --full` repairs, and that case must still say so.
func TestPrintTurnCountSection_PrescribesRecoveryOnlyWhenItCanWork(t *testing.T) {
	drift := db.TurnDrift{
		Counted:  200,
		Rows:     100,
		Sessions: 3,
		Causes: []db.DriftByCause{
			{Cause: db.DriftTranscriptGone, Sessions: 1, Claimed: 50, Rows: 10},
			{Cause: db.DriftUnparsed, Sessions: 2, Claimed: 150, Rows: 90},
		},
	}

	out := captureStdout(t, func() { printTurnCountSection(drift) })

	if !strings.Contains(out, "sync --full") {
		t.Errorf("recoverable drift printed no recovery instruction:\n%s", out)
	}
	if !strings.Contains(out, "no longer on disk") {
		t.Errorf("the unrecoverable share went unnamed:\n%s", out)
	}
}

// TestPrintTurnCountSection_ReportsWhatTheBoundCouldNotReach keeps the
// bounded classification honest: sessions past the bound are said out loud,
// because a breakdown that silently omitted them would under-report the drift
// it exists to explain.
func TestPrintTurnCountSection_ReportsWhatTheBoundCouldNotReach(t *testing.T) {
	drift := db.TurnDrift{
		Counted:  5000,
		Rows:     100,
		Sessions: 1200,
		Causes: []db.DriftByCause{
			{Cause: db.DriftTranscriptGone, Sessions: 1000, Claimed: 4000, Rows: 80},
			{Cause: db.DriftUnclassified, Sessions: 200},
		},
	}

	out := captureStdout(t, func() { printTurnCountSection(drift) })

	if !strings.Contains(out, "200 session(s)") {
		t.Errorf("the unclassified sessions went unreported:\n%s", out)
	}
	if !strings.Contains(out, "sync --full") {
		t.Errorf("unclassified drift printed no instruction, though nothing ruled one out:\n%s", out)
	}
}

// TestTurnCountDriftJSON_CarriesTheCauses holds the same split for the machine
// reader. `stats --json` is what an agent reads, and a cause an agent cannot
// see is a cause it will keep asking about.
func TestTurnCountDriftJSON_CarriesTheCauses(t *testing.T) {
	drift := db.TurnDrift{
		Counted:  8716 + 100,
		Rows:     100,
		Sessions: 4,
		Causes: []db.DriftByCause{
			{Cause: db.DriftTranscriptGone, Sessions: 3, Claimed: 10332, Rows: 1638},
			{Cause: db.DriftUnstorable, Sessions: 1, Claimed: 125, Rows: 103},
		},
	}

	out := turnCountDriftJSON(drift)

	if out["sessions"] != 4 {
		t.Errorf("sessions = %v, want 4", out["sessions"])
	}
	if out["actionable"] != false {
		t.Errorf("actionable = %v, want false", out["actionable"])
	}

	causes, ok := out["causes"].([]map[string]interface{})
	if !ok {
		t.Fatalf("causes is %#v, want a list", out["causes"])
	}
	if len(causes) != 2 {
		t.Fatalf("got %d causes, want 2", len(causes))
	}
	if causes[0]["cause"] != "transcript-gone" {
		t.Errorf("first cause is %v, want transcript-gone", causes[0]["cause"])
	}
	if causes[1]["cause"] != "unstorable" {
		t.Errorf("second cause is %v, want unstorable", causes[1]["cause"])
	}
	if causes[0]["sessions"] != 3 || causes[0]["claimed"] != 10332 || causes[0]["rows"] != 1638 {
		t.Errorf("first cause carries %#v", causes[0])
	}
}
