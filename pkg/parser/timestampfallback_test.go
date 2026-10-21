// ABOUTME: Tests that an unreadable transcript timestamp becomes the zero time and is kept out of the session's bounds.
// ABOUTME: Guards #165 — the old time.Now() fallback invented a plausible value that dragged the extrema db.MergeFrom dedupes on.

package parser

import (
	"strings"
	"testing"
	"time"
)

// A transcript with one unreadable timestamp between two readable ones. The
// readable pair brackets a five-minute window; the unreadable line sits in the
// middle of the file, which is where a real format drift would put it.
const unreadableMiddleJSONL = `{"uuid":"t1","sessionId":"s1","type":"user","timestamp":"2026-02-02T20:00:00.000Z","message":{"role":"user","content":"first"}}
{"uuid":"t2","sessionId":"s1","type":"assistant","timestamp":"2026/02/02 20:02:00","message":{"role":"assistant","content":[{"type":"text","text":"unreadable clock"}]}}
{"uuid":"t3","sessionId":"s1","type":"user","timestamp":"2026-02-02T20:05:00.000Z","message":{"role":"user","content":"last"}}`

// TestParseSessionReader_UnreadableTimestampIsNotTheSyncTime is #165's first
// half. The fallback used to be time.Now(), which is worse than being wrong: a
// present-day instant is indistinguishable from a recorded one on inspection,
// it answers every date predicate, and since #142 made timestamps sort
// lexicographically as instants it sorts to the newest end of the archive —
// the part people look at.
//
// The zero time is the opposite kind of wrong: impossible on its face, and the
// value codex and jeff already produce for the same failure, so one archive
// does not carry two different markers for "we could not read the clock".
func TestParseSessionReader_UnreadableTimestampIsNotTheSyncTime(t *testing.T) {
	before := time.Now()

	turns, _, stats, err := ParseSessionReader(strings.NewReader(unreadableMiddleJSONL), "/test/unreadable.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("parsed %d turns, want 3 — an unreadable clock must not drop the conversation", len(turns))
	}
	if stats.TurnsWithUnparsableTimestamp != 1 {
		t.Fatalf("TurnsWithUnparsableTimestamp = %d, want 1", stats.TurnsWithUnparsableTimestamp)
	}

	if !turns[1].Timestamp.IsZero() {
		t.Errorf("turn t2 timestamp = %s, want the zero time", turns[1].Timestamp.Format(time.RFC3339Nano))
	}
	// Stated as a range rather than only as IsZero, because the specific
	// failure was a value inside this window: a fallback taken at parse time.
	if !turns[1].Timestamp.Before(before) {
		t.Errorf("turn t2 timestamp = %s, which is at or after the moment the parse began — the sync time was substituted for a recorded one",
			turns[1].Timestamp.Format(time.RFC3339Nano))
	}
}

// TestParseSessionReader_UnreadableTimestampDoesNotMoveSessionBounds is #165's
// second and higher-stakes half, and the reason it is filed apart from #146.
//
// db.MergeFrom picks between two copies of one session by comparing ended_at,
// so a bound computed from an invented instant is a data-loss path and not a
// display bug: the time.Now() fallback dragged ended_at to the sync time, which
// is later than any real transcript, and the copy holding the unreadable line
// won a merge it should have lost. The zero value would drag started_at the
// other way, to year 1, for the same reason.
//
// Only turns whose time the transcript actually recorded may establish the
// extrema. A turn whose clock is unknown says nothing about when the session
// ran, so it contributes nothing.
func TestParseSessionReader_UnreadableTimestampDoesNotMoveSessionBounds(t *testing.T) {
	_, session, _, err := ParseSessionReader(strings.NewReader(unreadableMiddleJSONL), "/test/unreadable.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}

	wantStart := time.Date(2026, 2, 2, 20, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 2, 2, 20, 5, 0, 0, time.UTC)
	if !session.StartedAt.Equal(wantStart) {
		t.Errorf("StartedAt = %s, want %s — the unreadable turn moved the lower bound",
			session.StartedAt.Format(time.RFC3339), wantStart.Format(time.RFC3339))
	}
	if !session.EndedAt.Equal(wantEnd) {
		t.Errorf("EndedAt = %s, want %s — the unreadable turn moved the upper bound",
			session.EndedAt.Format(time.RFC3339), wantEnd.Format(time.RFC3339))
	}
}

// The unreadable line arriving first must not establish the bounds either. This
// is the ordering that the IsZero "not set yet" sentinel gets wrong on its own:
// the first turn seen sets both extrema unconditionally, so a leading zero time
// would be adopted as started_at and then never beaten.
func TestParseSessionReader_LeadingUnreadableTimestampDoesNotSetBounds(t *testing.T) {
	jsonl := `{"uuid":"t1","sessionId":"s1","type":"user","timestamp":"nonsense","message":{"role":"user","content":"first"}}
{"uuid":"t2","sessionId":"s1","type":"assistant","timestamp":"2026-02-02T20:02:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"readable"}]}}`

	_, session, _, err := ParseSessionReader(strings.NewReader(jsonl), "/test/leading.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}

	want := time.Date(2026, 2, 2, 20, 2, 0, 0, time.UTC)
	if !session.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %s, want %s", session.StartedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if !session.EndedAt.Equal(want) {
		t.Errorf("EndedAt = %s, want %s", session.EndedAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A session whose every timestamp is unreadable — the shape a wholesale format
// change by the producer would create — keeps all of its turns and reports no
// bounds at all. Both halves matter. Refusing the lines instead would turn a
// format drift into total content loss: the session would hold no turns, so it
// would get no id (that comes from turns[0]) and nothing would be archived but
// a counter. And a zero ended_at is the safe direction through MergeFrom, which
// reads a missing end time as the zero value precisely so a row with no end
// time never displaces one that has one.
func TestParseSessionReader_AllTimestampsUnreadableKeepsTurnsAndLeavesBoundsUnset(t *testing.T) {
	jsonl := `{"uuid":"t1","sessionId":"s1","type":"user","timestamp":"2026/02/02 20:00:00","message":{"role":"user","content":"first"}}
{"uuid":"t2","sessionId":"s1","type":"assistant","timestamp":"2026/02/02 20:02:00","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}`

	turns, session, stats, err := ParseSessionReader(strings.NewReader(jsonl), "/test/alldrift.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("parsed %d turns, want 2 — a producer changing its timestamp format must not empty the session", len(turns))
	}
	if session.ID != "s1" {
		t.Errorf("session ID = %q, want %q — the id comes from the first turn, so dropping the turns loses it", session.ID, "s1")
	}
	if session.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2 — the counter must agree with the turns actually returned", session.TurnCount)
	}
	if stats.TurnsWithUnparsableTimestamp != 2 {
		t.Errorf("TurnsWithUnparsableTimestamp = %d, want 2", stats.TurnsWithUnparsableTimestamp)
	}
	if !session.StartedAt.IsZero() || !session.EndedAt.IsZero() {
		t.Errorf("bounds = (%s, %s), want both unset — no turn recorded a time to derive them from",
			session.StartedAt.Format(time.RFC3339), session.EndedAt.Format(time.RFC3339))
	}
}

// The bracketing invariant from TestParseSessionReader_BoundsBracketEveryTurn,
// restated over the turns it can hold. A turn with no readable clock cannot be
// bracketed by anything; every turn that has one must still be.
func TestParseSessionReader_BoundsBracketEveryTurnWithAReadableClock(t *testing.T) {
	turns, session, _, err := ParseSessionReader(strings.NewReader(unreadableMiddleJSONL), "/test/unreadable.jsonl")
	if err != nil {
		t.Fatalf("ParseSessionReader: %v", err)
	}
	for _, turn := range turns {
		if turn.Timestamp.IsZero() {
			continue
		}
		if turn.Timestamp.Before(session.StartedAt) || turn.Timestamp.After(session.EndedAt) {
			t.Errorf("turn %s at %s falls outside the session's bounds (%s, %s)",
				turn.ID, turn.Timestamp.Format(time.RFC3339),
				session.StartedAt.Format(time.RFC3339), session.EndedAt.Format(time.RFC3339))
		}
	}
}
