// ABOUTME: Tests that a transcript timestamp the parser cannot read is counted and sampled rather than silently replaced.
// ABOUTME: Guards #146 — an unreadable timestamp is currently indistinguishable from a real one, in three independent layout lists.

package parser

import (
	"strings"
	"testing"
)

// TestParseSessionReader_CountsUnparsableTimestamps is #146's visibility half.
//
// The three layout lists in pkg/parser and pkg/adapter/{codex,jeff} are
// deliberately NOT consolidated: each reads a foreign transcript format whose
// producer we do not control, and a shared list would couple five unrelated
// producers together (and make pkg/ depend on internal/db). What they lack is
// not unification but a signal — a timestamp none of the layouts match is
// replaced by a fallback and nothing says so, so it reads back as an ordinary
// value and escapes every date filter.
//
// Counting it is what turns that from silent corruption into a number.
func TestParseSessionReader_CountsUnparsableTimestamps(t *testing.T) {
	const bad = "2026/05/26 14:30:00"
	jsonl := strings.Join([]string{
		`{"uuid":"t1","sessionId":"s-1","type":"user","timestamp":"2026-05-26T14:30:00Z","message":{"role":"user","content":"ok"}}`,
		`{"uuid":"t2","sessionId":"s-1","type":"user","timestamp":"` + bad + `","message":{"role":"user","content":"bad clock"}}`,
		`{"uuid":"t3","sessionId":"s-1","type":"user","timestamp":"","message":{"role":"user","content":"no clock"}}`,
	}, "\n") + "\n"

	turns, _, stats, err := ParseSessionReader(strings.NewReader(jsonl), "s-1.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("len(turns) = %d, want 3 — an unreadable timestamp must not drop the turn", len(turns))
	}

	if stats.TurnsWithUnparsableTimestamp != 2 {
		t.Errorf("TurnsWithUnparsableTimestamp = %d, want 2 (the bad layout and the empty field)",
			stats.TurnsWithUnparsableTimestamp)
	}

	// The sample is the point: a count says "something is unreadable", the
	// sample says which layout to add. #144 reports both for the same reason.
	if stats.UnparsableTimestampSample != bad {
		t.Errorf("UnparsableTimestampSample = %q, want %q", stats.UnparsableTimestampSample, bad)
	}
}

// TestParseSessionReader_CleanTranscriptCountsNothing keeps the counter honest.
// A counter that fires on good input is a counter nobody will read twice.
func TestParseSessionReader_CleanTranscriptCountsNothing(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"uuid":"t1","sessionId":"s-1","type":"user","timestamp":"2026-05-26T14:30:00Z","message":{"role":"user","content":"a"}}`,
		`{"uuid":"t2","sessionId":"s-1","type":"user","timestamp":"2026-05-26T14:30:00.123456789Z","message":{"role":"user","content":"b"}}`,
		`{"uuid":"t3","sessionId":"s-1","type":"user","timestamp":"2026-05-26T14:30:00.000Z","message":{"role":"user","content":"c"}}`,
	}, "\n") + "\n"

	_, _, stats, err := ParseSessionReader(strings.NewReader(jsonl), "s-1.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	if stats.TurnsWithUnparsableTimestamp != 0 {
		t.Errorf("TurnsWithUnparsableTimestamp = %d on a clean transcript, want 0",
			stats.TurnsWithUnparsableTimestamp)
	}
	if stats.UnparsableTimestampSample != "" {
		t.Errorf("UnparsableTimestampSample = %q on a clean transcript, want empty",
			stats.UnparsableTimestampSample)
	}
}
