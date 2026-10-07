// ABOUTME: Tests that a session's started_at and ended_at are the extrema of its turn timestamps
// ABOUTME: Covers clock skew making the first and last turns in file order the wrong answers (issue #76)

package parser

import (
	"strings"
	"testing"
	"time"
)

// A transcript whose first and last lines are both skewed: turn2 is stamped
// before turn1 and turn3 is stamped before turn4, so neither end of the file
// holds the extremum. Measured on the author's archive, 6,597 adjacent turn
// pairs carry a timestamp earlier than the turn written before them — this is
// a shape the real data has, not a contrived one.
const skewedSessionJSONL = `{"uuid":"turn1","sessionId":"s1","type":"user","timestamp":"2026-02-02T20:05:00.000Z","message":{"role":"user","content":"first in the file"}}
{"uuid":"turn2","sessionId":"s1","type":"assistant","timestamp":"2026-02-02T20:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"earliest moment"}]}}
{"uuid":"turn3","sessionId":"s1","type":"user","timestamp":"2026-02-02T20:30:00.000Z","message":{"role":"user","content":"latest moment"}}
{"uuid":"turn4","sessionId":"s1","type":"assistant","timestamp":"2026-02-02T20:10:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"last in the file"}]}}`

// TestParseSessionReader_EndedAtIsTheLatestTimestamp is #76: ended_at was
// taken from the last turn in file order, which under clock skew is not the
// latest timestamp. db.MergeFrom uses ended_at to decide which copy of a
// duplicated session wins ("incoming replaces local only if it ended strictly
// later"), so a skewed final turn picks the wrong copy, and anything computing
// a duration from the pair can get a negative number.
func TestParseSessionReader_EndedAtIsTheLatestTimestamp(t *testing.T) {
	_, session, _, err := ParseSessionReader(strings.NewReader(skewedSessionJSONL), "/test/skewed.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}

	want := time.Date(2026, 2, 2, 20, 30, 0, 0, time.UTC)
	if !session.EndedAt.Equal(want) {
		t.Errorf("EndedAt = %s, want %s (the latest turn timestamp, not the last turn in file order)",
			session.EndedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The mirror defect #76 asked about: started_at came from the first turn in
// file order, so a skewed early turn made the session appear to start after
// activity it contains. On the author's archive 15 sessions have a started_at
// later than their own earliest turn — slightly more than carry the ended_at
// version of the defect.
func TestParseSessionReader_StartedAtIsTheEarliestTimestamp(t *testing.T) {
	_, session, _, err := ParseSessionReader(strings.NewReader(skewedSessionJSONL), "/test/skewed.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}

	want := time.Date(2026, 2, 2, 20, 0, 0, 0, time.UTC)
	if !session.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %s, want %s (the earliest turn timestamp, not the first turn in file order)",
			session.StartedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// The two extrema must bracket every turn the session holds. Stated as an
// invariant rather than as two more literal expectations, because it is the
// property the consumers actually depend on: stats and orient report date
// ranges from these columns, and a session that appears to end before activity
// it contains is what makes those reports wrong.
func TestParseSessionReader_BoundsBracketEveryTurn(t *testing.T) {
	turns, session, _, err := ParseSessionReader(strings.NewReader(skewedSessionJSONL), "/test/skewed.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	if len(turns) != 4 {
		t.Fatalf("parsed %d turns, want 4", len(turns))
	}
	for _, turn := range turns {
		if turn.Timestamp.Before(session.StartedAt) {
			t.Errorf("turn %s at %s is before the session's StartedAt %s",
				turn.ID, turn.Timestamp.Format(time.RFC3339), session.StartedAt.Format(time.RFC3339))
		}
		if turn.Timestamp.After(session.EndedAt) {
			t.Errorf("turn %s at %s is after the session's EndedAt %s",
				turn.ID, turn.Timestamp.Format(time.RFC3339), session.EndedAt.Format(time.RFC3339))
		}
	}
}

// Taking the extrema must not disturb the ordering. #29 established by
// measurement that file order reproduces the source .jsonl sequence 1,412
// times out of 1,413 while timestamp-first ordering manages only 1,336, so
// file order is the correct sequence and MAX is the correct extremum —
// different questions, and this holds them apart.
func TestParseSessionReader_SkewDoesNotReorderTurns(t *testing.T) {
	turns, _, _, err := ParseSessionReader(strings.NewReader(skewedSessionJSONL), "/test/skewed.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}

	wantIDs := []string{"turn1", "turn2", "turn3", "turn4"}
	for i, want := range wantIDs {
		if turns[i].ID != want {
			t.Errorf("turn at position %d is %s, want %s — file order was not preserved",
				i, turns[i].ID, want)
		}
		if turns[i].Ordinal != i {
			t.Errorf("turn %s has ordinal %d, want %d", turns[i].ID, turns[i].Ordinal, i)
		}
	}
}

// A single-turn session is the degenerate case both extrema have to agree on.
func TestParseSessionReader_SingleTurnBounds(t *testing.T) {
	input := `{"uuid":"only","sessionId":"s1","type":"user","timestamp":"2026-02-02T20:00:00.000Z","message":{"role":"user","content":"hi"}}`
	_, session, _, err := ParseSessionReader(strings.NewReader(input), "/test/one.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	want := time.Date(2026, 2, 2, 20, 0, 0, 0, time.UTC)
	if !session.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %s, want %s", session.StartedAt, want)
	}
	if !session.EndedAt.Equal(want) {
		t.Errorf("EndedAt = %s, want %s", session.EndedAt, want)
	}
}
