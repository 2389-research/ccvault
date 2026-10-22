// ABOUTME: Tests that turn drift is reported in named causes (#121) — "cannot be stored" apart from "transcript gone" apart from "not re-parsed yet".
// ABOUTME: Red first: TurnCountDrift reported one undifferentiated number, so a warning nobody could ever clear was printed on every stats run.

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// driftFixture is one session to seed: what its counter claims, how many turns
// it actually holds, and whether its transcript is on disk and already parsed.
type driftFixture struct {
	id            string
	claimed       int
	held          int
	writeFile     bool
	recordedMtime recordedMtime
}

type recordedMtime int

const (
	// mtimeNone is a session whose transcript has no source_files row at all —
	// never synced, or a row migration 012 deleted so the next sync re-reads it.
	mtimeNone recordedMtime = iota
	// mtimeCurrent is a transcript already parsed at the revision now on disk,
	// so re-reading it cannot produce different rows.
	mtimeCurrent
	// mtimeStale is a transcript that changed since it was last parsed.
	mtimeStale
)

// seedDrift writes the fixtures and returns the data directory holding them.
func seedDrift(t *testing.T, fixtures []driftFixture) (*DB, string) {
	t.Helper()

	dir := t.TempDir()
	transcripts := filepath.Join(dir, "transcripts")
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatalf("mkdir transcripts: %v", err)
	}

	database, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	fileMtime := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	for _, f := range fixtures {
		path := filepath.Join(transcripts, f.id+".jsonl")

		if _, err := database.Exec(`
			INSERT INTO sessions (id, project_id, started_at, turn_count, source, source_file)
			VALUES (?, 1, ?, ?, 'claude-code', ?)`,
			f.id, FormatTimestamp(fileMtime), f.claimed, path); err != nil {
			t.Fatalf("insert session %s: %v", f.id, err)
		}

		for i := 0; i < f.held; i++ {
			if _, err := database.Exec(`
				INSERT INTO turns (id, session_id, type, timestamp, ordinal)
				VALUES (?, ?, 'user', ?, ?)`,
				fmt.Sprintf("%s-turn-%d", f.id, i), f.id, FormatTimestamp(fileMtime), i); err != nil {
				t.Fatalf("insert turn %d of %s: %v", i, f.id, err)
			}
		}

		if f.writeFile {
			if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
				t.Fatalf("write transcript %s: %v", path, err)
			}
			if err := os.Chtimes(path, fileMtime, fileMtime); err != nil {
				t.Fatalf("set mtime on %s: %v", path, err)
			}
		}

		switch f.recordedMtime {
		case mtimeNone:
		case mtimeCurrent:
			if err := database.UpsertSourceFileMtime(path, fileMtime, "claude-code"); err != nil {
				t.Fatalf("record mtime for %s: %v", path, err)
			}
		case mtimeStale:
			if err := database.UpsertSourceFileMtime(path, fileMtime.Add(-time.Hour), "claude-code"); err != nil {
				t.Fatalf("record mtime for %s: %v", path, err)
			}
		}
	}

	return database, dir
}

// TestTurnCountDrift_HealthyArchiveReportsNoCauses is the floor the whole
// issue rests on: an archive whose counters agree with its rows has nothing to
// report, so the warning is silent and therefore means something when it does
// appear.
func TestTurnCountDrift_HealthyArchiveReportsNoCauses(t *testing.T) {
	database, _ := seedDrift(t, []driftFixture{
		{id: "healthy", claimed: 3, held: 3, writeFile: true, recordedMtime: mtimeCurrent},
	})

	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}
	if drift.Drifted() {
		t.Errorf("a healthy archive reports drift: %+v", drift)
	}
	if len(drift.Causes) != 0 {
		t.Errorf("a healthy archive reports causes: %+v", drift.Causes)
	}
	if drift.Actionable() {
		t.Error("a healthy archive reports something to act on")
	}
}

// TestTurnCountDrift_SeparatesTheThreeCauses is #121's subject. The drift on
// the author's archive decomposes into exactly these three, and reporting them
// as one number is what made the warning an attention tax: two of the three
// can never be cleared, so the instruction attached to the number could never
// be satisfied.
func TestTurnCountDrift_SeparatesTheThreeCauses(t *testing.T) {
	database, _ := seedDrift(t, []driftFixture{
		// Transcript gone from disk: nothing can recover these turns.
		{id: "gone-a", claimed: 100, held: 10, writeFile: false},
		{id: "gone-b", claimed: 50, held: 1, writeFile: false},
		// Parsed at the revision now on disk and still short: re-reading it
		// produces the same rows. #121's own case — a transcript repeating a
		// turn uuid within itself — lands here.
		{id: "unstorable", claimed: 125, held: 103, writeFile: true, recordedMtime: mtimeCurrent},
		// On disk and not parsed at its current revision: a sync fixes it.
		{id: "unparsed", claimed: 20, held: 5, writeFile: true, recordedMtime: mtimeStale},
		{id: "never-parsed", claimed: 7, held: 2, writeFile: true, recordedMtime: mtimeNone},
		// Healthy, so it must not appear in any cause.
		{id: "healthy", claimed: 4, held: 4, writeFile: true, recordedMtime: mtimeCurrent},
	})

	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}

	if !drift.Drifted() {
		t.Fatal("drift went unreported")
	}
	if drift.Sessions != 5 {
		t.Errorf("Sessions = %d, want 5", drift.Sessions)
	}

	want := map[DriftCause]struct{ sessions, claimed, rows int }{
		DriftTranscriptGone: {2, 150, 11},
		DriftUnstorable:     {1, 125, 103},
		DriftUnparsed:       {2, 27, 7},
	}

	if len(drift.Causes) != len(want) {
		t.Fatalf("got %d causes, want %d: %+v", len(drift.Causes), len(want), drift.Causes)
	}
	for _, got := range drift.Causes {
		w, ok := want[got.Cause]
		if !ok {
			t.Errorf("unexpected cause %v", got.Cause)
			continue
		}
		if got.Sessions != w.sessions || got.Claimed != w.claimed || got.Rows != w.rows {
			t.Errorf("cause %v: got %d session(s), %d claimed, %d present; want %d/%d/%d",
				got.Cause, got.Sessions, got.Claimed, got.Rows,
				w.sessions, w.claimed, w.rows)
		}
	}

	// The whole point of the split: only the recoverable cause carries an
	// instruction, so the report stops telling a user to run a command that
	// cannot help.
	if !drift.Actionable() {
		t.Error("an archive holding unparsed transcripts reports nothing to act on")
	}
}

// TestTurnCountDrift_UnfixableDriftIsNotActionable is the state the author's
// archive is in once every recoverable transcript has been re-read: drift
// remains, every turn of it is accounted for, and there is nothing to run.
// That is the report #121 asks for — the warning reaching zero in the only
// sense it can.
func TestTurnCountDrift_UnfixableDriftIsNotActionable(t *testing.T) {
	database, _ := seedDrift(t, []driftFixture{
		{id: "gone", claimed: 100, held: 10, writeFile: false},
		{id: "unstorable", claimed: 125, held: 103, writeFile: true, recordedMtime: mtimeCurrent},
	})

	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}
	if !drift.Drifted() {
		t.Fatal("drift went unreported")
	}
	if drift.Actionable() {
		t.Error("drift that nothing can repair is reported as actionable")
	}
}

// TestTurnCountDrift_CausesAreOrdered pins the order the report prints in:
// unrecoverable first, then unstorable, then the one a sync repairs. A map
// iteration would reorder the section between runs, which makes a diff of two
// stats runs unreadable.
func TestTurnCountDrift_CausesAreOrdered(t *testing.T) {
	database, _ := seedDrift(t, []driftFixture{
		{id: "unparsed", claimed: 20, held: 5, writeFile: true, recordedMtime: mtimeStale},
		{id: "unstorable", claimed: 125, held: 103, writeFile: true, recordedMtime: mtimeCurrent},
		{id: "gone", claimed: 100, held: 10, writeFile: false},
	})

	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}

	want := []DriftCause{DriftTranscriptGone, DriftUnstorable, DriftUnparsed}
	if len(drift.Causes) != len(want) {
		t.Fatalf("got %d causes, want %d", len(drift.Causes), len(want))
	}
	for i, w := range want {
		if drift.Causes[i].Cause != w {
			t.Errorf("cause %d is %v, want %v", i, drift.Causes[i].Cause, w)
		}
	}
}

// TestTurnCountDrift_ClassificationIsBounded holds the property every read
// this codebase added after #68: the work does not grow without limit. One
// stat per drifted session is free on a healthy archive and cheap on a damaged
// one, but an archive where every session drifts must not turn a stats run
// into 49,603 stat calls. Past the bound the sessions are reported as
// unclassified rather than silently dropped — and an unclassified session is
// actionable, because nothing has established that it is not.
func TestTurnCountDrift_ClassificationIsBounded(t *testing.T) {
	fixtures := make([]driftFixture, 0, driftClassifyLimit+5)
	for i := 0; i < driftClassifyLimit+5; i++ {
		fixtures = append(fixtures, driftFixture{
			id:      "gone-" + strconv.Itoa(i),
			claimed: 2,
			held:    1,
		})
	}

	database, _ := seedDrift(t, fixtures)

	drift, err := database.TurnCountDrift()
	if err != nil {
		t.Fatalf("TurnCountDrift: %v", err)
	}
	if drift.Sessions != driftClassifyLimit+5 {
		t.Errorf("Sessions = %d, want %d", drift.Sessions, driftClassifyLimit+5)
	}

	var classified, unclassified int
	for _, c := range drift.Causes {
		if c.Cause == DriftUnclassified {
			unclassified += c.Sessions
			continue
		}
		classified += c.Sessions
	}
	if classified != driftClassifyLimit {
		t.Errorf("classified %d session(s), want the bound of %d", classified, driftClassifyLimit)
	}
	if unclassified != 5 {
		t.Errorf("reported %d unclassified session(s), want 5", unclassified)
	}
	if !drift.Actionable() {
		t.Error("unclassified drift is not actionable, so a damaged archive would print no instruction")
	}
}
